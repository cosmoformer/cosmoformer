//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/worker"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func deployer(t *testing.T, f *executionFixture) {
	t.Helper()
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
}
func attemptRevision(t *testing.T, f *executionFixture) int64 {
	t.Helper()
	var v int64
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT revision FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID).Scan(&v); e != nil {
		t.Fatal(e)
	}
	return v
}
func caseID(t *testing.T, f *executionFixture) string {
	t.Helper()
	var v string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.recovery_cases WHERE origin_attempt_id=$1`, f.assignment.AttemptID).Scan(&v); e != nil {
		t.Fatal(e)
	}
	return v
}
func recoveryBody(rev any, method, reference string, complete bool) map[string]any {
	return map[string]any{"expected_revision": rev, "stop_proof": map[string]any{"method": method, "statement": "Exact host and process ownership verified; no secret values.", "protected_reference": reference}, "reconciliation": map[string]any{"complete": complete, "statement": "Infrastructure and state were independently reconciled.", "protected_references": []string{"protected-record-74"}}}
}
func recoveryPath(f *executionFixture, id string) string {
	return "/organizations/" + f.org + "/recoveries/" + id
}

func TestMilestone9ControlsExactAuthorityDeliveryAndReplay(t *testing.T) {
	f := newExecutionFixture(t)
	f.stoppedInit(t)
	path := "/organizations/" + f.org + "/attempts/" + f.assignment.AttemptID
	// Installation administration alone (with planning) cannot stop execution.
	f.admin.call("POST", path+"/cancel", map[string]any{"expected_attempt_revision": attemptRevision(t, f)}, 403)
	deployer(t, f)
	f.admin.call("POST", path+"/force-stop", map[string]any{"expected_attempt_revision": attemptRevision(t, f), "confirm": true, "confirmed_context": map[string]any{"stack_id": f.assignment.StackID, "environment_id": runs.ID(), "attempt_id": f.assignment.AttemptID}}, 422)
	key := identity.Token()
	body := map[string]any{"expected_attempt_revision": attemptRevision(t, f)}
	first := f.admin.callKey("POST", path+"/cancel", body, 202, key)
	again := f.admin.callKey("POST", path+"/cancel", body, 202, key)
	if first["control_id"] != again["control_id"] || first["state"] != "requested" {
		t.Fatal("control replay changed identity", first, again)
	}
	heartbeatRaw := f.workerCall(t, "/heartbeat", map[string]any{"request_id": runs.ID(), "worker_id": f.workerID, "generation": f.agent.Identity.Generation, "agent_instance": f.agent.Identity.Agent, "agent_tick": 1000, "phase": "execution"}, 200)
	var heartbeat map[string]any
	json.Unmarshal(heartbeatRaw, &heartbeat)
	controls := heartbeat["controls"].([]any)
	if len(controls) != 1 {
		t.Fatal(heartbeat)
	}
	c := controls[0].(map[string]any)
	if c["attempt_id"] != f.assignment.AttemptID || c["agent_instance"] != f.agent.Identity.Agent || c["command_ordinal"] != float64(1) {
		t.Fatal("incomplete control capability", c)
	}
	f.transition(t, "controls/ack", runs.ControlAck{ID: first["control_id"].(string), Ordinal: 1, State: "delivered"}, 200)
	f.transition(t, "controls/ack", runs.ControlAck{ID: first["control_id"].(string), Ordinal: 1, State: "observed"}, 200)
	f.retained(t)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='deployer'`)
	f.admin.callKey("POST", path+"/cancel", body, 403, key)
}

