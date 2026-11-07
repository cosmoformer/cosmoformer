//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/cosmoformer/cosmoformer/internal/worker"
	"go.yaml.in/yaml/v3"
)

type testClock struct {
	mu   sync.Mutex
	tick time.Duration
}

func (c *testClock) Now() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick, nil
}

type sourceTransport struct {
	manifest      []byte
	commit        string
	prDestination string
	mu            sync.Mutex
}

func (s *sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Host != "api.bitbucket.org" {
		panic("test attempted external access")
	}
	body := []byte(`{"name":"main","target":{"hash":"` + s.commit + `"}}`)
	if !strings.Contains(r.URL.Path, "/refs/") && !strings.Contains(r.URL.Path, "/src/") {
		body = []byte(`{"uuid":"{fictional-repository}"}`)
	}
	if strings.Contains(r.URL.Path, "/src/") {
		body = s.manifest
	}
	if strings.Contains(r.URL.Path, "/pullrequests/") {
		destination := s.prDestination
		if destination == "" {
			destination = "main"
		}
		body = []byte(`{"state":"OPEN","source":{"repository":{"uuid":"{fictional-repository}"},"commit":{"hash":"` + s.commit + `"},"branch":{"name":"change"}},"destination":{"repository":{"uuid":"{fictional-repository}"},"branch":{"name":"` + destination + `"}}}`)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: r}, nil
}

type lossTransport struct {
	base      http.RoundTripper
	ack       runs.Message
	dropped   bool
	beforeAck func(runs.Message)
}

