//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func applyFixture(t *testing.T) *approvalFixture {
	f := newApprovalFixture(t, 1)
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
	return f
}
func (f *approvalFixture) applyPath() string {
	return strings.TrimSuffix(f.path, "/approvals") + "/apply"
}
func (f *approvalFixture) applyBody() map[string]any {
	var env string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT environment_id::text FROM cosmoformer.stacks WHERE id=$1`, f.stack).Scan(&env); e != nil {
		f.voter.t.Fatal(e)
	}
	return map[string]any{"expected_plan_revision": f.plan["revision"], "manifest_digest": f.plan["manifest_digest"], "priority": "medium", "confirmed_context": map[string]any{"stack_id": f.stack, "environment_id": env, "source_commit": f.assignment.Source.Commit}}
}
func TestSavedPlanApplyAdmissionAndReplay(t *testing.T) {
	f := applyFixture(t)
	denied := f.voter.call("POST", f.applyPath(), f.applyBody(), 409)
	if denied["code"] != "APPROVALS_REQUIRED" {
		t.Fatal(denied)
	}
	f.voter.call("POST", f.path, f.body(), 200)
	key := identity.Token()
	first := f.voter.callKey("POST", f.applyPath(), f.applyBody(), 202, key)
	again := f.voter.callKey("POST", f.applyPath(), f.applyBody(), 202, key)
	if first["operation_id"] != again["operation_id"] || first["subject_id"] != f.assignment.RunID {
		t.Fatal("not exact same run/operation", first, again)
	}
	f.voter.call("POST", f.applyPath(), f.applyBody(), 409)
	var ops, holds, reservations int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'),(SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='operation' AND released_at IS NULL),(SELECT count(*) FROM cosmoformer.backend_reservations)`).Scan(&ops, &holds, &reservations); e != nil || ops != 1 || holds != 8 || reservations != 0 {
		t.Fatal(ops, holds, reservations, e)
	}
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='deployer'`)
	f.voter.callKey("POST", f.applyPath(), f.applyBody(), 403, key)
}
func TestSavedPlanApplyConcurrentRequests(t *testing.T) {
	f := applyFixture(t)
	f.voter.call("POST", f.path, f.body(), 200)
	start := make(chan struct{})
	done := make(chan int, 6)
	body := f.applyBody()
	for range 6 {
		go func() {
			<-start
			s, m, _ := rawVote(f.voter, f.applyPath(), body, identity.Token())
			if s != 202 {
				t.Log(m)
			}
			done <- s
		}()
	}
	close(start)
	accepted := 0
	for range 6 {
		status := <-done
		if status == 202 {
			accepted++
		} else if status != 409 {
			t.Fatal(status)
		}
	}
	if accepted == 0 {
		f.voter.call("POST", f.applyPath(), body, 202)
		accepted++
	}
	if accepted != 1 {
		t.Fatal("multiple possible apply operations", accepted)
	}
}
func TestSavedPlanApplyRetirementIsPermanent(t *testing.T) {
	for _, change := range []string{"withdraw", "grant", "membership", "account", "source", "inputs"} {
		t.Run(change, func(t *testing.T) {
			f := applyFixture(t)
			f.voter.call("POST", f.path, f.body(), 200)
			first := f.voter.call("POST", f.applyPath(), f.applyBody(), 202)
			switch change {
			case "withdraw":
				f.withdraw(f.voter)
			case "grant":
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='approver'`)
				f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=NULL WHERE role='approver'`)
			case "membership":
				f.exec(t, `DELETE FROM cosmoformer.team_members WHERE account_id=(SELECT id FROM cosmoformer.accounts WHERE username='admin')`)
			case "account":
				f.exec(t, `UPDATE cosmoformer.accounts SET enabled=false WHERE username='admin'`)
			case "source":
				f.remote.mu.Lock()
				f.remote.commit = strings.Repeat("b", 40)
				f.remote.mu.Unlock()
				f.voter.call("POST", f.path, f.body(), 409)
			case "inputs":
				f.exec(t, `UPDATE cosmoformer.secrets SET semantic_revision=semantic_revision+1`)
			}
			var status string
			var holds int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='operation' AND owner_id=o.id AND released_at IS NULL) FROM cosmoformer.operations o WHERE id=$1`, first["operation_id"]).Scan(&status, &holds); e != nil || status != "retired" || holds != 0 {
				t.Fatal("queued apply survived eligibility loss", status, holds, e)
			}
			if change == "withdraw" || change == "grant" {
				f.voter.call("POST", f.path, f.body(), 200)
				next := f.voter.call("POST", f.applyPath(), f.applyBody(), 202)
				if next["operation_id"] == first["operation_id"] {
					t.Fatal("retired intent resumed")
				}
			}
		})
	}
}
func TestSavedPlanApplySourceAndClockFences(t *testing.T) {
	for _, scenario := range []string{"outage", "changed", "reverse", "clock", "clock read failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := applyFixture(t)
			f.voter.call("POST", f.path, f.body(), 200)
			switch scenario {
			case "outage":
				f.provider.outage = true
				f.voter.call("POST", f.applyPath(), f.applyBody(), 503)
			case "changed":
				f.remote.mu.Lock()
				f.remote.commit = strings.Repeat("b", 40)
				f.remote.mu.Unlock()
				f.voter.call("POST", f.applyPath(), f.applyBody(), 409)
			case "clock read failure":
				f.clock.mu.Lock()
				f.clock.failed = true
				f.clock.mu.Unlock()
				f.voter.call("POST", f.applyPath(), f.applyBody(), 503)
				var latched bool
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT clock_unreliable FROM cosmoformer.installation`).Scan(&latched); e != nil || !latched {
					t.Fatal("failed clock read did not latch", e)
				}
			case "clock":
				f.provider.after = func() { f.clock.mu.Lock(); f.clock.offset += 5 * time.Second; f.clock.mu.Unlock() }
				f.voter.call("POST", f.applyPath(), f.applyBody(), 503)
				var latched bool
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT clock_unreliable FROM cosmoformer.installation`).Scan(&latched); e != nil || !latched {
					t.Fatal("clock latch lost", e)
				}
			case "reverse":
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				f.provider.after = func() { once.Do(func() { close(entered); <-release }) }
				done := make(chan int, 1)
				body := f.applyBody()
				go func() { status, _, _ := rawVote(f.voter, f.applyPath(), body, identity.Token()); done <- status }()
				awaitApprovalSourceRead(t, entered, done)
				f.provider.mu.Lock()
				f.provider.after = nil
				f.provider.mu.Unlock()
				f.remote.mu.Lock()
				f.remote.commit = strings.Repeat("b", 40)
				f.remote.mu.Unlock()
				f.voter.call("POST", f.applyPath(), body, 409)
				close(release)
				if s := <-done; s != 409 {
					t.Fatal(s)
				}
			}
			var n int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'`).Scan(&n); e != nil || n != 0 {
				t.Fatal("rejection created intent", n, e)
			}
		})
	}
}