func TestMilestone9RestartRetainsOwnershipUntilManualStopAndHolds(t *testing.T) {
	ctx := context.Background()
	f := newExecutionFixture(t)
	deployer(t, f)
	f.prepare(t)
	permit := f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200)
	if permit.Permission == nil {
		t.Fatal("no permission")
	}
	// Merely missing contact is a warning, not a recovery event.
	f.exec(t, `UPDATE cosmoformer.workers SET last_contact_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, f.workerID)
	f.retained(t)
	var n int
	f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.recovery_cases`).Scan(&n)
	if n != 0 {
		t.Fatal("contact opened recovery")
	}
	restarted := f.agent.Admission
	restarted.Agent = runs.ID()
	responseRaw := f.workerCall(t, "/admit", restarted, 409)
	var response map[string]any
	json.Unmarshal(responseRaw, &response)
	if response["code"] != "RECOVERY_REQUIRED" {
		t.Fatal(response)
	}
	id := caseID(t, f)
	f.workerCall(t, "/admit", restarted, 409)
	var epoch int64
	if e := f.db.Owner.QueryRow(ctx, `SELECT b.state_epoch FROM cosmoformer.backend_targets b JOIN cosmoformer.stack_bindings s ON s.target_id=b.id WHERE s.stack_id=$1`, f.stack).Scan(&epoch); e != nil || epoch != 1 {
		t.Fatal("replayed restart changed epoch", epoch, e)
	}
	path := recoveryPath(f, id)
	f.retained(t)
	r := f.admin.call("GET", path, nil, 200)
	if _, present := r["evidence"]; present {
		t.Fatal("initial recovery emitted an invalid empty evidence DTO", r)
	}
	// Agent cannot attest to an issued permission with a lost launch response.
	f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, false), 409)
	r = f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "host", "protected-host-stop-74", false), 200)
	if r["stop_authority"] != "manual_host" || r["infrastructure_outcome"] != "unknown" || r["can_clear"] != false {
		t.Fatal(r)
	}
	var workers, backends, holds, bindings int
	e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='recovery' AND owner_id=$2 AND released_at IS NULL),(SELECT count(*) FROM cosmoformer.stack_bindings WHERE stack_id=$3)`, f.assignment.AttemptID, id, f.stack).Scan(&workers, &backends, &holds, &bindings)
	if e != nil || workers != 0 || backends != 0 || holds < 1 || bindings != 1 {
		t.Fatal("incorrect stop disposition", workers, backends, holds, bindings, e)
	}
	var paused bool
	f.db.Owner.QueryRow(context.Background(), `SELECT paused FROM cosmoformer.workers WHERE id=$1`, f.workerID).Scan(&paused)
	if !paused {
		t.Fatal("old process not fenced")
	}
	f.admin.call("POST", path+"/clear", map[string]any{"expected_evidence_revision": r["evidence_revision"]}, 409)
	replacement, newAgent := runs.ID(), runs.ID()
	f.exec(t, `UPDATE cosmoformer.workers SET agent_instance=$2 WHERE id=$1`, f.workerID, newAgent)
	f.exec(t, `INSERT INTO cosmoformer.attempts(id,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline) SELECT $2,a.org_id,a.work_item_id,a.operation_id,a.stack_id,a.target_id,a.worker_id,w.generation,$3,'claimed',a.budget_platform_instance,a.accepted_tick,a.deadline_tick,a.cap_seconds,a.display_deadline FROM cosmoformer.attempts a JOIN cosmoformer.workers w ON w.id=a.worker_id WHERE a.id=$1`, f.assignment.AttemptID, replacement, newAgent)
	f.exec(t, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) SELECT org_id,worker_id,id,generation,agent_instance FROM cosmoformer.attempts WHERE id=$1`, replacement)
	f.exec(t, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, replacement)
	f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "host", "protected-host-recheck", false), 200)
	if e := f.db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=$1)`, replacement).Scan(&workers, &backends); e != nil || workers != 1 || backends != 1 {
		t.Fatal("old evidence released replacement reservations", workers, backends, e)
	}
}

func TestMilestone9RecoveryVotesRevisionsDistinctCurrentAndNoSourceRead(t *testing.T) {
	f := newApprovalFixture(t, 2)
	deployer(t, f.executionFixture)
	attemptPath := "/organizations/" + f.org + "/attempts/" + f.assignment.AttemptID
	f.voter.call("POST", attemptPath+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f.executionFixture)}, 200)
	id := caseID(t, f.executionFixture)
	path := recoveryPath(f.executionFixture, id)
	f.provider.mu.Lock()
	f.provider.outage = true
	calls := f.provider.calls
	f.provider.mu.Unlock()
	r := f.voter.call("GET", path, nil, 200)
	r = f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), 200)
	rev := r["evidence_revision"]
	vote := map[string]any{"expected_evidence_revision": rev}
	r = f.voter.call("POST", path+"/approvals", vote, 200)
	assertCount(t, r, 1, 2)
	r = f.voter.call("POST", path+"/approvals", vote, 200)
	assertCount(t, r, 1, 2)
	second := f.second(t, "recovery.second")
	r = second.call("POST", path+"/approvals", vote, 200)
	assertCount(t, r, 2, 2)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
	r = f.voter.call("GET", path, nil, 200)
	assertCount(t, r, 0, 2)
	f.voter.call("POST", path+"/clear", vote, 409)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=NULL WHERE role='approver'`)
	r = f.voter.call("GET", path, nil, 200)
	assertCount(t, r, 2, 2)
	changed := recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true)
	changed["reconciliation"].(map[string]any)["statement"] = "Reconciliation independently checked again."
	r = f.voter.call("PATCH", path+"/evidence", changed, 200)
	assertCount(t, r, 0, 2)
	f.voter.call("POST", path+"/approvals", vote, 409)
	f.voter.call("POST", path+"/clear", vote, 409)
	vote["expected_evidence_revision"] = r["evidence_revision"]
	f.voter.call("POST", path+"/approvals", vote, 200)
	second.call("POST", path+"/approvals", vote, 200)
	key := identity.Token()
	r = f.voter.callKey("POST", path+"/clear", vote, 200, key)
	if r["status"] != "cleared" {
		t.Fatal(r)
	}
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='deployer'`)
	f.voter.callKey("POST", path+"/clear", vote, 403, key)
	f.provider.mu.Lock()
	if f.provider.calls != calls {
		t.Error("recovery read Bitbucket")
	}
	f.provider.mu.Unlock()
	var status string
	f.db.Owner.QueryRow(context.Background(), `SELECT status FROM cosmoformer.plans WHERE id=$1`, f.plan["id"]).Scan(&status)
	if status != "invalidated" {
		t.Fatal("pre-recovery plan revived", status)
	}
	var versions int
	f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.recovery_evidence_versions WHERE case_id=$1`, id).Scan(&versions)
	if versions != 2 {
		t.Fatal("evidence history lost", versions)
	}
}

