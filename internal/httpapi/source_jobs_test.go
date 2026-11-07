//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
	"go.yaml.in/yaml/v3"
)

type elapsedTest struct{}

func (elapsedTest) Now() (time.Duration, error) { return time.Second, nil }

type sourceFixture struct {
	DB                                                 *testsupport.Database
	Server                                             *Server
	Handler                                            http.Handler
	Org, Repo, Integration, Account, Token, CSRF, Hook string
	Remote                                             *sourceFake
}
type sourceFake struct {
	mu                                 sync.Mutex
	Manifest                           []byte
	Commit                             string
	Calls, Creates, Updates, Refreshes int
	Status                             int
	PRCommit, PRState, PRDestination   string
	RefreshStarted                     chan struct{}
	RefreshRelease                     chan struct{}
}

func (s *sourceFake) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.Calls++
	status := s.Status
	commit := s.Commit
	raw := []byte(`{"uuid":"{fictional-repository}"}`)
	if r.URL.Path == "/site/oauth2/access_token" {
		s.Refreshes++
		raw = []byte(`{"access_token":"fictional-access","refresh_token":"fictional-refresh-next","expires_in":3600,"scopes":"repository pullrequest:write webhook"}`)
		if s.RefreshStarted != nil {
			close(s.RefreshStarted)
			s.RefreshStarted = nil
		}
		release := s.RefreshRelease
		s.mu.Unlock()
		if release != nil {
			<-release
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
	}
	if strings.Contains(r.URL.Path, "/refs/") {
		raw = []byte(`{"name":"main","target":{"hash":"` + commit + `"}}`)
	}
	if strings.Contains(r.URL.Path, "/src/") {
		raw = s.Manifest
	}
	if strings.Contains(r.URL.Path, "/pullrequests/") {
		prCommit, state, destination := s.PRCommit, s.PRState, s.PRDestination
		if prCommit == "" {
			prCommit = commit
		}
		if state == "" {
			state = "OPEN"
		}
		if destination == "" {
			destination = "main"
		}
		raw = []byte(`{"state":"` + state + `","source":{"repository":{"uuid":"{fictional-repository}"},"commit":{"hash":"` + prCommit + `"},"branch":{"name":"change"}},"destination":{"repository":{"uuid":"{fictional-repository}"},"branch":{"name":"` + destination + `"}}}`)
	}
	if strings.Contains(r.URL.Path, "/comments") {
		raw = []byte(`{"values":[]}`)
		if r.Method == "POST" {
			s.Creates++
			raw = []byte(`{"id":17}`)
		}
		if r.Method == "PUT" {
			s.Updates++
			raw = []byte(`{"id":17}`)
		}
	}
	s.mu.Unlock()
	if status == 0 {
		status = 200
	}
	header := make(http.Header)
	if status == 429 {
		header.Set("Retry-After", "60")
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(raw)), Request: r}, nil
}
func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	org, project, env, integration, repo, pool, account := runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID()
	token, csrf := identity.Token(), identity.Token()
	cipher, _ := cryptostore.New(bytes.Repeat([]byte{42}, 32))
	fp := cipher.Fingerprint()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, e := db.Owner.Exec(ctx, sql, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO cosmoformer.crypto_keys(fingerprint,state) VALUES($1,'active')`, fp[:])
	exec(`UPDATE cosmoformer.installation SET active_key_fingerprint=$1`, fp[:])
	exec(`INSERT INTO cosmoformer.accounts(id,username,display_name,installation_admin) VALUES($1,'fixture','Synthetic operator',true)`, account)
	exec(`INSERT INTO cosmoformer.sessions(account_id,verifier,csrf_verifier,credential_revision) VALUES($1,$2,$3,1)`, account, identity.Digest(token), identity.Digest(csrf))
	exec(`INSERT INTO cosmoformer.organizations(id,name) VALUES($1,'Synthetic')`, org)
	exec(`SELECT set_config('app.org_id',$1,false)`, org)
	exec(`INSERT INTO cosmoformer.memberships(org_id,account_id,org_admin) VALUES($1,$2,true)`, org, account)
	exec(`INSERT INTO cosmoformer.projects(id,org_id,name) VALUES($1,$2,'Synthetic')`, project, org)
	exec(`INSERT INTO cosmoformer.environments(id,org_id,project_id,name) VALUES($1,$2,$3,'Local')`, env, org, project)
	exec(`INSERT INTO cosmoformer.integrations(id,org_id,name,provider,enabled,hook_uuid) VALUES($1,$2,'Synthetic','bitbucket_cloud',true,'fixture-hook')`, integration, org)
	exec(`INSERT INTO cosmoformer.repositories(id,org_id,project_id,integration_id,external_id,url,tracked_branch,enabled) VALUES($1,$2,$3,$4,'{fictional-repository}','https://bitbucket.org/fictional/repo.git','main',true)`, repo, org, project, integration)
	exec(`INSERT INTO cosmoformer.worker_pools(id,org_id,name) VALUES($1,$2,'Synthetic')`, pool, org)
	exec(`INSERT INTO cosmoformer.runtime_catalog(engine,version,architecture,image_digest,approved) VALUES('opentofu','1.13.8','amd64',$1,true)`, "sha256:"+strings.Repeat("a", 64))
	exec(`INSERT INTO cosmoformer.aws_connections(org_id,project_id,role_arn,expected_account,enabled) VALUES($1,$2,'arn:aws:iam::111122223333:role/fictional','111122223333',true)`, org, project)
	for _, grant := range []struct{ Kind, Resource, Namespace, Key string }{{Kind: "repository", Resource: repo}, {Kind: "pool", Resource: pool}, {Kind: "s3_object", Namespace: "aws/fictional-bucket", Key: "state.tfstate"}, {Kind: "s3_object", Namespace: "aws/fictional-bucket", Key: "state.tfstate.tflock"}} {
		exec(`INSERT INTO cosmoformer.resource_grants(org_id,project_id,kind,resource_id,namespace,resource_key) VALUES($1,$2,$3,$4,$5,$6)`, org, project, grant.Kind, nilString(grant.Resource), nilString(grant.Namespace), nilString(grant.Key))
	}
	stack := catalog.Stack{ID: runs.ID(), Name: "Synthetic stack", ProjectID: project, EnvironmentID: env, Root: ".", TrackedBranch: "main", Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", BackendOwnerAccountID: "111122223333", Worker: catalog.Pool{ID: pool}, Approvals: catalog.Approvals{Minimum: 1}}
	manifest, _ := yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{stack}})
	remote := &sourceFake{Manifest: manifest, Commit: strings.Repeat("a", 40)}
	bb := bitbucket.New(&http.Client{Transport: remote})
	server := &Server{Store: db.App, Origin: "https://localhost:5173", Cipher: cipher, Bitbucket: bb, Coordinator: coordinator.New(db.App, elapsedTest{})}
	handler, _ := NewWithOptions(db.App, server.Origin, Options{Clock: elapsedTest{}, Cipher: cipher, Bitbucket: bb})
	return &sourceFixture{db, server, handler, org, repo, integration, account, token, csrf, "fixture-hook", remote}
}
func (f *sourceFixture) browser(t *testing.T, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	return f.browserKey(t, method, path, body, runs.ID(), status)
}
func (f *sourceFixture) browserKey(t *testing.T, method, path string, body any, key string, status int) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.AddCookie(&http.Cookie{Name: cookieName, Value: f.Token})
	r.Header.Set("Origin", f.Server.Origin)
	r.Header.Set("X-Cosmoformer-Request", "browser")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.CSRF)
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.Handler.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("%s %s status=%d body=%s", method, path, w.Code, w.Body.String())
	}
	return w
}
func (f *sourceFixture) configure(t *testing.T) {
	f.browser(t, "POST", "/api/v1/organizations/"+f.Org+"/integrations/"+f.Integration+"/credentials", map[string]any{"expected_revision": 1, "client_id": "fictional-client", "client_secret": "fictional-consumer-secret", "webhook_secret": strings.Repeat("w", 32), "hook_uuid": f.Hook}, 200)
}
func TestSignedWebhookQueuesAndReconcilesLocally(t *testing.T) {
	f := newSourceFixture(t)
	f.configure(t)
	f.connect(t)
	f.Remote.Calls = 0
	ctx := context.Background()
	body := []byte(`{"repository":{"uuid":"{fictional-repository}"}}`)
	delivery := runs.ID()
	send := func(body []byte, signature, delivery, event string, status int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/webhooks/bitbucket/"+f.Org+"/"+f.Integration, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Hub-Signature", signature)
		r.Header.Set("X-Hook-UUID", f.Hook)
		r.Header.Set("X-Request-UUID", delivery)
		r.Header.Set("X-Event-Key", event)
		w := httptest.NewRecorder()
		f.Handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	sign := func(b []byte) string {
		h := hmac.New(sha256.New, []byte(strings.Repeat("w", 32)))
		h.Write(b)
		return "sha256=" + hex.EncodeToString(h.Sum(nil))
	}
	send(body, sign(body), delivery, "repo:push", 200)
	send(body, sign(body), delivery, "repo:push", 200)
	send(body, "sha256="+strings.Repeat("0", 64), runs.ID(), "repo:push", 403)
	if f.Remote.Calls != 0 {
		t.Fatal("webhook performed provider I/O")
	}
	f.Server.sourceJobTick(ctx, f.Org, false)
	var count int
	f.DB.Owner.Exec(ctx, `SELECT set_config('app.org_id',$1,false)`, f.Org)
	if e := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.runs`).Scan(&count); e != nil || count != 1 {
		t.Fatal("source reconciliation did not queue exactly one preflight", count, e)
	}
	f.Server.sourceJobTick(ctx, f.Org, true)
	if e := f.DB.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.runs`).Scan(&count); e != nil || count != 1 {
		t.Fatal("unchanged sweep duplicated work", count, e)
	}
	prBody := []byte(`{"repository":{"uuid":"{fictional-repository}"},"pullrequest":{"id":3}}`)
	send(prBody, sign(prBody), runs.ID(), "pullrequest:created", 200)
	f.Server.sourceJobTick(ctx, f.Org, false)
	f.Server.sourceJobTick(ctx, f.Org, false)
	if f.Remote.Creates != 1 {
		t.Fatal("missing PR feedback", f.Remote.Creates)
	}
	f.Remote.Commit = strings.Repeat("b", 40)
	send(prBody, sign(prBody), runs.ID(), "pullrequest:updated", 200)
	f.Server.sourceJobTick(ctx, f.Org, false)
	f.Server.sourceJobTick(ctx, f.Org, false)
	if f.Remote.Creates != 1 || f.Remote.Updates != 1 {
		t.Fatal("feedback comment identity not reused", f.Remote.Creates, f.Remote.Updates)
	}
}
func TestOAuthStateIsSessionBoundAndOneUse(t *testing.T) {
	f := newSourceFixture(t)
	f.configure(t)
	w := f.browser(t, "POST", "/api/v1/organizations/"+f.Org+"/integrations/"+f.Integration+"/connect", map[string]any{}, 200)
	var link struct {
		URL string `json:"authorization_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &link)
	u, e := url.Parse(link.URL)
	if e != nil {
		t.Fatal(e)
	}
	state := u.Query().Get("state")
	if u.Host != "bitbucket.org" || !identity.ValidToken(state) {
		t.Fatal("invalid authorize URL")
	}
	callback := "/oauth/bitbucket/callback?state=" + state + "&code=fictional-code"
	saved := f.Token
	f.Token = identity.Token()
	f.browser(t, "GET", callback, nil, 403)
	f.Token = saved
	f.browser(t, "GET", callback, nil, 303)
	f.browser(t, "GET", callback, nil, 403)
	if f.Remote.Refreshes != 1 {
		t.Fatal("callback replay exchanged code again")
	}
	var sessions int
	if e = f.DB.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.sessions`).Scan(&sessions); e != nil || sessions != 1 {
		t.Fatal("OAuth changed local login", e)
	}
}
func TestInterruptedRefreshRequiresReconnect(t *testing.T) {
	f := newSourceFixture(t)
	f.configure(t)
	ctx := context.Background()
	var old integrationRecord
	f.Server.serviceTx(ctx, f.Org, func(tx pgx.Tx) error {
		var e error
		old, e = f.Server.loadIntegration(ctx, tx, f.Org, f.Integration)
		return e
	})
	old.Credentials.Tokens = bitbucket.Tokens{AccessToken: "old", RefreshToken: "fictional-refresh"}
	old.Credentials.ExpiresAt = time.Now().Add(-time.Minute)
	sealed, e := f.Server.sealRevision(ctx, f.Org, f.Integration, "integration", old.Semantic, jsonBytes(old.Credentials))
	if e != nil {
		t.Fatal(e)
	}
	f.Server.serviceTx(ctx, f.Org, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE cosmoformer.integrations SET encrypted_credentials=$2 WHERE id=$1`, f.Integration, sealed)
		return e
	})
	started, release := make(chan struct{}), make(chan struct{})
	f.Remote.RefreshStarted = started
	f.Remote.RefreshRelease = release
	done := make(chan error, 1)
	go func() { _, e := f.Server.apiToken(ctx, f.Org, f.Integration); done <- e }()
	<-started
	if _, e = f.Server.apiToken(ctx, f.Org, f.Integration); e == nil {
		t.Fatal("second refresh did not respect durable claim")
	}
	close(release)
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if f.Remote.Refreshes != 1 {
		t.Fatal("rotating refresh duplicated")
	}
}

