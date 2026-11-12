package bitbucket

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func TestRepositoryIdentityAndExactBranch(t *testing.T) {
	commit := strings.Repeat("a", 40)
	c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.bitbucket.org" || r.Header.Get("Authorization") != "Bearer fictional-token" {
			t.Fatal("credential origin not constrained")
		}
		if strings.Contains(r.URL.Path, "/refs/") {
			return response(r, `{"name":"main","target":{"hash":"`+commit+`"}}`), nil
		}
		return response(r, `{"uuid":"{fictional}"}`), nil
	})})
	f, e := c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", "main", 0, "fictional-token")
	if e != nil || f.Commit != commit {
		t.Fatal(f, e)
	}
	if _, e = c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{other}", "main", 0, "fictional-token"); e == nil {
		t.Fatal("mutable slug changed repository identity")
	}
}
func TestForkRefusalRedirectAndBounds(t *testing.T) {
	c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		return response(r, `{"state":"OPEN","source":{"repository":{"uuid":"{fork}"},"commit":{"hash":"`+strings.Repeat("a", 40)+`"},"branch":{"name":"change"}},"destination":{"repository":{"uuid":"{main}"},"branch":{"name":"main"}}}`), nil
	})})
	if _, e := c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{main}", "main", 1, ""); e == nil {
		t.Fatal("fork permitted")
	}
	c.HTTP.Transport = transport(func(r *http.Request) (*http.Response, error) {
		v := response(r, "")
		v.StatusCode = 302
		v.Header.Set("Location", "https://attacker.example")
		return v, nil
	})
	if _, e := c.Manifest(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", strings.Repeat("a", 40), "fictional"); e == nil {
		t.Fatal("redirect followed")
	}
	c.HTTP.Transport = transport(func(r *http.Request) (*http.Response, error) { return response(r, strings.Repeat("a", 1048577)), nil })
	if _, e := c.Manifest(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", strings.Repeat("a", 40), ""); e == nil {
		t.Fatal("oversized manifest accepted")
	}
}
func TestPRReadPreservesAuthenticatedDestinationChanges(t *testing.T) {
	for _, destination := range []string{"main", "release", "../invalid"} {
		t.Run(destination, func(t *testing.T) {
			c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if !strings.Contains(r.URL.Path, "/pullrequests/") {
					return response(r, `{"uuid":"{fictional}"}`), nil
				}
				return response(r, `{"state":"OPEN","source":{"repository":{"uuid":"{fictional}"},"commit":{"hash":"`+strings.Repeat("a", 40)+`"},"branch":{"name":"change"}},"destination":{"repository":{"uuid":"{fictional}"},"branch":{"name":"`+destination+`"}}}`), nil
			})})
			facts, err := c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", "main", 3, "")
			if destination == "../invalid" {
				if err == nil {
					t.Fatal("invalid destination accepted")
				}
				return
			}
			if err != nil || facts.Destination != destination {
				t.Fatal("authenticated destination was discarded or rewritten", facts, err)
			}
		})
	}
}
func TestSignatureUsesOriginalBytes(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	body := []byte(`{"repository":{"uuid":"{fictional}"}}`)
	h := hmac.New(sha256.New, key)
	h.Write(body)
	header := "sha256=" + hex.EncodeToString(h.Sum(nil))
	if !Signature(body, header, key) {
		t.Fatal("valid signature rejected")
	}
	if Signature(append(body, ' '), header, key) || Signature(body, strings.Replace(header, "sha256", "sha1", 1), key) || Signature(body, strings.ToUpper(header), key) {
		t.Fatal("invalid signature accepted")
	}
}

func TestChangedRepositoryFactsAreReturnedOnlyWithDenial(t *testing.T) {
	for _, pr := range []int{0, 1} {
		t.Run(fmtInt(pr), func(t *testing.T) {
			c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "pullrequests") {
					return response(r, `{"state":"OPEN","source":{"repository":{"uuid":"{fork}"},"commit":{"hash":"`+strings.Repeat("a", 40)+`"},"branch":{"name":"change"}},"destination":{"repository":{"uuid":"{main}"},"branch":{"name":"main"}}}`), nil
				}
				if strings.Contains(r.URL.Path, "refs/branches") {
					return response(r, `{"name":"main","target":{"hash":"`+strings.Repeat("a", 40)+`"}}`), nil
				}
				return response(r, `{"uuid":"{replacement}"}`), nil
			})})
			facts, e := c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{main}", "main", pr, "")
			if e != ErrContextChanged || facts.RepositoryID != "{replacement}" || facts.Commit != "" {
				t.Fatal("unsupported context was permitted or authoritative facts lost", facts, e)
			}
		})
	}
}