func TestMilestone9DiagnosticPlanRetainsProvenanceAndNeverDeploys(t *testing.T) {
	f := newPlanningFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
	receipt := f.requestNative(t)
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	f.agent.Native = diagnosticNative(t, true)
	if e := f.agent.RunOne(ctx); e == nil {
		t.Fatal("uncertain native child accepted")
	}
	var attempt, id string
	if e := f.db.Owner.QueryRow(ctx, `SELECT a.id::text,r.id::text FROM cosmoformer.attempts a JOIN cosmoformer.recovery_cases r ON r.origin_attempt_id=a.id WHERE a.operation_id=$1`, receipt["operation_id"]).Scan(&attempt, &id); e != nil {
		t.Fatal(e)
	}
	path := "/organizations/" + f.org + "/recoveries/" + id
	plans := "/organizations/" + f.org + "/stacks/" + f.stack + "/plans"
	r := f.admin.call("GET", path, nil, 200)
	diagnostic := func(rev any) map[string]any {
		return map[string]any{"intent": "recovery_plan", "recovery_case_id": id, "expected_evidence_revision": rev, "source": map[string]any{"kind": "branch"}, "priority": "medium"}
	}
	f.admin.call("POST", plans, diagnostic(r["evidence_revision"]), 409)
	r = f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "operator", "protected-stop", false), 200)
	f.admin.call("POST", plans, map[string]any{"intent": "native_plan", "source": map[string]any{"kind": "branch"}, "priority": "medium"}, 409)
	// A queued diagnostic is retired when its bound evidence changes.
	stale := f.admin.call("POST", plans, diagnostic(r["evidence_revision"]), 200)
	r = f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "operator", "protected-stop-rechecked", true), 200)
	var status string
	f.db.Owner.QueryRow(ctx, `SELECT status FROM cosmoformer.operations WHERE id=$1`, stale["operation_id"]).Scan(&status)
	if status != "retired" {
		t.Fatal("stale diagnostic retained launch intent", status)
	}
	// Re-admission follows independent stop/fencing, never launch adoption.
	f.exec(t, `UPDATE cosmoformer.workers SET paused=false,last_contact_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, f.workerID)
	f.agent.Admission.Agent = runs.ID()
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	next := f.admin.call("POST", plans, diagnostic(r["evidence_revision"]), 200)
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal("diagnostic preflight", e)
	}
	f.agent.Native = diagnosticNative(t, false)
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal("diagnostic execution", e)
	}
	var plan, provenance string
	var version int64
	if e := f.db.Owner.QueryRow(ctx, `SELECT id::text,status,provenance,recovery_evidence_revision FROM cosmoformer.plans WHERE operation_id=$1`, next["operation_id"]).Scan(&plan, &status, &provenance, &version); e != nil {
		t.Fatal(e)
	}
	if status != "recovery_only" || provenance != "recovery" || float64(version) != r["evidence_revision"] {
		t.Fatal("diagnostic identity lost", status, provenance, version)
	}
	var holds int
	f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.artifact_holds h JOIN cosmoformer.artifacts a ON a.id=h.artifact_id JOIN cosmoformer.attempts t ON t.id=a.attempt_id WHERE t.operation_id=$1 AND h.owner_kind='recovery' AND h.owner_id=$2 AND h.released_at IS NULL`, next["operation_id"], id).Scan(&holds)
	if holds < 7 {
		t.Fatal("diagnostic evidence unheld", holds)
	}
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'approver' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
	summary := f.admin.call("GET", "/organizations/"+f.org+"/plans/"+plan+"/approvals", nil, 200)
	if summary["eligible"] != false {
		t.Fatal("diagnostic eligible for normal approval", summary)
	}
	vote := map[string]any{"expected_evidence_revision": r["evidence_revision"]}
	f.admin.call("POST", path+"/approvals", vote, 200)
	f.admin.call("POST", path+"/clear", vote, 200)
	// Clearance never promotes a diagnostic. A new normal request has fresh input
	// provenance and must earn separate normal approvals.
	f.db.Owner.QueryRow(ctx, `SELECT status FROM cosmoformer.plans WHERE id=$1`, plan).Scan(&status)
	if status != "recovery_only" {
		t.Fatal("diagnostic promoted", status)
	}
	fresh := f.requestNative(t)
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	f.db.Owner.QueryRow(ctx, `SELECT status,provenance FROM cosmoformer.plans WHERE operation_id=$1`, fresh["operation_id"]).Scan(&status, &provenance)
	if status != "ready" || provenance != "normal" {
		t.Fatal("fresh normal plan unavailable", status, provenance)
	}
}

