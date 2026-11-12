// Package bitbucket provides bounded API operations. No request executes native
// infrastructure commands; source facts always come from authenticated reads.
package bitbucket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

var ErrRemote = errors.New("source service unavailable or returned unsupported facts")

// ErrContextChanged carries validated authoritative facts for invalidation only.
// Callers must still reject work/approval; a non-nil error never authorizes a
// fork, a replacement repository, or a changed destination repository.
var ErrContextChanged = errors.New("source repository context changed")

type RemoteError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *RemoteError) Error() string { return "source service request failed" }
func (e *RemoteError) Unwrap() error { return ErrRemote }
func remoteError(r *http.Response) error {
	delay := time.Duration(0)
	if n, e := strconv.Atoi(r.Header.Get("Retry-After")); e == nil && n > 0 {
		delay = time.Duration(min(n, 86400)) * time.Second
	} else if t, e := http.ParseTime(r.Header.Get("Retry-After")); e == nil {
		delay = max(0, time.Until(t))
	}
	return &RemoteError{r.StatusCode, delay}
}

type Client struct{ HTTP *http.Client }

func New(c *http.Client) *Client {
	if c == nil {
		c = &http.Client{}
	}
	clone := *c
	clone.Timeout = 10 * time.Second
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{&clone}
}

type Facts struct {
	RepositoryID       string `json:"repository_id"`
	SourceRepositoryID string `json:"source_repository_id"`
	Commit             string `json:"commit"`
	Branch             string `json:"branch"`
	Destination        string `json:"destination"`
	State              string `json:"state"`
	InventoryRevision  int64  `json:"inventory_revision"`
}

