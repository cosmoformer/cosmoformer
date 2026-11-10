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
	if _, e := c.Manifest(context.Background(), "https://bitbucket.org/fictional/repo.git", strings.Repeat("a", 40), "fictional"); e == nil {
		t.Fatal("redirect followed")
	}
	c.HTTP.Transport = transport(func(r *http.Request) (*http.Response, error) { return response(r, strings.Repeat("a", 1048577)), nil })
	if _, e := c.Manifest(context.Background(), "https://bitbucket.org/fictional/repo.git", strings.Repeat("a", 40), ""); e == nil {
		t.Fatal("oversized manifest accepted")
	}
}
func TestPRReadPreservesAuthenticatedDestinationChanges(t *testing.T) {
	for _, destination := range []string{"main", "release", "../invalid"} {
		t.Run(destination, func(t *testing.T) {
			c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
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
			if e != ErrContextChanged || facts.Commit != strings.Repeat("a", 40) || pr == 0 && facts.RepositoryID != "{replacement}" || pr == 1 && facts.SourceRepositoryID != "{fork}" {
				t.Fatal("unsupported context was permitted or authoritative facts lost", facts, e)
			}
		})
	}
}