func diagnosticNative(t *testing.T, uncertain bool) worker.NativeIO {
	return func(ctx context.Context, in worker.NativeCommand, spawn func(runs.CommandEvent) error) (runs.CommandEvent, error) {
		if e := in.Start(func() error { return nil }); e != nil {
			return runs.CommandEvent{}, e
		}
		pid := int64(31000 + in.Permission.Ordinal)
		event := runs.CommandEvent{Ordinal: in.Permission.Ordinal, Permission: in.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
		if e := spawn(event); e != nil {
			return event, e
		}
		if uncertain {
			return event, source.ErrProcess
		}
		switch in.Descriptor.Command {
		case "schema":
			in.Output.WriteString(`{"format_version":"1.0"}`)
		case "plan":
			for _, a := range in.Args {
				if strings.HasPrefix(a, "-out=") {
					if e := os.WriteFile(strings.TrimPrefix(a, "-out="), []byte("synthetic saved diagnostic plan"), 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
		case "show":
			in.Output.WriteString(`{"format_version":"1.2","terraform_version":"1.13.8","planned_values":{},"configuration":{}}`)
		}
		zero := 0
		empty := true
		event.Event = "stopped"
		event.Exit = &zero
		event.GroupEmpty = &empty
		event.Pipes = "drained"
		return event, nil
	}
}

func concurrentBrowser(c *client, method, path string, body any) (int, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, c.base+"/api/v1"+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("X-Cosmoformer-Request", "browser")
	req.Header.Set("Idempotency-Key", identity.Token())
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.AddCookie(&http.Cookie{Name: "__Host-cosmoformer_session", Value: c.token})
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		return 0, e
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode, nil
}
func TestMilestone9ConcurrentEvidenceEditAndClearance(t *testing.T) {
	f := newApprovalFixture(t, 1)
	deployer(t, f.executionFixture)
	f.voter.call("POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f.executionFixture)}, 200)
	path := recoveryPath(f.executionFixture, caseID(t, f.executionFixture))
	r := f.voter.call("GET", path, nil, 200)
	r = f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), 200)
	vote := map[string]any{"expected_evidence_revision": r["evidence_revision"]}
	f.voter.call("POST", path+"/approvals", vote, 200)
	edit := recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true)
	edit["reconciliation"].(map[string]any)["statement"] = "New independent reconciliation observations."
	type reply struct {
		status int
		err    error
	}
	results := make(chan reply, 2)
	start := make(chan struct{})
	for _, call := range []struct {
		method, suffix string
		body           any
	}{{"PATCH", "/evidence", edit}, {"POST", "/clear", vote}} {
		go func() {
			<-start
			s, e := concurrentBrowser(f.voter, call.method, path+call.suffix, call.body)
			results <- reply{s, e}
		}()
	}
	close(start)
	one, two := <-results, <-results
	if one.err != nil || two.err != nil || !(one.status == 200 && two.status == 409 || one.status == 409 && two.status == 200) {
		t.Fatal("edit/clear were not serialized", one, two)
	}
	current := f.voter.call("GET", path, nil, 200)
	if current["status"] == "open" && current["counted_approvals"] != float64(0) {
		t.Fatal("stale votes counted after concurrent edit", current)
	}
}
func TestMilestone9CancelCompletionRace(t *testing.T) {
	f := newExecutionFixture(t)
	deployer(t, f)
	stopped := f.stoppedInit(t)
	p := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{{Ordinal: 1, Permission: stopped.Permission, Status: "stopped", Exit: stopped.Exit, Stop: stopped}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	result := f.message(p)
	cancel := map[string]any{"expected_attempt_revision": attemptRevision(t, f)}
	start := make(chan struct{})
	statuses := make(chan int, 2)
	go func() {
		<-start
		s, e := concurrentBrowser(f.admin, "POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/cancel", cancel)
		if e != nil {
			s = 0
		}
		statuses <- s
	}()
	go func() {
		<-start
		raw, _ := json.Marshal(result)
		req, _ := http.NewRequest("POST", f.agent.Origin+"/worker/v1/attempts/"+f.assignment.AttemptID+"/result", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.token)
		res, e := f.agent.HTTP.Do(req)
		if e != nil {
			statuses <- 0
			return
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		statuses <- res.StatusCode
	}()
	close(start)
	one, two := <-statuses, <-statuses
	if !(one == 200 && (two == 202 || two == 409) || two == 200 && (one == 202 || one == 409)) {
		t.Fatal("unexpected completion/control ordering", one, two)
	}
	var worker, backend int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=$1)`, f.assignment.AttemptID).Scan(&worker, &backend); e != nil || worker != 0 || backend != 0 {
		t.Fatal("matching stopped attempt not released", worker, backend, e)
	}
}

func TestMilestone9SourceOnlyUncertaintyHasNoBackendCase(t *testing.T) {
	f := newPreflightFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	f.agent.Git.Run = func(context.Context, string, []string, []string) ([]byte, error) { return nil, source.ErrSource }
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	var op string
	var opRevision int64
	if e := f.db.Owner.QueryRow(ctx, `SELECT id::text,revision FROM cosmoformer.operations WHERE stack_id=$1`, f.stack).Scan(&op, &opRevision); e != nil {
		t.Fatal(e)
	}
	f.admin.call("POST", "/organizations/"+f.org+"/operations/"+op+"/retry", map[string]any{"expected_revision": opRevision}, 200)
	f.agent.Git.Run = func(context.Context, string, []string, []string) ([]byte, error) { return nil, source.ErrProcess }
	if e := f.agent.RunOne(ctx); e == nil {
		t.Fatal("uncertain Git process accepted")
	}
	restarted := f.agent.Admission
	restarted.Agent = runs.ID()
	f.workerCall(t, "/admit", restarted, 409)
	var attempt string
	var revision int64
	if e := f.db.Owner.QueryRow(ctx, `SELECT id::text,revision FROM cosmoformer.attempts WHERE stack_id=$1 AND closed_at IS NULL`, f.stack).Scan(&attempt, &revision); e != nil {
		t.Fatal(e)
	}
	var cases, targets int
	f.db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.recovery_cases),(SELECT count(*) FROM cosmoformer.stack_bindings)`).Scan(&cases, &targets)
	if cases != 0 || targets != 0 {
		t.Fatal("source-only preflight invented backend recovery", cases, targets)
	}
	f.admin.call("PATCH", "/organizations/"+f.org+"/attempts/"+attempt+"/source-stop", recoveryBody(revision, "operator", "protected-git-stop", false), 200)
	var reservations int
	var status string
	f.db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations),(SELECT status FROM cosmoformer.operations WHERE id=a.operation_id) FROM cosmoformer.attempts a WHERE id=$1`, attempt).Scan(&reservations, &status)
	if reservations != 0 || status != "failed" {
		t.Fatal("source ownership not accounted", reservations, status)
	}
	if e := f.db.Owner.QueryRow(ctx, `SELECT revision FROM cosmoformer.operations WHERE id=$1`, op).Scan(&opRevision); e != nil {
		t.Fatal(e)
	}
	// A prior preparation failure cannot make a later unknown attempt retryable.
	f.admin.call("POST", "/organizations/"+f.org+"/operations/"+op+"/retry", map[string]any{"expected_revision": opRevision}, 409)
}

func TestMilestone9ContradictoryAcceptedStopNeedsManualAuthority(t *testing.T) {
	f := newApprovalFixture(t, 1)
	deployer(t, f.executionFixture)
	var raw []byte
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT stop_facts FROM cosmoformer.start_permissions WHERE attempt_id=$1 AND ordinal=4`, f.assignment.AttemptID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var event runs.CommandEvent
	if json.Unmarshal(raw, &event) != nil {
		t.Fatal("stop facts missing")
	}
	exit := 17
	event.Exit = &exit
	f.transition(t, "command-events", event, 409)
	path := recoveryPath(f.executionFixture, caseID(t, f.executionFixture))
	r := f.admin.call("GET", path, nil, 200)
	f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, false), 409)
	key := identity.Token()
	body := recoveryBody(r["revision"], "operator", "protected-independent-host-stop", true)
	r = f.admin.callKey("PATCH", path+"/evidence", body, 200, key)
	if r["stop_authority"] != "manual_operator" {
		t.Fatal(r)
	}
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='deployer'`)
	f.admin.callKey("PATCH", path+"/evidence", body, 403, key)
}

// A lost-response retry authorizes and projects current permissions, never the
// protected references or eligibility cached when the mutation first committed.
func TestMilestone9RecoveryReplayUsesCurrentEvidenceAuthority(t *testing.T) {
	for _, route := range []string{"evidence", "approvals", "clear"} {
		t.Run(route, func(t *testing.T) {
			f := newApprovalFixture(t, 1)
			deployer(t, f.executionFixture)
			f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'raw_evidence_read' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
			f.voter.call("POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f.executionFixture)}, 200)
			path := recoveryPath(f.executionFixture, caseID(t, f.executionFixture))
			r := f.voter.call("GET", path, nil, 200)
			method := "POST"
			var body any
			if route == "evidence" {
				method = "PATCH"
				body = recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true)
			} else {
				r = f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), 200)
				body = map[string]any{"expected_evidence_revision": r["evidence_revision"]}
				if route == "clear" {
					f.voter.call("POST", path+"/approvals", body, 200)
				}
			}
			key := identity.Token()
			first := f.voter.callKey(method, path+"/"+route, body, 200, key)
			check := func(r map[string]any, raw bool) {
				t.Helper()
				ev := r["evidence"].(map[string]any)
				ref := ev["stop_proof"].(map[string]any)["protected_reference"]
				refs := ev["reconciliation"].(map[string]any)["protected_references"].([]any)
				if raw && (ref != f.assignment.AttemptID || len(refs) != 1) || !raw && (ref != "" || len(refs) != 0) {
					t.Fatal("wrong current reference visibility", r)
				}
				if r["vote_event_id"] != first["vote_event_id"] {
					t.Fatal("retry changed original vote identity")
				}
			}
			check(first, true)
			check(f.voter.callKey(method, path+"/"+route, body, 200, key), true)
			var receipt string
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT result::text FROM cosmoformer.browser_requests WHERE key=$1`, key).Scan(&receipt); e != nil {
				t.Fatal(e)
			}
			if strings.Contains(receipt, f.assignment.AttemptID) || strings.Contains(receipt, "protected-record-74") {
				t.Fatal("receipt retained protected recovery context")
			}
			// Also exercise receipts written by the original implementation. This is
			// dummy fixture content and never a production browser receipt.
			legacy, _ := json.Marshal(map[string]any{"Body": first})
			f.exec(t, `UPDATE cosmoformer.browser_requests SET result=$2 WHERE key=$1`, key, legacy)
			check(f.voter.callKey(method, path+"/"+route, body, 200, key), true)
			f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='raw_evidence_read'`)
			check(f.voter.callKey(method, path+"/"+route, body, 200, key), false)
			f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
			if route != "approvals" {
				current := f.voter.callKey(method, path+"/"+route, body, 200, key)
				if current["counted_approvals"] != float64(0) {
					t.Fatal("replayed stale approval count", current)
				}
			}
			action := "deployer"
			if route == "approvals" {
				action = "approver"
			}
			f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role=$1`, action)
			f.voter.callKey(method, path+"/"+route, body, 403, key)
			var events, versions int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.approvals WHERE subject_kind='recovery'),(SELECT count(*) FROM cosmoformer.recovery_evidence_versions)`).Scan(&events, &versions); e != nil {
				t.Fatal(e)
			}
			want := 1
			if route == "evidence" {
				want = 0
			}
			if events != want || versions != 1 {
				t.Fatal("retry repeated mutation", events, versions)
			}
		})
	}
}

func TestMilestone9RecoveryPositiveAuthorityRespectsInstallationGate(t *testing.T) {
	f := newApprovalFixture(t, 1)
	deployer(t, f.executionFixture)
	f.voter.call("POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f.executionFixture)}, 200)
	path := recoveryPath(f.executionFixture, caseID(t, f.executionFixture))
	r := f.voter.call("GET", path, nil, 200)
	for _, flag := range []string{"maintenance", "clock_unreliable"} {
		f.exec(t, `UPDATE cosmoformer.installation SET `+flag+`=true`)
		// Evidence and positive stop reports remain available during the gate.
		r = f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), 200)
		if r["can_approve"] != false || r["can_clear"] != false || r["can_plan"] != false {
			t.Fatal("positive authority exposed during gate", r)
		}
		vote := map[string]any{"expected_evidence_revision": r["evidence_revision"]}
		status := 409
		if flag == "clock_unreliable" {
			status = 503
		}
		f.voter.call("POST", path+"/approvals", vote, status)
		f.voter.call("POST", path+"/clear", vote, status)
		f.exec(t, `UPDATE cosmoformer.installation SET `+flag+`=false`)
		r = f.voter.call("GET", path, nil, 200)
	}
}

func TestMilestone9ManualStopCannotBecomeAgentAuthority(t *testing.T) {
	for _, method := range []string{"host", "operator", "protocol"} {
		t.Run(method, func(t *testing.T) {
			f := newExecutionFixture(t)
			deployer(t, f)
			if method == "protocol" {
				prepared := f.prepare(t)
				f.transition(t, "result", runs.ExecutionResult{Outcome: "preparation_failed", Commands: []runs.CommandResult{}, Artifacts: []string{prepared.Artifact}, PreInitDigest: prepared.Digest, NoStateChild: true, Reaped: true}, 200)
			}
			f.admin.call("POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f)}, 200)
			path := recoveryPath(f, caseID(t, f))
			r := f.admin.call("GET", path, nil, 200)
			if method != "protocol" {
				r = f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], method, "protected-manual-proof", true), 200)
				if r["stop_authority"] != "manual_"+method {
					t.Fatal("manual proof mislabeled", r)
				}
				var stored string
				var hasDigest bool
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT stop_proof->>'method',result_digest IS NOT NULL FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID).Scan(&stored, &hasDigest); e != nil || stored != method || hasDigest {
					t.Fatal("manual closure fabricated protocol facts", stored, hasDigest, e)
				}
			}
			status := 409
			if method == "protocol" {
				status = 200
			}
			out := f.admin.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), status)
			if method == "protocol" {
				if out["stop_authority"] != "agent_protocol" {
					t.Fatal("positive zero-permit native result rejected", out)
				}
			} else {
				if out["code"] != "INDEPENDENT_STOP_PROOF_REQUIRED" {
					t.Fatal(out)
				}
				current := f.admin.call("GET", path, nil, 200)
				if current["revision"] != r["revision"] || current["evidence_revision"] != r["evidence_revision"] || current["stop_authority"] != "manual_"+method {
					t.Fatal("rejected relabel changed evidence", current)
				}
				var versions int
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.recovery_evidence_versions WHERE authority='agent_protocol'`).Scan(&versions); e != nil || versions != 0 {
					t.Fatal("fabricated protocol history", versions, e)
				}
			}
		})
	}
}