func (l *lossTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/ack") {
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			return nil, e
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		json.Unmarshal(raw, &l.ack)
		if l.beforeAck != nil {
			callback := l.beforeAck
			l.beforeAck = nil
			callback(l.ack)
		}
		response, e := l.base.RoundTrip(r)
		if e == nil && !l.dropped {
			l.dropped = true
			response.Body.Close()
			return nil, errors.New("synthetic response loss after commit")
		}
		return response, e
	}
	return l.base.RoundTrip(r)
}
func newPreflightFixture(t *testing.T, requiredTags ...[]string) *preflightFixture {
	t.Helper()
	ticks := &testClock{tick: time.Second}
	remote := &sourceTransport{commit: strings.Repeat("a", 40)}
	cipher, e := cryptostore.New(bytes.Repeat([]byte{42}, 32))
	if e != nil {
		t.Fatal(e)
	}
	options := httpapi.Options{Clock: ticks, Cipher: cipher, Bitbucket: bitbucket.New(&http.Client{Transport: remote})}
	db, admin := fixtureOptions(t, options)
	fp := cipher.Fingerprint()
	if _, e := db.Owner.Exec(context.Background(), `INSERT INTO cosmoformer.crypto_keys(fingerprint,state) VALUES($1,'active')`, fp[:]); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Owner.Exec(context.Background(), `UPDATE cosmoformer.installation SET active_key_fingerprint=$1`, fp[:]); e != nil {
		t.Fatal(e)
	}
	org := id(admin.call("POST", "/organizations", map[string]any{"name": "Synthetic organization"}, 200))
	base := "/organizations/" + org
	project := id(admin.call("POST", base+"/projects", map[string]any{"name": "Synthetic project"}, 200))
	env := id(admin.call("POST", base+"/projects/"+project+"/environments", map[string]any{"name": "Local"}, 200))
	integration := id(admin.call("POST", base+"/integrations", map[string]any{"name": "Synthetic source"}, 200))
	repo := id(admin.call("POST", base+"/repositories", map[string]any{"project_id": project, "integration_id": integration, "external_id": "{fictional-repository}", "url": "https://bitbucket.org/fictional/repo.git", "tracked_branch": "main", "enabled": true}, 200))
	pool := id(admin.call("POST", base+"/worker-pools", map[string]any{"name": "Local source workers"}, 200))
	runtime := id(admin.call("POST", "/admin/runtimes", map[string]any{"engine": "opentofu", "version": "1.13.8", "architecture": "amd64", "image_digest": "sha256:" + strings.Repeat("a", 64), "approved": true}, 200))
	admin.call("POST", base+"/aws-connections", map[string]any{"project_id": project, "environment_id": env, "role_arn": "arn:aws:iam::111122223333:role/fictional", "expected_account": "111122223333"}, 200)
	for _, resource := range []map[string]any{{"kind": "repository", "resource_id": repo}, {"kind": "pool", "resource_id": pool}, {"kind": "s3_object", "namespace": "aws/fictional-state-bucket", "resource_key": "local/state.tfstate"}, {"kind": "s3_object", "namespace": "aws/fictional-state-bucket", "resource_key": "local/state.tfstate.tflock"}} {
		resource["project_id"] = project
		resource["environment_id"] = env
		admin.call("POST", base+"/resource-grants", resource, 200)
	}
	team := id(admin.call("POST", base+"/teams", map[string]any{"name": "Local planners"}, 200))
	session := admin.call("GET", "/session", nil, 200)
	actor := session["actor"].(map[string]any)["id"].(string)
	admin.call("POST", base+"/teams/"+team+"/members", map[string]any{"account_id": actor}, 200)
	admin.call("POST", base+"/grants", map[string]any{"team_id": team, "project_id": project, "environment_id": env, "role": "planner"}, 200)
	declaration := catalog.Stack{ID: runs.ID(), Name: "Local fixture", ProjectID: project, EnvironmentID: env, Root: ".", TrackedBranch: "main", Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", BackendOwnerAccountID: "111122223333", BackendFiles: []string{}, VarFiles: []string{}, Worker: catalog.Pool{ID: pool, Tags: []string{}}, Secrets: []catalog.Secret{}, Approvals: catalog.Approvals{Minimum: 1}, ChangePaths: []string{}, DependsOn: []string{}}
	if len(requiredTags) > 0 {
		declaration.Worker.Tags = requiredTags[0]
	}
	manifest, _ := yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{declaration}})
	remote.manifest = manifest
	admin.call("POST", base+"/repositories/"+repo+"/source-credentials", map[string]any{"expected_revision": 0, "username": "x-token-auth", "token": "synthetic-repository-only"}, 200)
	syncResult := admin.call("POST", base+"/repositories/"+repo+"/sync", map[string]any{"expected_revision": 1}, 200)
	stack := syncResult["stack_ids"].([]any)[0].(string)
	admin.call("POST", base+"/stacks/"+stack+"/plans", map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium"}, 200)
	enrolled := admin.call("POST", base+"/workers", map[string]any{"name": "Synthetic worker", "pool_id": pool, "tags": []string{}}, 200)
	workerID := enrolled["id"].(string)
	token := enrolled["token"].(string)
	admin.call("POST", base+"/workers/"+workerID+"/resume", map[string]any{"expected_revision": 1}, 200)
	handler, _ := httpapi.NewWithOptions(db.App, origin, options)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	workerHTTP := server.Client()
	wire := &lossTransport{base: workerHTTP.Transport}
	workerHTTP.Transport = wire
	// Real protocol and source parsing, with only external Git I/O replaced. The
	// separate Git/process tests exercise the fixed subprocess boundary itself.
	attemptDir, e := os.MkdirTemp("", "cf-worker-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(attemptDir) })
	agent := worker.Agent{HTTP: workerHTTP, Origin: server.URL, Token: token, Clock: ticks, AttemptDir: attemptDir, Admission: runs.Admission{Protocol: 1, WorkerID: workerID, Agent: runs.ID(), RuntimeID: runtime, Engine: "opentofu", Version: "1.13.8", Arch: "amd64", ImageDigest: "sha256:" + strings.Repeat("a", 64), GitPackage: "1:2.47.3-0+deb13u1", GitVersion: "git version 2.47.3", Phases: []string{"preflight"}, ClockSource: "boottime", ClockOK: true}}
	agent.Git.Run = func(ctx context.Context, dir string, args, env []string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "rev-parse") {
			return []byte(remote.commit + "\n"), nil
		}
		if strings.Contains(joined, "checkout --quiet --detach") {
			if e := os.WriteFile(filepath.Join(dir, "cosmoformer.yaml"), manifest, 0600); e != nil {
				return nil, e
			}
			return nil, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`terraform {
 backend "s3" {
  bucket = "fictional-state-bucket"
  key = "local/state.tfstate"
  region = "eu-west-1"
   encrypt = true
  use_lockfile = true
 }
}
`), 0600)
		}
		return nil, nil
	}
	agent.Git.Helper = "/usr/local/bin/cosmoformer-worker"
	if _, e := db.Owner.Exec(context.Background(), `SELECT set_config('app.org_id',$1,false)`, org); e != nil {
		t.Fatal(e)
	}
	return &preflightFixture{db: db, admin: admin, agent: &agent, ticks: ticks, org: org, stack: stack, repo: repo, workerID: workerID, token: token, wire: wire, remote: remote}
}

