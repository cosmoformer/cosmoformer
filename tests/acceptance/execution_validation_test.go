//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func rejectExecution(t *testing.T, f *executionFixture, route string, p any, code string) {
	t.Helper()
	status := 409
	if code == "INVALID_REQUEST" {
		status = 422
	}
	raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/"+route, f.message(p), status)
	if !bytes.Contains(raw, []byte(`"code":"`+code+`"`)) {
		t.Fatalf("want %s, got %s", code, raw)
	}
	f.retained(t)
}
func TestExecutionRejectsInvalidPreparationWithoutConsumingSequence(t *testing.T) {
	f := newExecutionFixture(t)
	rejectExecution(t, f, "ack", map[string]any{"work_item_id": runs.ID()}, "INVALID_STATE")
	f.transition(t, "ack", map[string]any{"work_item_id": f.assignment.WorkItemID}, 200)
	rejectExecution(t, f, "prepare-result", runs.ExecutionPrepare{Phase: "execution", Result: "failure"}, "PREPARATION_FAILED_USE_RESULT")
	id, hash := f.artifact(t, "pre_init_spec", f.pre)
	good := runs.ExecutionPrepare{Phase: "execution", Result: "success", Artifact: id, Digest: hash, Commit: f.assignment.Source.Commit, Lock: runs.InputLock{Origin: f.pre.LockOrigin}, SpecDigest: f.assignment.Digest, Bootstrap: f.assignment.Execution.AWS.Bootstrap, Role: f.assignment.Execution.AWS.RoleARN, CredentialMS: 3000000}
	for name, change := range map[string]func(*runs.ExecutionPrepare){
		"wrong assignment":      func(p *runs.ExecutionPrepare) { p.SpecDigest = strings.Repeat("b", 64) },
		"wrong principal":       func(p *runs.ExecutionPrepare) { p.Bootstrap = "arn:aws:iam::111122223333:role/other" },
		"wrong role":            func(p *runs.ExecutionPrepare) { p.Role = "arn:aws:iam::111122223333:role/other" },
		"credential overflow":   func(p *runs.ExecutionPrepare) { p.CredentialMS = 3300001 },
		"unexpected stop claim": func(p *runs.ExecutionPrepare) { p.EverPermitted = true },
	} {
		t.Run(name, func(t *testing.T) {
			p := good
			change(&p)
			rejectExecution(t, f, "prepare-result", p, "REPREPARE_REQUIRED")
		})
	}
	p := good
	p.Lock.Origin = "tracked"
	rejectExecution(t, f, "prepare-result", p, "PROVIDER_LOCK_INVALID")
	p = good
	p.CredentialMS = 1
	rejectExecution(t, f, "prepare-result", p, "CREDENTIAL_BUDGET_REQUIRED")
	raw, _ := json.Marshal(f.pre)
	p = good
	p.Artifact, p.Digest = f.artifact(t, "pre_init_spec", append(raw, '\n'))
	rejectExecution(t, f, "prepare-result", p, "INVALID_MANIFEST")
	p = good
	p.Artifact = runs.ID()
	rejectExecution(t, f, "prepare-result", p, "EVIDENCE_UNAVAILABLE")
	f.transition(t, "prepare-result", good, 200)
	rejectExecution(t, f, "prepare-result", good, "REPREPARE_REQUIRED")
}
func TestExecutionRejectsMalformedChildEvidenceBeforeStateChanges(t *testing.T) {
	f := newExecutionFixture(t)
	f.prepare(t)
	permission := f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200).Permission
	pid := int64(73)
	exit := 0
	empty := true
	spawn := runs.CommandEvent{Ordinal: 1, Permission: permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
	for _, p := range []runs.CommandEvent{{Ordinal: 0, Permission: permission.ID, Event: "spawned"}, {Ordinal: 1, Permission: "", Event: "spawned"}, {Ordinal: 1, Permission: permission.ID, Event: "spawned"}} {
		rejectExecution(t, f, "command-events", p, "INVALID_REQUEST")
	}
	p := spawn
	p.Permission = runs.ID()
	rejectExecution(t, f, "command-events", p, "PERMISSION_REQUIRED")
	p = spawn
	p.Event = "other"
	rejectExecution(t, f, "command-events", p, "COMMAND_UNSUPPORTED")
	p = spawn
	p.Event = "stopped"
	p.Exit = &exit
	p.GroupEmpty = &empty
	p.Pipes = "drained"
	rejectExecution(t, f, "command-events", p, "SPAWN_EVIDENCE_REQUIRED")
	p = runs.CommandEvent{Ordinal: 1, Permission: permission.ID, Event: "not_executed"}
	rejectExecution(t, f, "command-events", p, "NEVER_STARTED_PROOF_REQUIRED")
	f.transition(t, "command-events", spawn, 200)
	// A second observation of identical spawn facts is accepted without replacing them.
	f.transition(t, "command-events", spawn, 200)
	for _, signal := range []int{0, 65} {
		p = spawn
		p.Event = "stopped"
		p.Signal = &signal
		p.GroupEmpty = &empty
		p.Pipes = "drained"
		rejectExecution(t, f, "command-events", p, "INVALID_REQUEST")
	}
	for _, code := range []int{-1, 256} {
		p = spawn
		p.Event = "stopped"
		p.Exit = &code
		p.GroupEmpty = &empty
		p.Pipes = "drained"
		rejectExecution(t, f, "command-events", p, "INVALID_REQUEST")
	}
	p = spawn
	p.Event = "stopped"
	p.Exit = &exit
	p.GroupEmpty = &empty
	p.Pipes = "drained"
	f.transition(t, "command-events", p, 200)
	f.retained(t)
}
func TestExecutionResultRequiresExactAccountingAndFinalLogFacts(t *testing.T) {
	f := newExecutionFixture(t)
	stop := f.stoppedInit(t)
	good := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{{Ordinal: 1, Permission: stop.Permission, Status: "stopped", Exit: stop.Exit, Stop: stop}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	for name, c := range map[string]struct {
		change func(*runs.ExecutionResult)
		code   string
	}{
		"missing commands":         {func(p *runs.ExecutionResult) { p.Commands = nil }, "RESULT_IDENTITY_MISMATCH"},
		"missing artifacts":        {func(p *runs.ExecutionResult) { p.Artifacts = nil }, "RESULT_IDENTITY_MISMATCH"},
		"unknown outcome":          {func(p *runs.ExecutionResult) { p.Outcome = "finished" }, "INVALID_REQUEST"},
		"unaccounted permission":   {func(p *runs.ExecutionResult) { p.Commands = []runs.CommandResult{} }, "PERMISSION_UNACCOUNTED"},
		"unproven never started":   {func(p *runs.ExecutionResult) { p.Outcome = "not_executed" }, "NEVER_STARTED_PROOF_REQUIRED"},
		"unrequested cancellation": {func(p *runs.ExecutionResult) { p.Outcome = "cancelled" }, "CANCEL_EVIDENCE_REQUIRED"},
		"unowned artifact":         {func(p *runs.ExecutionResult) { p.Artifacts = []string{runs.ID()} }, "EVIDENCE_REQUIRED"},
		"wrong log stream":         {func(p *runs.ExecutionResult) { p.LogFinal = &runs.LogFinal{Stream: "private", LastSequence: -1} }, "INVALID_REQUEST"},
		"contradictory final log": {func(p *runs.ExecutionResult) {
			p.LogFinal = &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true, Unknown: true}
		}, "INVALID_REQUEST"},
		"missing final segment": {func(p *runs.ExecutionResult) {
			p.LogFinal = &runs.LogFinal{Stream: "console", LastSequence: 0, Complete: true}
		}, "LOG_FINAL_MISMATCH"},
	} {
		t.Run(name, func(t *testing.T) { p := good; c.change(&p); rejectExecution(t, f, "result", p, c.code) })
	}
	f.transition(t, "result", good, 200)
	var open int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$1`, f.assignment.AttemptID).Scan(&open); e != nil || open != 0 {
		t.Fatal("accounted result did not close exactly once", e, open)
	}
}
func TestExecutionCurrentMetadataChangesRevokeLaunchAuthority(t *testing.T) {
	f := newExecutionFixture(t)
	f.prepare(t)
	request := f.request(t, 1, "init", "")
	var project string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT project_id::text FROM cosmoformer.stacks WHERE id=$1`, f.stack).Scan(&project); e != nil {
		t.Fatal(e)
	}
	for name, c := range map[string]struct {
		set, restore string
		args         []any
		code         string
	}{
		"project revision": {`UPDATE cosmoformer.projects SET revision=revision+1 WHERE id=$1`, `UPDATE cosmoformer.projects SET revision=revision-1 WHERE id=$1`, []any{project}, "INPUTS_CHANGED"},
		"source facts":     {`UPDATE cosmoformer.source_subjects SET facts_revision=facts_revision+1 WHERE id=$1`, `UPDATE cosmoformer.source_subjects SET facts_revision=facts_revision-1 WHERE id=$1`, []any{f.assignment.Source.SubjectID}, "SOURCE_CHANGED"},
		"AWS metadata":     {`UPDATE cosmoformer.aws_connections SET expected_bootstrap_principal='arn:aws:iam::111122223333:role/other' WHERE id=$1`, `UPDATE cosmoformer.aws_connections SET expected_bootstrap_principal=$2 WHERE id=$1`, []any{f.assignment.Execution.AWS.ID, f.assignment.Execution.AWS.Bootstrap}, "INPUTS_CHANGED"},
		"repository URL":   {`UPDATE cosmoformer.repositories SET url='https://bitbucket.org/fictional/changed.git' WHERE id=$1`, `UPDATE cosmoformer.repositories SET url=$2 WHERE id=$1`, []any{f.assignment.Source.RepositoryID, f.assignment.Source.URL}, "INPUTS_CHANGED"},
	} {
		t.Run(name, func(t *testing.T) {
			setArgs := c.args[:1]
			f.exec(t, c.set, setArgs...)
			rejectExecution(t, f, "start-permission", request, c.code)
			f.exec(t, c.restore, c.args...)
		})
	}
	f.transition(t, "start-permission", request, 200)
}