func TestSavedPlanApplyExpiryWhileWaitingForGate(t *testing.T) {
	f := newApprovalFixture(t, 1, 3*time.Second)
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
	f.voter.call("POST", f.path, f.body(), 200)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.provider.after = func() { once.Do(func() { close(entered); <-release }) }
	done := make(chan int, 1)
	body := f.applyBody()
	go func() { s, _, _ := rawVote(f.voter, f.applyPath(), body, identity.Token()); done <- s }()
	awaitApprovalSourceRead(t, entered, done)
	tx, e := f.db.Owner.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if _, e = tx.Exec(context.Background(), `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	close(release)
	var wait float64
	if e = tx.QueryRow(context.Background(), `SELECT greatest(0,extract(epoch FROM (expires_at-clock_timestamp())))+0.05 FROM cosmoformer.plans WHERE id=$1`, f.plan["id"]).Scan(&wait); e != nil {
		t.Fatal(e)
	}
	time.Sleep(time.Duration(wait * float64(time.Second)))
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	if status := <-done; status != 409 {
		t.Fatal("expired apply accepted", status)
	}
	var n int
	if e = f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'`).Scan(&n); e != nil || n != 0 {
		t.Fatal(n, e)
	}
}
func TestSavedPlanApplyEvidenceAndCleanupRace(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt", "cleanup first", "apply first"} {
		t.Run(scenario, func(t *testing.T) {
			f := applyFixture(t)
			f.voter.call("POST", f.path, f.body(), 200)
			var artifact, key string
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text,object_key FROM cosmoformer.artifacts WHERE plan_id=$1 AND kind='binary_plan'`, f.plan["id"]).Scan(&artifact, &key); e != nil {
				t.Fatal(e)
			}
			switch scenario {
			case "missing":
				f.storage.mu.Lock()
				delete(f.storage.objects, key)
				f.storage.mu.Unlock()
			case "corrupt":
				f.storage.mu.Lock()
				f.storage.objects[key] = "corrupt"
				f.storage.mu.Unlock()
			case "cleanup first":
				f.exec(t, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE artifact_id=$1`, artifact)
				f.exec(t, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'retained',gen_random_uuid(),clock_timestamp())`, f.org, artifact)
			case "apply first":
				f.voter.call("POST", f.applyPath(), f.applyBody(), 202)
				if _, e := f.db.Owner.Exec(context.Background(), `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'retained',gen_random_uuid(),clock_timestamp())`, f.org, artifact); e == nil {
					t.Fatal("cleanup acquired accepted operation evidence")
				}
				return
			}
			f.voter.call("POST", f.applyPath(), f.applyBody(), 409)
			var n int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'`).Scan(&n); e != nil || n != 0 {
				t.Fatal(n, e)
			}
		})
	}
}