func (c *Client) request(ctx context.Context, method, path, token string, body io.Reader, out any) error {
	if !strings.HasPrefix(path, "/repositories/") || strings.ContainsAny(path, "\r\n") {
		return ErrRemote
	}
	req, e := http.NewRequestWithContext(ctx, method, "https://api.bitbucket.org/2.0"+path, body)
	if e != nil {
		return ErrRemote
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.HTTP.Do(req)
	if e != nil {
		return ErrRemote
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return remoteError(resp)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if e != nil || len(b) > 1048576 {
		return ErrRemote
	}
	if out == nil {
		return nil
	}
	if json.Unmarshal(b, out) != nil {
		return ErrRemote
	}
	return nil
}
func repoPath(repo string) (string, error) {
	if !source.Repository(repo) {
		return "", ErrRemote
	}
	u, _ := url.Parse(repo)
	return "/repositories/" + strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git"), nil
}

// API requests address the immutable provider UUID, not a reusable slug. The
// registered URL is still validated separately and checked against live facts.
func repositoryPath(repo, externalID string) (string, error) {
	if _, e := repoPath(repo); e != nil || !regexp.MustCompile(`^\{[A-Za-z0-9-]{1,254}\}$`).MatchString(externalID) {
		return "", ErrRemote
	}
	return "/repositories/%7B%7D/" + url.PathEscape(externalID), nil
}

func (c *Client) Read(ctx context.Context, repo, externalID, branch string, pr int, token string) (Facts, error) {
	var f Facts
	p, e := repoPath(repo)
	if e != nil || !catalog.Branch(branch) {
		return f, ErrRemote
	}
	if pr == 0 {
		var identity struct {
			UUID string `json:"uuid"`
		}
		if e = c.request(ctx, "GET", p, token, nil, &identity); e != nil {
			return f, e
		}
		if identity.UUID == "" || len(identity.UUID) > 256 {
			return f, ErrRemote
		}
		if identity.UUID != externalID {
			return Facts{RepositoryID: identity.UUID}, ErrContextChanged
		}
		p, e = repositoryPath(repo, externalID)
		if e != nil {
			return f, e
		}
		var b struct {
			Name   string `json:"name"`
			Target struct {
				Hash string `json:"hash"`
			} `json:"target"`
		}
		e = c.request(ctx, "GET", p+"/refs/branches/"+url.PathEscape(branch), token, nil, &b)
		if e != nil {
			return f, e
		}
		if b.Name != branch || !source.Commit(b.Target.Hash) {
			return f, ErrRemote
		}
		f = Facts{identity.UUID, identity.UUID, b.Target.Hash, branch, branch, "OPEN", 0}
		return f, nil
	}
	if pr < 1 {
		return f, ErrRemote
	}
	var canonical struct {
		UUID string `json:"uuid"`
	}
	if e = c.request(ctx, "GET", p, token, nil, &canonical); e != nil {
		return f, e
	}
	if canonical.UUID == "" || len(canonical.UUID) > 256 {
		return f, ErrRemote
	}
	if canonical.UUID != externalID {
		return Facts{RepositoryID: canonical.UUID}, ErrContextChanged
	}
	p, e = repositoryPath(repo, externalID)
	if e != nil {
		return f, e
	}

	var b struct {
		State  string `json:"state"`
		Source struct {
			Repository struct {
				UUID string `json:"uuid"`
			} `json:"repository"`
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
			Branch struct {
				Name string `json:"name"`
			} `json:"branch"`
		} `json:"source"`
		Destination struct {
			Repository struct {
				UUID string `json:"uuid"`
			} `json:"repository"`
			Branch struct {
				Name string `json:"name"`
			} `json:"branch"`
		} `json:"destination"`
	}
	e = c.request(ctx, "GET", p+"/pullrequests/"+fmtInt(pr), token, nil, &b)
	if e != nil {
		return f, e
	}
	if b.Source.Repository.UUID == "" || len(b.Source.Repository.UUID) > 256 || b.Destination.Repository.UUID == "" || len(b.Destination.Repository.UUID) > 256 || !catalog.Branch(b.Destination.Branch.Name) || !source.Commit(b.Source.Commit.Hash) || !catalog.Branch(b.Source.Branch.Name) || (b.State != "OPEN" && b.State != "MERGED" && b.State != "DECLINED" && b.State != "SUPERSEDED") {
		return f, ErrRemote
	}
	// Eligibility is checked only after these authenticated facts invalidate any
	// previous PR authority, including when its destination no longer matches.
	f = Facts{b.Destination.Repository.UUID, b.Source.Repository.UUID, b.Source.Commit.Hash, b.Source.Branch.Name, b.Destination.Branch.Name, b.State, 0}
	if f.RepositoryID != externalID || f.SourceRepositoryID != externalID {
		return f, ErrContextChanged
	}
	return f, nil
}
func fmtInt(n int) string { b, _ := json.Marshal(n); return string(b) }
func (c *Client) Manifest(ctx context.Context, repo, externalID, commit, token string) ([]byte, error) {
	p, e := repositoryPath(repo, externalID)
	if e != nil || !source.Commit(commit) {
		return nil, ErrRemote
	}
	r, e := http.NewRequestWithContext(ctx, "GET", "https://api.bitbucket.org/2.0"+p+"/src/"+commit+"/cosmoformer.yaml", nil)
	if e != nil {
		return nil, ErrRemote
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := c.HTTP.Do(r)
	if e != nil {
		return nil, ErrRemote
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, remoteError(resp)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if e != nil || len(b) > 1048576 {
		return nil, ErrRemote
	}
	return b, nil
}
func Signature(body []byte, header string, key []byte) bool {
	if len(body) > 1048576 || len(key) < 32 || !regexp.MustCompile(`^sha256=[0-9a-f]{64}$`).MatchString(header) {
		return false
	}
	got, e := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if e != nil {
		return false
	}
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return hmac.Equal(got, h.Sum(nil))
}

// ChangedPaths reads only an exact verified range. Any incomplete or unsupported
// response is conservative; diff metadata never authorizes a repository/stack.
func (c *Client) ChangedPaths(ctx context.Context, repo, externalID, base, head, token string) ([]string, bool) {
	p, e := repositoryPath(repo, externalID)
	if e != nil || !source.Commit(base) || !source.Commit(head) {
		return nil, false
	}
	// Equality can follow an inventory import whose path decision was lost. Treat
	// it conservatively; durable per-facts selection prevents unchanged-sweep drift.
	if base == head {
		return nil, false
	}
	endpoint := p + "/diffstat/" + base + ".." + head
	next := endpoint + "?pagelen=100"
	paths := []string{}
	entries := 0
	for page := 0; page < 10; page++ {
		var b struct {
			Values *[]struct {
				Old, New *struct {
					Path string `json:"path"`
				}
			} `json:"values"`
			Next      string `json:"next"`
			Size      *int   `json:"size"`
			Truncated bool   `json:"truncated"`
		}
		if c.request(ctx, "GET", next, token, nil, &b) != nil || b.Values == nil || b.Truncated || len(*b.Values) > 100 {
			return nil, false
		}
		entries += len(*b.Values)
		for _, entry := range *b.Values {
			if entry.Old == nil && entry.New == nil {
				return nil, false
			}
			for _, side := range []*struct {
				Path string `json:"path"`
			}{entry.Old, entry.New} {
				if side == nil {
					continue
				}
				if !catalog.RepoPath(side.Path) {
					return nil, false
				}
				paths = append(paths, side.Path)
			}
		}
		if b.Next == "" {
			if b.Size != nil && *b.Size != entries {
				return nil, false
			}
			return paths, true
		}
		u, e := url.Parse(b.Next)
		if e != nil || u.Scheme != "https" || u.Host != "api.bitbucket.org" || u.User != nil || u.Fragment != "" || u.EscapedPath() != "/2.0"+endpoint {
			return nil, false
		}
		next = endpoint + "?" + u.RawQuery
	}
	return nil, false
}
