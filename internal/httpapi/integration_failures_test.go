//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

func TestOAuthRejectsUnconfiguredConsumersAndExcessScopes(t *testing.T) {
	f := newSourceFixture(t)
	base := "/api/v1/organizations/" + f.Org + "/integrations/" + f.Integration
	f.browser(t, "POST", base+"/connect", map[string]any{}, 409)
	f.configure(t)
	f.browser(t, "POST", base+"/credentials", map[string]any{"expected_revision": 1, "client_id": "fictional", "client_secret": "fictional-secret", "webhook_secret": strings.Repeat("w", 32), "hook_uuid": "fixture"}, 409)
	f.browser(t, "GET", "/oauth/bitbucket/callback?state=invalid&code=fixture", nil, 422)
	link := f.browser(t, "POST", base+"/connect", map[string]any{}, 200)
	var data struct {
		URL string `json:"authorization_url"`
	}
	if e := json.Unmarshal(link.Body.Bytes(), &data); e != nil {
		t.Fatal(e)
	}
	parsed, e := url.Parse(data.URL)
	if e != nil {
		t.Fatal(e)
	}
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "bitbucket.org" || r.URL.Path != "/site/oauth2/access_token" {
			t.Error("OAuth request left fixed consumer endpoint")
		}
		body := `{"access_token":"fictional","refresh_token":"fictional","expires_in":3600,"scopes":"repository pullrequest:write webhook account:write"}`
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	callback := "/oauth/bitbucket/callback?state=" + parsed.Query().Get("state") + "&code=fixture"
	f.browser(t, "GET", callback, nil, 409)
	f.browser(t, "GET", callback, nil, 403)
}

func TestFailedRefreshRequiresReconnectAndNeverReplaysRefreshToken(t *testing.T) {
	f := newSourceFixture(t)
	f.configure(t)
	f.connect(t)
	ctx := context.Background()
	var old integrationRecord
	if e := f.Server.serviceTx(ctx, f.Org, func(tx pgx.Tx) error {
		var e error
		old, e = f.Server.loadIntegration(ctx, tx, f.Org, f.Integration)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	old.Credentials.ExpiresAt = time.Now().Add(-time.Minute)
	sealed, e := f.Server.sealRevision(ctx, f.Org, f.Integration, "integration", old.Semantic, jsonBytes(old.Credentials))
	if e != nil {
		t.Fatal(e)
	}
	f.exec(t, `UPDATE cosmoformer.integrations SET encrypted_credentials=$2 WHERE id=$1`, f.Integration, sealed)
	calls := 0
	f.Server.Bitbucket.HTTP.Transport = fixtureTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 401, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
	})
	for range 2 {
		if _, e := f.Server.apiToken(ctx, f.Org, f.Integration); e == nil {
			t.Fatal("failed refresh returned a token")
		}
	}
	var reconnect, claimed bool
	if e := f.DB.Owner.QueryRow(ctx, `SELECT reconnect_required,refresh_claim IS NOT NULL FROM cosmoformer.integrations WHERE id=$1`, f.Integration).Scan(&reconnect, &claimed); e != nil || !reconnect || !claimed || calls != 1 {
		t.Fatal("failed refresh was replayed or lost durable uncertainty", reconnect, claimed, calls, e)
	}
}

func TestManagedCredentialEndpointsRefuseMissingEncryptionKey(t *testing.T) {
	f := newSourceFixture(t)
	base := "/api/v1/organizations/" + f.Org
	f.configure(t)
	f.Server.Cipher = nil
	if _, e := f.Server.apiToken(context.Background(), f.Org, f.Integration); e == nil {
		t.Fatal("encrypted integration opened without its key")
	}
	f.Handler, _ = NewWithOptions(f.DB.App, f.Server.Origin, Options{Clock: elapsedTest{}, Bitbucket: bitbucket.New(&http.Client{Transport: f.Remote})})
	f.browser(t, "POST", base+"/secrets", map[string]any{"name": "Fixture", "value": "fictional-private-value"}, 503)
	f.browser(t, "POST", base+"/repositories/"+f.Repo+"/source-credentials", map[string]any{"expected_revision": 0, "username": "fixture", "token": "fictional-token"}, 503)
	f.browser(t, "PATCH", base+"/secrets/"+runs.ID(), map[string]any{"expected_revision": 1, "enabled": false}, 404)
}