type preflightFixture struct {
	db                                *testsupport.Database
	admin                             *client
	agent                             *worker.Agent
	ticks                             *testClock
	org, stack, repo, workerID, token string
	wire                              *lossTransport
	remote                            *sourceTransport
}

func TestLocalSourcePreflightThroughWorkerAPI(t *testing.T) {
	f := newPreflightFixture(t)
	db, agent, ticks, org, token, wire := f.db, f.agent, f.ticks, f.org, f.token, f.wire
	ctx := context.Background()
	wire.beforeAck = func(m runs.Message) {
		m.Seq = 3
		m.MessageID = runs.ID()
		_, e := coordinator.New(db.App, ticks).Transition(ctx, token, "ack", m)
		var issue *runs.Error
		if !errors.As(e, &issue) || issue.Code != "EXPECTED_SEQUENCE" || issue.ExpectedSeq != 1 {
			t.Fatal("sequence gap was not rejected with the current expected sequence", e)
		}
	}
	if _, e := coordinator.New(db.App, ticks).Admit(ctx, token, agent.Admission); e != nil {
		t.Fatal("coordinator admission", e)
	}
	if e := agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	if !wire.dropped {
		t.Fatal("response-loss fixture was not exercised")
	}
	replayCoordinator := coordinator.New(db.App, ticks)
	if _, e := replayCoordinator.Transition(ctx, token, "ack", wire.ack); e != nil {
		t.Fatal("accepted ack could not be replayed", e)
	}
	altered := wire.ack
	altered.Payload = []byte(`{"work_item_id":"` + runs.ID() + `"}`)
	if _, e := replayCoordinator.Transition(ctx, token, "ack", altered); e == nil {
		t.Fatal("changed accepted message was replayed")
	}
	tx, e := db.App.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, org)
	for _, query := range []string{`SELECT count(*) FROM cosmoformer.worker_reservations`, `SELECT count(*) FROM cosmoformer.backend_reservations`, `SELECT count(*) FROM cosmoformer.start_permissions`, `SELECT count(*) FROM cosmoformer.plans`} {
		var count int
		if e = tx.QueryRow(ctx, query).Scan(&count); e != nil || count != 0 {
			t.Fatal("preflight left execution work or ownership", query, count, e)
		}
	}
	var status, reason string
	if e = tx.QueryRow(ctx, `SELECT status,terminal_reason FROM cosmoformer.operations`).Scan(&status, &reason); e != nil || status != "succeeded" || reason != "PREFLIGHT_ONLY" {
		var proof []byte
		tx.QueryRow(ctx, `SELECT stop_proof FROM cosmoformer.attempts`).Scan(&proof)
		t.Fatal(status, reason, e, string(proof))
	}
	var bindings int
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.stack_bindings`).Scan(&bindings); e != nil || bindings != 1 {
		t.Fatal("missing permanent binding", e)
	}
	// Current generation gates every replay; a replacement process cannot adopt
	// the former live worker's assignments merely by knowing their IDs.
	other := agent.Admission
	other.Agent = runs.ID()
	coord := coordinator.New(db.App, ticks)
	if _, e = coord.Admit(ctx, token, other); e == nil {
		t.Fatal("healthy duplicate worker admitted")
	}
}
