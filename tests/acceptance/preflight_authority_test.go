//go:build integration

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/store"
)

func TestPreparationRevalidatesAuthorityAndBackendClaims(t *testing.T) {
	for _, scenario := range []struct{ name, code string }{
		{"source advanced", "SOURCE_CHANGED"}, {"connection changed", "INPUTS_CHANGED"}, {"grant revoked", "INPUTS_CHANGED"}, {"runtime withdrawn", "STALE_AGENT"},
		{"worker paused", "STALE_AGENT"}, {"budget expired", "ATTEMPT_BUDGET_INSTANCE_ENDED"}, {"platform restarted", "ATTEMPT_BUDGET_INSTANCE_ENDED"},
		{"forged backend", "BACKEND_MISMATCH"}, {"forged owner", "BACKEND_MISMATCH"}, {"ungranted state", "FORBIDDEN"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newPreflightFixture(t)
			ctx := context.Background()
			if e := f.agent.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			c := coordinator.New(f.db.App, f.ticks)
			a, e := c.Poll(ctx, f.token, runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20})
			if e != nil || a == nil {
				t.Fatal(e)
			}
			payload, _ := json.Marshal(map[string]string{"work_item_id": a.WorkItemID})
			m := runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: f.agent.Identity, AttemptID: a.AttemptID, Seq: 1, Payload: payload}
			if _, e := c.Transition(ctx, f.token, "ack", m); e != nil {
				t.Fatal(e)
			}
			attributes := map[string]any{"bucket": "fictional-state-bucket", "key": "local/state.tfstate", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}
			if scenario.name == "ungranted state" {
				attributes["key"] = "another/state.tfstate"
			}
			backend, e := source.CanonicalBackend(attributes, source.BackendContext{Engine: catalog.Engine{Name: a.Spec.Engine.Name, Version: a.Spec.Engine.Version}, Workspace: a.Spec.Workspace, ExpectedOwner: a.Spec.BackendOwnerAccountID}, "")
			if e != nil {
				t.Fatal(e)
			}
			switch scenario.name {
			case "source advanced":
				f.exec(t, `UPDATE cosmoformer.source_subjects SET facts_revision=facts_revision+1 WHERE id=$1`, a.Source.SubjectID)
			case "connection changed":
				f.exec(t, `UPDATE cosmoformer.aws_connections SET revision=revision+1 WHERE id=$1`, a.Spec.AWS.ID)
			case "grant revoked":
				f.exec(t, `UPDATE cosmoformer.resource_grants SET enabled=false WHERE id=$1`, a.Spec.Grants[0].ID)
			case "runtime withdrawn":
				f.exec(t, `UPDATE cosmoformer.runtime_catalog SET withdrawn=true WHERE id=$1`, a.Spec.Engine.ID)
			case "worker paused":
				f.exec(t, `UPDATE cosmoformer.workers SET paused=true WHERE id=$1`, f.workerID)
			case "budget expired":
				f.ticks.mu.Lock()
				f.ticks.tick += 301 * time.Second
				f.ticks.mu.Unlock()
			case "platform restarted":
				c = coordinator.New(f.db.App, f.ticks)
			case "forged owner":
				backend.ExpectedOwner = "999900001111"
			case "forged backend":
				backend.Claims[0].Key = "untrusted/state.tfstate"
			}
			m.MessageID = runs.ID()
			m.Seq = 2
			m.Payload, _ = json.Marshal(runs.Prepare{Phase: "preflight", Result: "success", Digest: strings.Repeat("a", 64), Backend: backend, NoStateChild: true, Reaped: true})
			_, e = c.Transition(ctx, f.token, "prepare-result", m)
			if scenario.code == "FORBIDDEN" {
				if !errors.Is(e, store.ErrForbidden) {
					t.Fatal("ungranted state accepted", e)
				}
			} else if runs.Code(e) != scenario.code {
				t.Fatalf("expected %s, got %v", scenario.code, e)
			}
			var state string
			var messages, bindings int
			if e := f.db.Owner.QueryRow(ctx, `SELECT state FROM cosmoformer.attempts WHERE id=$1`, a.AttemptID).Scan(&state); e != nil || state != "preparing" {
				t.Fatal("rejected preparation changed attempt", state, e)
			}
			if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.attempt_messages WHERE attempt_id=$1`, a.AttemptID).Scan(&messages); e != nil || messages != 1 {
				t.Fatal("rejected preparation consumed sequence", messages, e)
			}
			if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.stack_bindings WHERE stack_id=$1`, f.stack).Scan(&bindings); e != nil || bindings != 0 {
				t.Fatal("rejected preparation bound state", bindings, e)
			}
		})
	}
}