// Start with a healthy persisted latch, then discover the fault at the fresh
// positive mutation. Compare complete rows so vote identities and hold release
// timestamps cannot change unnoticed.
func TestMilestone9RecoveryFreshClockFence(t *testing.T) {
	for _, route := range []string{"approvals", "clear"} {
		for _, fault := range []string{"read failure", "database backward", "elapsed discrepancy"} {
			t.Run(route+"/"+fault, func(t *testing.T) {
				f := newApprovalFixture(t, 1)
				deployer(t, f.executionFixture)
				f.voter.call("POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f.executionFixture)}, 200)
				path := recoveryPath(f.executionFixture, caseID(t, f.executionFixture))
				r := f.voter.call("GET", path, nil, 200)
				r = f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), 200)
				body := map[string]any{"expected_evidence_revision": r["evidence_revision"]}
				// Establish this platform instance's trusted UTC/elapsed baseline.
				f.voter.call("POST", path+"/approvals", body, 200)
				f.provider.mu.Lock()
				calls := f.provider.calls
				f.provider.mu.Unlock()
				snapshot := func() string {
					t.Helper()
					var s string
					if e := f.db.Owner.QueryRow(context.Background(), `SELECT jsonb_build_array(
					 (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM cosmoformer.recovery_cases r),
					 (SELECT jsonb_agg(to_jsonb(v) ORDER BY case_id,account_id) FROM cosmoformer.recovery_votes v),
					 (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM cosmoformer.approvals a),
					 (SELECT jsonb_agg(to_jsonb(h) ORDER BY artifact_id,owner_kind,owner_id,reason) FROM cosmoformer.artifact_holds h),
					 (SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM cosmoformer.operations o),
					 (SELECT jsonb_agg(to_jsonb(a) ORDER BY id) FROM cosmoformer.attempts a)
					)::text`).Scan(&s); e != nil {
						t.Fatal(e)
					}
					return s
				}
				before := snapshot()
				switch fault {
				case "read failure":
					f.clock.mu.Lock()
					f.clock.failed = true
					f.clock.mu.Unlock()
				case "database backward":
					f.exec(t, `UPDATE cosmoformer.installation SET authority_highwater=clock_timestamp()+interval '1 hour'`)
				case "elapsed discrepancy":
					f.clock.mu.Lock()
					f.clock.offset += 5 * time.Second
					f.clock.mu.Unlock()
				}
				latched := func() bool {
					t.Helper()
					var b bool
					if e := f.db.Owner.QueryRow(context.Background(), `SELECT clock_unreliable FROM cosmoformer.installation`).Scan(&b); e != nil {
						t.Fatal(e)
					}
					return b
				}
				if latched() {
					t.Fatal("fault was already latched")
				}
				key := identity.Token()
				for range 2 {
					response := f.voter.callKey("POST", path+"/"+route, body, 503, key)
					if response["code"] != "CLOCK_UNRELIABLE" {
						t.Fatal(response)
					}
					if snapshot() != before {
						t.Fatal("rejected request changed recovery authority or holds")
					}
				}
				if fault != "read failure" && !latched() {
					t.Fatal("discrepancy latch rolled back")
				}
				var receipts int
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.browser_requests WHERE key=$1`, key).Scan(&receipts); e != nil || receipts != 0 {
					t.Fatal("rejection saved a receipt", receipts, e)
				}
				// Reset fixture time only; production requires the host maintenance procedure.
				f.clock.mu.Lock()
				f.clock.failed = false
				f.clock.offset = 0
				f.clock.mu.Unlock()
				f.exec(t, `UPDATE cosmoformer.installation SET clock_unreliable=false,authority_highwater=clock_timestamp(),authority_platform_instance=NULL`)
				first := f.voter.callKey("POST", path+"/"+route, body, 200, key)
				if route == "clear" && first["status"] != "cleared" || route == "approvals" && (first["vote_event_id"] == "" || first["counted_approvals"] != float64(1)) {
					t.Fatal("healthy clock did not grant the requested recovery action", first)
				}
				after := snapshot()
				if after == before {
					t.Fatal("healthy clock request had no effect")
				}
				f.clock.mu.Lock()
				f.clock.failed = true
				f.clock.mu.Unlock()
				again := f.voter.callKey("POST", path+"/"+route, body, 200, key)
				if again["vote_event_id"] != first["vote_event_id"] || snapshot() != after {
					t.Fatal("successful replay repeated effects")
				}
				role := "approver"
				if route == "clear" {
					role = "deployer"
				}
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role=$1`, role)
				f.voter.callKey("POST", path+"/"+route, body, 403, key)
				f.provider.mu.Lock()
				defer f.provider.mu.Unlock()
				if calls != f.provider.calls {
					t.Fatal("recovery required Bitbucket")
				}
			})
		}
	}
}

