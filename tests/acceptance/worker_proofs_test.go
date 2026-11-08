//go:build integration

package acceptance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func TestWorkerRejectsUnsafeTransitionProofsWithoutConsumingSequence(t *testing.T) {
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
	seq := int64(1)
	send := func(route string, payload any, code string) {
		t.Helper()
		raw, _ := json.Marshal(payload)
		m := runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: f.agent.Identity, AttemptID: a.AttemptID, Seq: seq, Payload: raw}
		out, e := c.Transition(ctx, f.token, route, m)
		if code != "" {
			if runs.Code(e) != code {
				t.Fatalf("%s: expected %s got %v", route, code, e)
			}
		} else {
			if e != nil || !out.Accepted || out.NextSeq != seq+1 {
				t.Fatal(out, e)
			}
			seq++
		}
	}
	send("unknown", map[string]any{}, "INVALID_REQUEST")
	send("ack", map[string]any{"work_item_id": runs.ID()}, "INVALID_STATE")
	send("result", runs.Result{Outcome: "success", NoStateChild: true, Reaped: true}, "INVALID_STATE")
	send("ack", map[string]any{"work_item_id": a.WorkItemID}, "")
	send("prepare-result", runs.Prepare{Phase: "execution", Result: "success", NoStateChild: true, Reaped: true}, "INVALID_STATE")
	send("prepare-result", runs.Prepare{Phase: "preflight", Result: "failure", NoStateChild: true, Reaped: true}, "PREPARATION_FAILED_USE_RESULT")
	send("prepare-result", runs.Prepare{Phase: "preflight", Result: "success", Digest: "short", NoStateChild: true, Reaped: true}, "INVALID_REQUEST")
	for _, p := range []runs.Result{{Outcome: "preparation_failed", Reaped: true}, {Outcome: "preparation_failed", NoStateChild: true}, {Outcome: "apply", NoStateChild: true, Reaped: true}, {Outcome: "preparation_failed", Reason: "UNBOUNDED_PRIVATE_LOG", NoStateChild: true, Reaped: true}} {
		send("result", p, "INVALID_REQUEST")
	}
	backend, e := source.CanonicalBackend(map[string]any{"bucket": "fictional-state-bucket", "key": "local/state.tfstate", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}, source.BackendContext{Engine: catalog.Engine{Name: a.Spec.Engine.Name, Version: a.Spec.Engine.Version}, Workspace: a.Spec.Workspace, ExpectedOwner: a.Spec.BackendOwnerAccountID}, "")
	if e != nil {
		t.Fatal(e)
	}
	digest := strings.Repeat("a", 64)
	send("prepare-result", runs.Prepare{Phase: "preflight", Result: "success", Digest: digest, Backend: backend, NoStateChild: true, Reaped: true}, "")
	send("result", runs.Result{Outcome: "success", Digest: strings.Repeat("b", 64), NoStateChild: true, Reaped: true}, "MESSAGE_CONFLICT")
	send("result", runs.Result{Outcome: "success", Digest: digest, NoStateChild: true, Reaped: true}, "")
	send("result", runs.Result{Outcome: "success", Digest: digest, NoStateChild: true, Reaped: true}, "ATTEMPT_CLOSED")
	replay, e := c.Poll(ctx, f.token, runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20})
	if e != nil || replay != nil {
		t.Fatal("terminal work assigned again", e)
	}
}

func TestWorkerSchedulingRequiresTagsAndFencesReplacementGeneration(t *testing.T) {
	f := newPreflightFixture(t, []string{"isolated"})
	ctx := context.Background()
	c := coordinator.New(f.db.App, f.ticks)
	invalid := f.agent.Admission
	invalid.Phases = []string{"apply"}
	if _, e := c.Admit(ctx, f.token, invalid); runs.Code(e) != "RUNTIME_UNSUPPORTED" {
		t.Fatal("execution-capable admission accepted", e)
	}
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	poll := runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}
	if a, e := c.Poll(ctx, f.token, poll); e != nil || a != nil {
		t.Fatal("untagged worker received isolated work", a, e)
	}
	changed := f.agent.Admission
	changed.Agent = runs.ID()
	if _, e := c.Admit(ctx, f.token, changed); runs.Code(e) != "WORKER_BUSY" {
		t.Fatal("healthy process replaced", e)
	}
	f.exec(t, `UPDATE cosmoformer.workers SET last_contact_at=clock_timestamp()-interval '31 seconds',tags=ARRAY['isolated'] WHERE id=$1`, f.workerID)
	generation, e := c.Admit(ctx, f.token, changed)
	if e != nil || generation <= f.agent.Identity.Generation {
		t.Fatal("idle replacement did not fence generation", generation, e)
	}
	if e := c.Heartbeat(ctx, f.token, f.agent.Identity); runs.Code(e) != "STALE_AGENT" {
		t.Fatal("old generation retained identity", e)
	}
	poll.Identity = runs.Identity{WorkerID: f.workerID, Generation: generation, Agent: changed.Agent}
	poll.RequestID = runs.ID()
	a, e := c.Poll(ctx, f.token, poll)
	if e != nil || a == nil {
		t.Fatal("matching replacement could not claim work", e)
	}
	f.ticks.mu.Lock()
	f.ticks.tick += 301 * time.Second
	f.ticks.mu.Unlock()
	replay, e := c.Poll(ctx, f.token, poll)
	if e != nil || replay == nil || replay.RemainingMS != 0 {
		t.Fatal("expired reservation gained time", replay, e)
	}
	replacement := coordinator.New(f.db.App, f.ticks)
	replay, e = replacement.Poll(ctx, f.token, poll)
	if e != nil || replay.RemainingMS != 0 {
		t.Fatal("platform restart revived budget", e)
	}
}
