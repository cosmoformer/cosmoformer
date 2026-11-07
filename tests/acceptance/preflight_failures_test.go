//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/jackc/pgx/v5"
)

func (f *preflightFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, e := f.db.Owner.Exec(context.Background(), query, args...); e != nil {
		t.Fatal(e)
	}
}
func (f *preflightFixture) operation(t *testing.T) (string, string, int64) {
	t.Helper()
	var id, status string
	var revision int64
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text,status,revision FROM cosmoformer.operations WHERE stack_id=$1 ORDER BY created_at DESC LIMIT 1`, f.stack).Scan(&id, &status, &revision); e != nil {
		t.Fatal(e)
	}
	return id, status, revision
}
func (f *preflightFixture) workerCall(t *testing.T, path string, body any, status int) []byte {
	t.Helper()
	raw, e := json.Marshal(body)
	if e != nil {
		t.Fatal(e)
	}
	r, e := http.NewRequest("POST", f.agent.Origin+"/worker/v1"+path, bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.token)
	client := *f.agent.HTTP
	client.Transport = f.wire.base
	response, e := client.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	raw, e = io.ReadAll(response.Body)
	if e != nil {
		t.Fatal(e)
	}
	if response.StatusCode != status {
		t.Fatalf("%s: got %d want %d: %s", path, response.StatusCode, status, raw)
	}
	return raw
}
func TestPreflightFailureRecoveryAndRetryFencing(t *testing.T) {
	for _, scenario := range []struct{ name, reason string }{{"checkout", "SOURCE_CHECKOUT_FAILED"}, {"manifest", "MANIFEST_INVALID"}, {"declaration", "DECLARATION_MISMATCH"}, {"inspection", "SOURCE_INSPECTION_FAILED"}, {"directory", "ATTEMPT_DIRECTORY_UNAVAILABLE"}, {"unreaped", ""}, {"module unreaped", ""}} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newPreflightFixture(t)
			ctx := context.Background()
			run := f.agent.Git.Run
			if scenario.name == "directory" {
				p := filepath.Join(t.TempDir(), "file")
				if e := os.WriteFile(p, []byte("fixture"), 0600); e != nil {
					t.Fatal(e)
				}
				f.agent.AttemptDir = p
			}
			f.agent.Git.Run = func(ctx context.Context, dir string, args, env []string) ([]byte, error) {
				if scenario.name == "checkout" {
					return nil, source.ErrSource
				}
				if scenario.name == "unreaped" {
					return nil, source.ErrProcess
				}
				if scenario.name == "module unreaped" && strings.HasPrefix(filepath.Base(dir), "module-") {
					return nil, source.ErrProcess
				}
				b, e := run(ctx, dir, args, env)
				if e != nil {
					return b, e
				}
				if strings.Contains(strings.Join(args, " "), "checkout --quiet --detach") {
					switch scenario.name {
					case "module unreaped":
						file, e2 := os.OpenFile(filepath.Join(dir, "main.tf"), os.O_APPEND|os.O_WRONLY, 0600)
						if e2 != nil {
							return nil, e2
						}
						_, e = file.WriteString(`
module "remote" { source = "git::https://bitbucket.org/fictional/repo.git?ref=` + strings.Repeat("a", 40) + `" }
`)
						file.Close()

					case "manifest":
						e = os.WriteFile(filepath.Join(dir, "cosmoformer.yaml"), []byte("version: unsupported"), 0600)
					case "declaration":
						raw, err := os.ReadFile(filepath.Join(dir, "cosmoformer.yaml"))
						if err != nil {
							return nil, err
						}
						e = os.WriteFile(filepath.Join(dir, "cosmoformer.yaml"), bytes.Replace(raw, []byte("workspace: default"), []byte("workspace: other"), 1), 0600)
					case "inspection":
						e = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`terraform { backend "local" {} }`), 0600)
					}
				}
				return b, e
			}
			if e := f.agent.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			e := f.agent.RunOne(ctx)
			op, status, revision := f.operation(t)
			base := "/organizations/" + f.org
			if scenario.name == "unreaped" || scenario.name == "module unreaped" {
				if e == nil || status != "preparing" {
					t.Fatal("unproven stop released assignment", status, e)
				}
				var reservations int
				if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.worker_reservations WHERE worker_id=$1`, f.workerID).Scan(&reservations); e != nil || reservations != 1 {
					t.Fatal("reservation lost", e)
				}
				entries, readErr := os.ReadDir(f.agent.AttemptDir)
				if readErr != nil || len(entries) == 0 {
					t.Fatal("unproven stop removed preparation files", readErr)
				}
				f.admin.call("POST", base+"/operations/"+op+"/retry", map[string]any{"expected_revision": revision}, 409)
				return
			}
			if e != nil || status != "failed" {
				t.Fatal("failure not recorded", status, e)
			}
			var reason string
			if e := f.db.Owner.QueryRow(ctx, `SELECT terminal_reason FROM cosmoformer.operations WHERE id=$1`, op).Scan(&reason); e != nil || reason != scenario.reason {
				t.Fatal(reason, e)
			}
			entries, e := os.ReadDir(f.agent.AttemptDir)
			if scenario.name != "directory" && (e != nil || len(entries) != 0) {
				t.Fatal("reaped failure retained files", e)
			}
			listed := f.admin.call("GET", base+"/preflights", nil, 200)["preflights"].([]any)
			if len(listed) != 1 || listed[0].(map[string]any)["can_retry"] != true {
				t.Fatal("failed preflight not retryable", listed)
			}
			f.admin.call("POST", base+"/operations/"+op+"/retry", map[string]any{"expected_revision": revision - 1}, 409)
			f.admin.call("POST", base+"/operations/"+op+"/retry", map[string]any{"expected_revision": revision}, 200)
			f.admin.call("POST", base+"/operations/"+op+"/retry", map[string]any{"expected_revision": revision}, 409)
			_, status, next := f.operation(t)
			if status != "queued" || next <= revision {
				t.Fatal("retry did not requeue with new revision")
			}
			f.admin.call("PATCH", base+"/operations/"+op+"/priority", map[string]any{"expected_revision": next, "priority": "high"}, 200)
			f.admin.call("PATCH", base+"/operations/"+op+"/priority", map[string]any{"expected_revision": next, "priority": "low"}, 409)
		})
	}
}
func TestQueuedWorkRechecksAuthority(t *testing.T) {
	for name, sql := range map[string]string{
		"repository disabled":    `UPDATE cosmoformer.repositories SET enabled=false WHERE id=$1`,
		"connection revoked":     `UPDATE cosmoformer.aws_connections SET enabled=false WHERE org_id=$1`,
		"resource grant revoked": `UPDATE cosmoformer.resource_grants SET enabled=false WHERE org_id=$1`,
		"requester disabled":     `UPDATE cosmoformer.accounts SET enabled=false WHERE username='admin' AND $1::uuid IS NOT NULL`,
		"planner revoked":        `DELETE FROM cosmoformer.team_members WHERE org_id=$1`,
		"source changed":         `UPDATE cosmoformer.source_subjects SET facts_revision=facts_revision+1 WHERE org_id=$1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newPreflightFixture(t)
			ctx := context.Background()
			if e := f.agent.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			arg := f.org
			if name == "repository disabled" {
				arg = f.agent.Admission.WorkerID
				var repo string
				if e := f.db.Owner.QueryRow(ctx, `SELECT repository_id::text FROM cosmoformer.stacks WHERE id=$1`, f.stack).Scan(&repo); e != nil {
					t.Fatal(e)
				}
				arg = repo
			}
			f.exec(t, sql, arg)
			c := coordinator.New(f.db.App, f.ticks)
			out, e := c.Poll(ctx, f.token, runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20})
			if e != nil || out != nil {
				t.Fatal("revoked queued work assigned", out, e)
			}
			_, status, _ := f.operation(t)
			if status != "retired" {
				t.Fatal("revoked work was not retired", status)
			}
		})
	}
}
func TestWorkerProtocolReplayBudgetAndStaleIdentity(t *testing.T) {
	f := newPreflightFixture(t)
	ctx := context.Background()
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	c := coordinator.New(f.db.App, f.ticks)
	poll := runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}
	first, e := c.Poll(ctx, f.token, poll)
	if e != nil || first == nil {
		t.Fatal(first, e)
	}
	f.ticks.mu.Lock()
	f.ticks.tick += time.Second
	f.ticks.mu.Unlock()
	replay, e := c.Poll(ctx, f.token, poll)
	if e != nil || replay.AttemptID != first.AttemptID || replay.RemainingMS >= first.RemainingMS {
		t.Fatal("replay minted assignment budget", replay, e)
	}
	next := poll
	next.RequestID = runs.ID()
	again, e := c.Poll(ctx, f.token, next)
	if e != nil || again.AttemptID != first.AttemptID {
		t.Fatal("reservation was not retained", e)
	}
	if e := c.Heartbeat(ctx, f.token, f.agent.Identity); e != nil {
		t.Fatal(e)
	}
	stale := f.agent.Identity
	stale.Generation++
	if e := c.Heartbeat(ctx, f.token, stale); runs.Code(e) != "STALE_AGENT" {
		t.Fatal("stale heartbeat accepted", e)
	}
	f.workerCall(t, "/heartbeat", map[string]any{"worker_id": stale.WorkerID, "generation": stale.Generation, "agent_instance": stale.Agent, "request_id": runs.ID(), "agent_tick": 1, "phase": "preflight"}, 409)
	f.workerCall(t, "/heartbeat", map[string]any{"worker_id": f.workerID, "generation": f.agent.Identity.Generation, "agent_instance": f.agent.Identity.Agent, "request_id": runs.ID(), "agent_tick": 1, "phase": "preflight"}, 200)
	f.workerCall(t, "/heartbeat", map[string]any{}, 422)
	payload, _ := json.Marshal(map[string]string{"work_item_id": first.WorkItemID})
	msg := runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: f.agent.Identity, AttemptID: first.AttemptID, Seq: 1, Payload: payload}
	if _, e := c.Transition(ctx, f.token, "ack", msg); e != nil {
		t.Fatal(e)
	}
	changed := msg
	changed.Payload = []byte(`{"work_item_id":"` + runs.ID() + `"}`)
	if _, e := c.Transition(ctx, f.token, "ack", changed); runs.Code(e) != "MESSAGE_CONFLICT" {
		t.Fatal("changed replay accepted", e)
	}
	var state string
	if e := f.db.Owner.QueryRow(ctx, `SELECT state FROM cosmoformer.attempts WHERE id=$1`, first.AttemptID).Scan(&state); e != nil || state != "uncertain" {
		t.Fatal("conflict did not retain uncertain ownership", state, e)
	}
	f.workerCall(t, "/attempts/"+runs.ID()+"/ack", msg, 422)
	invalid := msg
	invalid.Protocol = 2
	if _, e := c.Transition(ctx, f.token, "ack", invalid); runs.Code(e) != "INVALID_REQUEST" {
		t.Fatal(e)
	}
	if _, e := c.Poll(ctx, "not-a-token", poll); e == nil {
		t.Fatal("invalid worker token accepted")
	}
	if e := c.WithAttempt(ctx, f.token, f.agent.Identity, first.AttemptID, func(_tx pgx.Tx, _ coordinator.Worker, _ runs.Assignment) error { return errors.New("must not run") }); e == nil {
		t.Fatal("uncertain attempt retained positive authority")
	}
}

func TestPreflightControlsRequireCurrentScopedGrants(t *testing.T) {
	f := newPreflightFixture(t)
	op, _, revision := f.operation(t)
	base := "/organizations/" + f.org
	f.admin.call("PATCH", base+"/operations/"+runs.ID()+"/priority", map[string]any{"expected_revision": 1, "priority": "high"}, 404)
	f.exec(t, `UPDATE cosmoformer.grants SET role='administrator' WHERE org_id=$1`, f.org)
	listed := f.admin.call("GET", base+"/preflights", nil, 200)["preflights"].([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["can_prioritize"] != true || listed[0].(map[string]any)["can_retry"] != false {
		t.Fatal("manager capabilities were not separated from execution", listed)
	}
	f.admin.call("PATCH", base+"/operations/"+op+"/priority", map[string]any{"expected_revision": revision, "priority": "high"}, 200)
	f.admin.call("POST", base+"/operations/"+op+"/retry", map[string]any{"expected_revision": revision + 1}, 403)
	f.exec(t, `DELETE FROM cosmoformer.team_members WHERE org_id=$1`, f.org)
	listed = f.admin.call("GET", base+"/preflights", nil, 200)["preflights"].([]any)
	if len(listed) != 0 {
		t.Fatal("revoked scope retained preflight visibility")
	}
	f.admin.call("PATCH", base+"/operations/"+op+"/priority", map[string]any{"expected_revision": revision + 1, "priority": "low"}, 403)
}