func TestChangedPathsRequireCompletePinnedBoundedResponses(t *testing.T) {
	for _, scenario := range []string{"rename", "pages", "foreign next", "missing values", "invalid path", "too many pages", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				calls++
				if !strings.Contains(r.URL.Path, "/diffstat/"+strings.Repeat("a", 40)+".."+strings.Repeat("b", 40)) {
					t.Error("floating diff range")
				}
				b := `{"values":[{"old":{"path":"old.tf"},"new":{"path":"new.tf"}}]}`
				switch scenario {
				case "pages":
					if calls == 1 {
						b = `{"values":[],"next":"https://api.bitbucket.org` + r.URL.Path + `?page=2"}`
					}
				case "foreign next":
					b = `{"values":[],"next":"https://attacker.invalid/credential-trap"}`
				case "missing values":
					b = `{}`
				case "invalid path":
					b = `{"values":[{"new":{"path":"../escape"}}]}`
				case "too many pages":
					b = `{"values":[],"next":"https://api.bitbucket.org` + r.URL.Path + `?page=2"}`
				case "unavailable":
					res := response(r, `{}`)
					res.StatusCode = 429
					return res, nil
				}
				return response(r, b), nil
			})})
			paths, complete := c.ChangedPaths(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", strings.Repeat("a", 40), strings.Repeat("b", 40), "synthetic")
			want := scenario == "rename" || scenario == "pages"
			if complete != want || want && strings.Join(paths, ",") != "old.tf,new.tf" {
				t.Fatal(paths, complete)
			}
			if calls > 10 {
				t.Fatal("unbounded diff pagination")
			}
		})
	}
}

func TestCommentRecoveryRefusesIncompleteListings(t *testing.T) {
	for _, body := range []string{`{}`, `{"values":null}`, `{"values":[],"size":1}`, `{"values":[],"truncated":true}`} {
		c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) { return response(r, body), nil })})
		if _, e := c.FindComment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, "synthetic", "<!-- marker -->"); e == nil {
			t.Fatal("incomplete listing authorized a fresh create", body)
		}
	}
}

func TestCommentRecoveryRequiresUniqueMatchAcrossCompleteListing(t *testing.T) {
	for _, scenario := range []string{"one page", "later page", "incomplete later page"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				calls++
				raw := `{"values":[{"id":17,"content":{"raw":"<!-- random-nonce -->"}},{"id":51,"content":{"raw":"<!-- random-nonce -->"}}]}`
				if scenario != "one page" && calls == 1 {
					raw = `{"values":[{"id":17,"content":{"raw":"<!-- random-nonce -->"}}],"next":"https://api.bitbucket.org` + r.URL.EscapedPath() + `?page=2"}`
				}
				if calls == 2 && scenario == "incomplete later page" {
					raw = `{}`
				}
				return response(r, raw), nil
			})})
			id, e := c.FindComment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, "synthetic", "<!-- random-nonce -->")
			if e == nil || id != "" {
				t.Fatal("ambiguous or incomplete listing adopted an identity", id, e)
			}
			if scenario != "one page" && calls != 2 {
				t.Fatal("first match prematurely ended lookup", calls)
			}
		})
	}
}

func TestRepositoryAPISelectorsRemainPinnedDuringSlugReplacement(t *testing.T) {
	const external = "{11111111-1111-4111-8111-111111111111}"
	expected := "/repositories/%7B%7D/%7B11111111-1111-4111-8111-111111111111%7D"
	c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		// Only the initial canonical identity read may address the registered slug.
		// All source and feedback requests remain bound to the immutable UUID.
		if r.URL.Path == "/2.0/repositories/fictional/repo" {
			return response(r, `{"uuid":"`+external+`"}`), nil
		}
		if !strings.HasPrefix(r.URL.EscapedPath(), "/2.0"+expected) {
			t.Error("mutable selector", r.URL.EscapedPath())
		}
		if strings.Contains(r.URL.Path, "/refs/") {
			return response(r, `{"name":"main","target":{"hash":"`+strings.Repeat("a", 40)+`"}}`), nil
		}
		if strings.Contains(r.URL.Path, "/comments") {
			if r.Method == "GET" {
				return response(r, `{"values":[]}`), nil
			}
			return response(r, `{"id":17}`), nil
		}
		if strings.Contains(r.URL.Path, "/diffstat/") {
			return response(r, `{"values":[]}`), nil
		}
		return response(r, `version: 1`), nil
	})})
	ctx := context.Background()
	repo := "https://bitbucket.org/fictional/repo.git"
	if _, e := c.Read(ctx, repo, external, "main", 0, "synthetic"); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Manifest(ctx, repo, external, strings.Repeat("a", 40), "synthetic"); e != nil {
		t.Fatal(e)
	}
	if _, ok := c.ChangedPaths(ctx, repo, external, strings.Repeat("a", 40), strings.Repeat("b", 40), "synthetic"); !ok {
		t.Fatal("bound diff failed")
	}
	if _, e := c.FindComment(ctx, repo, external, 3, "synthetic", "<!-- nonce -->"); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Comment(ctx, repo, external, 3, "", "synthetic", "bounded"); e != nil {
		t.Fatal(e)
	}
}

func TestRepositoryIdentityChangeDoesNotWaitForAnAccessibleOldHead(t *testing.T) {
	for _, pr := range []int{0, 3} {
		calls := 0
		c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return response(r, `{"uuid":"{replacement}"}`), nil
			}
			res := response(r, `{}`)
			res.StatusCode = 404
			return res, nil
		})})
		facts, e := c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{original}", "main", pr, "synthetic")
		if e != ErrContextChanged || facts.RepositoryID != "{replacement}" || facts.Commit != "" || calls != 1 {
			t.Fatal("known identity change was lost or unknown source invented", facts, e, calls)
		}
	}
}