func TestChangedHeadWithInvalidManifestRevokesQueuedAndActivePreflight(t *testing.T) {
	testChangedSourceRevokesPreflight(t, false)
}

func TestRetargetedPRRevokesQueuedAndActivePreflight(t *testing.T) {
	testChangedSourceRevokesPreflight(t, true)
}

func testChangedSourceRevokesPreflight(t *testing.T, retarget bool) {
	t.Helper()
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "active"}[active], func(t *testing.T) {
			f := newPreflightFixture(t)
			ctx := context.Background()
			if retarget {
				// The fixture starts with branch work; isolate the PR preview.
				f.exec(t, `UPDATE cosmoformer.work_items SET status='retired'`)
				f.exec(t, `UPDATE cosmoformer.operations SET status='retired'`)
				f.admin.call("POST", "/organizations/"+f.org+"/stacks/"+f.stack+"/plans", map[string]any{"source": map[string]any{"kind": "pr", "pr_number": 3}, "priority": "medium"}, 200)
			}
			if e := f.agent.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			var assignment runs.Assignment
			if active {
				raw := f.workerCall(t, "/poll", runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}, 200)
				var response struct {
					Assignment *runs.Assignment `json:"assignment"`
				}
				if e := json.Unmarshal(raw, &response); e != nil || response.Assignment == nil {
					t.Fatal("active fixture not assigned", e)
				}
				assignment = *response.Assignment
				payload, _ := json.Marshal(map[string]string{"work_item_id": assignment.WorkItemID})
				f.workerCall(t, "/attempts/"+assignment.AttemptID+"/ack", runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: f.agent.Identity, AttemptID: assignment.AttemptID, Seq: 1, Payload: payload}, 200)
			}
			if retarget {
				f.remote.prDestination = "release"
				f.admin.call("POST", "/organizations/"+f.org+"/stacks/"+f.stack+"/plans", map[string]any{"source": map[string]any{"kind": "pr", "pr_number": 3}, "priority": "medium"}, 409)
			} else {
				f.remote.commit = strings.Repeat("b", 40)
				f.remote.manifest = []byte("version: unsupported")
				f.admin.call("POST", "/organizations/"+f.org+"/repositories/"+f.repo+"/sync", map[string]any{"expected_revision": 2}, 422)
			}
			if !active {
				raw := f.workerCall(t, "/poll", runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}, 200)
				var response struct {
					Assignment *runs.Assignment `json:"assignment"`
				}
				if e := json.Unmarshal(raw, &response); e != nil || response.Assignment != nil {
					t.Fatal("obsolete queued work was assigned", e)
				}
				_, status, _ := f.operation(t)
				if status != "retired" {
					t.Fatal("obsolete queued work not retired", status)
				}
			} else {
				denied := f.workerCall(t, "/attempts/"+assignment.AttemptID+"/source-credential", map[string]any{"worker_id": f.agent.Identity.WorkerID, "generation": f.agent.Identity.Generation, "agent_instance": f.agent.Identity.Agent, "request_id": runs.ID(), "repository_id": f.repo, "canonical_https_url": assignment.Source.URL}, 409)
				if !strings.Contains(string(denied), "SOURCE_CHANGED") {
					t.Fatal("source credential gate did not reject obsolete source")
				}
				backend, e := source.CanonicalBackend(map[string]any{"bucket": "fictional-state-bucket", "key": "local/state.tfstate", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}, source.BackendContext{Engine: catalog.Engine{Name: assignment.Spec.Engine.Name, Version: assignment.Spec.Engine.Version}, Workspace: assignment.Spec.Workspace, ExpectedOwner: assignment.Spec.BackendOwnerAccountID}, "")
				if e != nil {
					t.Fatal(e)
				}
				payload, _ := json.Marshal(runs.Prepare{Phase: "preflight", Result: "success", Digest: strings.Repeat("a", 64), Backend: backend, NoStateChild: true, Reaped: true})
				denied = f.workerCall(t, "/attempts/"+assignment.AttemptID+"/prepare-result", runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: f.agent.Identity, AttemptID: assignment.AttemptID, Seq: 2, Payload: payload}, 409)
				if !strings.Contains(string(denied), "SOURCE_CHANGED") {
					t.Fatal("obsolete source was prepared")
				}
			}
			var bindings int
			if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.stack_bindings`).Scan(&bindings); e != nil || bindings != 0 {
				t.Fatal("obsolete source created a permanent binding", e)
			}
		})
	}
}