func TestMilestone9ClockFaultAllowsIndependentStopAndEvidence(t *testing.T) {
	for _, latched := range []bool{false, true} {
		t.Run(fmt.Sprint(latched), func(t *testing.T) {
			f := newExecutionFixture(t)
			deployer(t, f)
			f.prepare(t)
			f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200)
			f.admin.call("POST", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/recovery", map[string]any{"expected_attempt_revision": attemptRevision(t, f)}, 200)
			clk := &approvalClock{failed: true}
			handler, _ := httpapi.NewWithOptions(f.db.App, origin, httpapi.Options{Clock: clk})
			server := httptest.NewServer(handler)
			defer server.Close()
			c := &client{t: t, base: server.URL, token: f.admin.token, csrf: f.admin.csrf}
			f.exec(t, `UPDATE cosmoformer.installation SET clock_unreliable=$1`, latched)
			id := caseID(t, f)
			c.call("PATCH", "/organizations/"+f.org+"/attempts/"+f.assignment.AttemptID+"/stop-proof", recoveryBody(attemptRevision(t, f), "host", "protected-independent-stop", false), 200)
			path := recoveryPath(f, id)
			r := c.call("GET", path, nil, 200)
			r = c.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "host", "protected-independent-stop", true), 200)
			if r["stop_verified"] != true || r["reconciliation_complete"] != true {
				t.Fatal(r)
			}
			var closed bool
			var reservations, holds int
			if err := f.db.Owner.QueryRow(context.Background(), `SELECT closed_at IS NOT NULL,(SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=a.id)+(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=a.id),(SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='recovery' AND owner_id=$2 AND released_at IS NULL) FROM cosmoformer.attempts a WHERE id=$1`, f.assignment.AttemptID, id).Scan(&closed, &reservations, &holds); err != nil || !closed || reservations != 0 || holds == 0 {
				t.Fatal("stop proof did not preserve recovery disposition", closed, reservations, holds, err)
			}
		})
	}
}