func TestSavedPlanApplyClaimRace(t *testing.T) {
	f := applyFixture(t)
	f.voter.call("POST", f.path, f.body(), 200)
	receipt := f.voter.call("POST", f.applyPath(), f.applyBody(), 202)
	f.exec(t, `UPDATE cosmoformer.workers SET capabilities=jsonb_set(capabilities,'{supported_work_phases}','["preflight","execution"]') WHERE id=$1`, f.workerID)
	type response struct {
		assignment *runs.Assignment
		err        error
	}
	done := make(chan response, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			body, _ := json.Marshal(runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20})
			req, _ := http.NewRequest("POST", f.agent.Origin+"/worker/v1/poll", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+f.token)
			req.Header.Set("Content-Type", "application/json")
			res, e := f.agent.HTTP.Do(req)
			if e != nil {
				done <- response{err: e}
				return
			}
			defer res.Body.Close()
			var out struct {
				Assignment *runs.Assignment `json:"assignment"`
			}
			e = json.NewDecoder(res.Body).Decode(&out)
			done <- response{out.Assignment, e}
		}()
	}
	close(start)
	var attempt string
	for range 2 {
		r := <-done
		if r.err != nil || r.assignment == nil || r.assignment.OperationID != receipt["operation_id"] {
			t.Fatal(r.err, r.assignment)
		}
		if attempt != "" && attempt != r.assignment.AttemptID {
			t.Fatal("two attempts claimed", attempt, r.assignment.AttemptID)
		}
		attempt = r.assignment.AttemptID
	}
	var workers, backends, holds int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations),(SELECT count(*) FROM cosmoformer.backend_reservations),(SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='attempt' AND owner_id=$1 AND released_at IS NULL)`, attempt).Scan(&workers, &backends, &holds); e != nil || workers != 1 || backends != 1 || holds != 8 {
		t.Fatal(workers, backends, holds, e)
	}
}
func TestSavedPlanApplyExactConfirmationAndRawBoundary(t *testing.T) {
	f := applyFixture(t)
	f.voter.call("POST", f.path, f.body(), 200)
	for _, field := range []string{"stack_id", "environment_id", "source_commit"} {
		body := f.applyBody()
		value := runs.ID()
		if field == "source_commit" {
			value = strings.Repeat("b", 40)
		}
		body["confirmed_context"].(map[string]any)[field] = value
		got := f.voter.call("POST", f.applyPath(), body, 409)
		if got["code"] != "CONFIRMATION_REQUIRED" {
			t.Fatal(got)
		}
	}
	var binary string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.artifacts WHERE plan_id=$1 AND kind='binary_plan'`, f.plan["id"]).Scan(&binary); e != nil {
		t.Fatal(e)
	}
	f.voter.call("GET", "/organizations/"+f.org+"/artifacts/"+binary+"/content", nil, 403)
}

func TestSavedPlanApplyPRPolicyAndMerge(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "allowed"}[allow], func(t *testing.T) {
			f := approvalFixtureWithExecution(t, newExecutionFixtureForSourcePolicy(t, map[string]any{"kind": "pr", "pr_number": 7}, true, allow, 1))
			f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
			f.voter.call("POST", f.path, f.body(), 200)
			if !allow {
				got := f.voter.call("POST", f.applyPath(), f.applyBody(), 409)
				if got["code"] != "PR_APPLY_DISABLED" {
					t.Fatal(got)
				}
				return
			}
			receipt := f.voter.call("POST", f.applyPath(), f.applyBody(), 202)
			f.remote.mu.Lock()
			f.remote.prState = "MERGED"
			f.remote.mu.Unlock()
			f.voter.call("POST", f.path, f.body(), 409)
			var status string
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT status FROM cosmoformer.operations WHERE id=$1`, receipt["operation_id"]).Scan(&status); e != nil || status != "retired" {
				t.Fatal("merged PR survived", status, e)
			}
			f.voter.call("POST", f.applyPath(), f.applyBody(), 409)
			var count int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'`).Scan(&count); e != nil || count != 1 {
				t.Fatal("merge reused prior plan", count, e)
			}
		})
	}
}
