package bitbucket

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProviderFailurePreservesRetryClassification(t *testing.T) {
	for _, status := range []int{403, 429, 503} {
		for _, operation := range []string{"branch", "branch pointer", "pull request", "manifest", "create comment", "update comment"} {
			t.Run(operation+"/"+fmtInt(status), func(t *testing.T) {
				c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
					if operation == "branch pointer" && !strings.Contains(r.URL.Path, "/refs/") {
						return response(r, `{"uuid":"{fictional}"}`), nil
					}
					v := response(r, `{"error":"synthetic"}`)
					v.StatusCode = status
					v.Header.Set("Retry-After", "60")
					return v, nil
				})})
				var e error
				if strings.HasSuffix(operation, "comment") {
					id := ""
					if operation == "update comment" {
						id = "42"
					}
					_, e = c.Comment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, id, "", "synthetic feedback")
				} else if operation == "manifest" {
					_, e = c.Manifest(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", strings.Repeat("a", 40), "")
				} else {
					pr := 0
					if operation == "pull request" {
						pr = 3
					}
					_, e = c.Read(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", "main", pr, "")
				}
				var remote *RemoteError
				if !errors.As(e, &remote) || remote.Status != status || remote.RetryAfter != time.Minute || !errors.Is(e, ErrRemote) {
					t.Fatalf("lost provider retry classification: %T %v", e, e)
				}
				if strings.Contains(e.Error(), "synthetic") {
					t.Fatal("provider response leaked")
				}
			})
		}
	}
}

func TestRetryAfterHandlesSecondsAndHTTPDates(t *testing.T) {
	for _, tc := range []struct {
		header           string
		minimum, maximum time.Duration
	}{
		{"99999999", 24 * time.Hour, 24 * time.Hour},
		{time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat), 115 * time.Second, 2 * time.Minute},
		{time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat), 0, 0},
		{"-10", 0, 0}, {"invalid", 0, 0},
	} {
		r := &http.Response{StatusCode: 429, Header: make(http.Header)}
		r.Header.Set("Retry-After", tc.header)
		e := remoteError(r)
		var remote *RemoteError
		if !errors.As(e, &remote) || remote.RetryAfter < tc.minimum || remote.RetryAfter > tc.maximum {
			t.Fatal("unbounded or invalid retry delay", remote)
		}
	}
}

func TestCommentPaginationCannotMoveCredentialOrigin(t *testing.T) {
	marker := "<!-- fictional-marker -->"
	for _, scenario := range []string{"found on next page", "foreign host", "foreign repository", "userinfo", "fragment", "endless pages", "bad JSON", "transport error"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Host != "api.bitbucket.org" || r.Header.Get("Authorization") != "Bearer fictional" {
					t.Fatal("credential origin changed")
				}
				if scenario == "transport error" {
					return nil, errors.New("synthetic transport failure")
				}
				if scenario == "bad JSON" {
					return response(r, `{`), nil
				}
				next := "https://api.bitbucket.org/2.0/repositories/%7B%7D/%7Bfictional%7D/pullrequests/3/comments?page=2"
				switch scenario {
				case "foreign host":
					next = "https://attacker.example/comments"
				case "foreign repository":
					next = "https://api.bitbucket.org/2.0/repositories/other/repo/pullrequests/3/comments"
				case "userinfo":
					next = "https://user@api.bitbucket.org/2.0/repositories/%7B%7D/%7Bfictional%7D/pullrequests/3/comments"
				case "fragment":
					next += "#fragment"
				}
				if calls == 2 && scenario == "found on next page" {
					return response(r, `{"values":[{"id":0,"content":{"raw":"`+marker+`"}},{"id":42,"content":{"raw":"`+marker+`"}}]}`), nil
				}
				return response(r, `{"values":[],"next":"`+next+`"}`), nil
			})})
			id, e := c.FindComment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, "fictional", marker)
			if scenario == "found on next page" {
				if e != nil || id != "42" || calls != 2 {
					t.Fatal(id, calls, e)
				}
			} else if e == nil {
				t.Fatal("unsafe pagination accepted")
			}
			if scenario == "endless pages" && calls != 20 {
				t.Fatal("pagination bound lost", calls)
			}
		})
	}
}

func TestTokenResponsesAndScopesRejectUnsupportedFacts(t *testing.T) {
	for _, body := range []string{`{`, `{}`, `{"access_token":"fictional","refresh_token":"fictional","expires_in":0}`, strings.Repeat("x", 65537)} {
		c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "bitbucket.org" || r.URL.Path != "/site/oauth2/access_token" {
				t.Fatal("token request escaped OAuth endpoint")
			}
			return response(r, body), nil
		})})
		if _, e := c.Refresh(context.Background(), "fictional-id", "fictional-secret", "fictional-refresh"); e == nil {
			t.Fatal("incomplete token response accepted")
		}
	}
	for _, value := range []string{"repository webhook", "repository pullrequest:write webhook account:write", ""} {
		if Scopes(value) {
			t.Fatal("incorrect OAuth scope accepted")
		}
	}
	if !Scopes("repository pullrequest pullrequest:write webhook") {
		t.Fatal("required scope set refused")
	}
}

func TestInvalidCommentIdentityNeverSendsARequest(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		// A successful response prevents response validation masking bad inputs.
		return response(r, `{"id":42}`), nil
	})})
	for _, id := range []string{"../other", "42?query"} {
		if _, e := c.Comment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, id, "", "fixture"); e == nil {
			t.Fatal("invalid comment identity accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid comment identity reached provider", calls)
	}
	if id, e := c.Comment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, "", "", "fixture"); e != nil || id != "42" || calls != 1 {
		t.Fatal("new comment positive control failed", id, e)
	}
}

func TestCommentsRejectMissingResponseIdentity(t *testing.T) {
	c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) { return response(r, `{"id":0}`), nil })})
	if _, e := c.Comment(context.Background(), "https://bitbucket.org/fictional/repo.git", "{fictional}", 3, "", "", "fixture"); e == nil {
		t.Fatal("zero comment identity accepted")
	}
}

func TestOAuthRejectsMissingConsumerCredentialsBeforeSendingRequest(t *testing.T) {
	calls := 0
	c := New(&http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return response(r, `{"access_token":"fictional","refresh_token":"fictional","expires_in":3600,"scopes":"repository pullrequest:write webhook"}`), nil
	})})
	for _, pair := range [][2]string{{"", "fictional"}, {"fictional", ""}, {"", ""}} {
		if _, e := c.Exchange(context.Background(), pair[0], pair[1], "fixture", "https://localhost/callback"); e == nil {
			t.Fatal("missing consumer credential accepted")
		}
	}
	if calls != 0 {
		t.Fatal("incomplete consumer credentials reached provider")
	}
	if _, e := c.Exchange(context.Background(), "fictional", "fictional", "fixture", "https://localhost/callback"); e != nil || calls != 1 {
		t.Fatal("valid OAuth positive control failed", e)
	}
}