func (f *sourceFixture) connect(t *testing.T) {
	t.Helper()
	w := f.browser(t, "POST", "/api/v1/organizations/"+f.Org+"/integrations/"+f.Integration+"/connect", map[string]any{}, 200)
	var link struct {
		URL string `json:"authorization_url"`
	}
	json.Unmarshal(w.Body.Bytes(), &link)
	u, e := url.Parse(link.URL)
	if e != nil {
		t.Fatal(e)
	}
	f.browser(t, "GET", "/oauth/bitbucket/callback?state="+u.Query().Get("state")+"&code=fictional-code", nil, 303)
}

func TestManagedCredentialReplacementPreservesSemanticFencing(t *testing.T) {
	f := newSourceFixture(t)
	ctx := context.Background()
	base := "/api/v1/organizations/" + f.Org
	w := f.browser(t, "POST", base+"/secrets", map[string]any{"name": "Fictional", "value": "first-private-value"}, 200)
	var created struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &created)
	f.browser(t, "PATCH", base+"/secrets/"+created.ID, map[string]any{"expected_revision": 1, "enabled": true, "value": "replacement-private-value"}, 200)
	f.browser(t, "PATCH", base+"/secrets/"+created.ID, map[string]any{"expected_revision": 2, "enabled": false}, 200)
	f.browser(t, "PATCH", base+"/secrets/"+created.ID, map[string]any{"expected_revision": 2, "enabled": true, "value": "stale-value"}, 409)
	var revision int64
	var sealed []byte
	if e := f.DB.Owner.QueryRow(ctx, `SELECT semantic_revision,encrypted_value FROM cosmoformer.secrets WHERE id=$1`, created.ID).Scan(&revision, &sealed); e != nil {
		t.Fatal(e)
	}
	plain, e := f.Server.Cipher.Open(sealed, cryptostore.AAD(f.Org, created.ID, "managed_secret", uint64(revision)))
	if e != nil || string(plain) != "replacement-private-value" || revision != 3 {
		t.Fatal("secret semantic identity was not advanced", revision, e)
	}
	path := base + "/repositories/" + f.Repo + "/source-credentials"
	f.browser(t, "POST", path, map[string]any{"expected_revision": 0, "username": "x-token-auth", "token": "first-private-token"}, 200)
	f.browser(t, "POST", path, map[string]any{"expected_revision": 1, "username": "x-token-auth", "token": "replacement-private-token"}, 200)
	f.browser(t, "POST", path, map[string]any{"expected_revision": 1, "username": "x-token-auth", "token": "stale-private-token"}, 409)
	var currentID string
	var currentRevision, semantic int64
	var currentSealed []byte
	if e := f.DB.Owner.QueryRow(ctx, `SELECT id::text,revision,semantic_revision,encrypted_value FROM cosmoformer.source_credentials WHERE repository_id=$1 AND enabled`, f.Repo).Scan(&currentID, &currentRevision, &semantic, &currentSealed); e != nil {
		t.Fatal(e)
	}
	currentPlain, e := f.Server.Cipher.Open(currentSealed, cryptostore.AAD(f.Org, currentID, "source_credential", uint64(semantic)))
	if e != nil || currentRevision != 2 || !strings.Contains(string(currentPlain), "replacement-private-token") || strings.Contains(string(currentPlain), "stale-private-token") {
		t.Fatal("stale edit replaced the latest credential", currentRevision, e)
	}
	var active, all int
	if e = f.DB.Owner.QueryRow(ctx, `SELECT count(*) FILTER(WHERE enabled),count(*) FROM cosmoformer.source_credentials WHERE repository_id=$1`, f.Repo).Scan(&active, &all); e != nil || active != 1 || all != 2 {
		t.Fatal("old credential remained active", active, all, e)
	}
	raw, _ := json.Marshal(map[string]any{"expected_revision": 1, "enabled": true, "value": "replacement-private-value"})
	fast := identity.Digest("PATCH\x00" + base + "/secrets/" + created.ID + "\x00" + string(raw))
	var leaked bool
	if e = f.DB.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.browser_requests WHERE request_digest=$1 OR result::text LIKE '%replacement-private-value%' OR result::text LIKE '%replacement-private-token%')`, fast).Scan(&leaked); e != nil || leaked {
		t.Fatal("plaintext credential evidence retained in browser receipt", e)
	}
}
