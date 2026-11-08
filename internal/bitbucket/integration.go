package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scopes"`
}

func (c *Client) Exchange(ctx context.Context, id, secret, code, redirect string) (Tokens, error) {
	return c.token(ctx, id, secret, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}})
}
func Scopes(s string) bool {
	seen := map[string]bool{}
	for _, scope := range strings.Fields(s) {
		if scope != "repository" && scope != "pullrequest" && scope != "pullrequest:write" && scope != "webhook" {
			return false
		}
		seen[scope] = true
	}
	return seen["repository"] && seen["pullrequest:write"] && seen["webhook"]
}

// Refresh tokens remain platform-only. Source helpers never receive OAuth tokens.
func (c *Client) Refresh(ctx context.Context, id, secret, refresh string) (Tokens, error) {
	return c.token(ctx, id, secret, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}})
}
func (c *Client) token(ctx context.Context, id, secret string, form url.Values) (Tokens, error) {
	var t Tokens
	if id == "" || secret == "" {
		return t, ErrRemote
	}
	r, e := http.NewRequestWithContext(ctx, "POST", "https://bitbucket.org/site/oauth2/access_token", strings.NewReader(form.Encode()))
	if e != nil {
		return t, ErrRemote
	}
	r.SetBasicAuth(id, secret)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, e := c.HTTP.Do(r)
	if e != nil {
		return t, ErrRemote
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(b) > 65536 || resp.StatusCode != 200 || json.Unmarshal(b, &t) != nil || t.AccessToken == "" || t.RefreshToken == "" || t.ExpiresIn < 1 {
		return Tokens{}, ErrRemote
	}
	return t, nil
}
func (c *Client) Comment(ctx context.Context, repo string, pr int, commentID, token, text string) (string, error) {
	p, e := repoPath(repo)
	if e != nil || pr < 1 || len(text) > 8192 {
		return "", ErrRemote
	}
	method := "POST"
	p += "/pullrequests/" + fmtInt(pr) + "/comments"
	if commentID != "" {
		for _, c := range commentID {
			if c < '0' || c > '9' {
				return "", ErrRemote
			}
		}
		method = "PUT"
		p += "/" + commentID
	}
	b, _ := json.Marshal(map[string]any{"content": map[string]string{"raw": text}})
	var out struct {
		ID int `json:"id"`
	}
	if e = c.request(ctx, method, p, token, bytes.NewReader(b), &out); e != nil {
		return "", e
	}
	if out.ID < 1 {
		return "", ErrRemote
	}
	return fmtInt(out.ID), nil
}
func (c *Client) FindComment(ctx context.Context, repo string, pr int, token, marker string) (string, error) {
	p, e := repoPath(repo)
	if e != nil || pr < 1 || marker == "" {
		return "", ErrRemote
	}
	base := p + "/pullrequests/" + fmtInt(pr) + "/comments"
	next := base + "?pagelen=100"
	for page := 0; page < 20; page++ {
		var body struct {
			Values []struct {
				ID      int `json:"id"`
				Content struct {
					Raw string `json:"raw"`
				} `json:"content"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if e = c.request(ctx, "GET", next, token, nil, &body); e != nil {
			return "", e
		}
		for _, v := range body.Values {
			if v.ID > 0 && strings.Contains(v.Content.Raw, marker) {
				return fmtInt(v.ID), nil
			}
		}
		if body.Next == "" {
			return "", nil
		}
		u, e := url.Parse(body.Next)
		if e != nil || u.Scheme != "https" || u.Host != "api.bitbucket.org" || u.User != nil || u.Fragment != "" || u.Path != "/2.0"+base {
			return "", ErrRemote
		}
		next = base + "?" + u.RawQuery
	}
	return "", ErrRemote
}