func TestMilestone13OriginlessRestoreRecoveryRequiresManualEvidenceAndCurrentVotes(t *testing.T) {
	f := newApprovalFixture(t, 2)
	deployer(t, f.executionFixture)
	id := runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.recovery_cases(id,org_id,stack_id,target_id,status,evidence,opened_at) SELECT $1,org_id,stack_id,target_id,'open','{}',clock_timestamp() FROM cosmoformer.attempts WHERE id=$2`, id, f.assignment.AttemptID)
	path := recoveryPath(f.executionFixture, id)
	r := f.voter.call("GET", path, nil, 200)
	if r["origin_attempt_id"] != "" {
		t.Fatal("restore invented an originating attempt")
	}
	f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "agent", f.assignment.AttemptID, true), 409)
	r = f.voter.call("PATCH", path+"/evidence", recoveryBody(r["revision"], "operator", "external-restore-stop-proof", true), 200)
	vote := map[string]any{"expected_evidence_revision": r["evidence_revision"]}
	f.voter.call("POST", path+"/clear", vote, 409)
	r = f.voter.call("POST", path+"/approvals", vote, 200)
	assertCount(t, r, 1, 2)
	f.voter.call("POST", path+"/clear", vote, 409)
	second := f.second(t, "restore.second")
	r = second.call("POST", path+"/approvals", vote, 200)
	assertCount(t, r, 2, 2)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
	f.voter.call("POST", path+"/clear", vote, 409)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=NULL WHERE role='approver'`)
	r = f.voter.call("POST", path+"/clear", vote, 200)
	if r["status"] != "cleared" {
		t.Fatal("manual restored backend did not clear with current votes")
	}
	var status string
	if err := f.db.Owner.QueryRow(context.Background(), `SELECT status FROM cosmoformer.plans WHERE id=$1`, f.plan["id"]).Scan(&status); err != nil || status != "invalidated" {
		t.Fatal("restore clearance revived an old plan", status, err)
	}
}

func TestMilestone13MaintenanceRetainsOwnershipAndAcceptsAuthenticatedStops(t *testing.T) {
	f := newExecutionFixture(t)
	prepared := f.prepare(t)
	f.exec(t, `UPDATE cosmoformer.installation SET maintenance=true,maintenance_id=gen_random_uuid()`)
	// Permission issuance is blocked even for the still-authenticated owning agent.
	f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", f.message(f.request(t, 1, "init", "")), 409)
	// The same agent can report that it never created a state-capable child.
	f.transition(t, "result", runs.ExecutionResult{Outcome: "preparation_failed", Commands: []runs.CommandResult{}, Artifacts: []string{prepared.Artifact}, PreInitDigest: runs.Digest(f.pre), NoStateChild: true, Reaped: true}, 200)
}
