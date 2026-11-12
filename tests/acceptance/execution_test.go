//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

type executionFixture struct {
	*preflightFixture
	assignment runs.Assignment
	pre        runs.PreInitSpec
	seq        int64
	secret     string
}

// Protocol-focused tests install synthetic execution claims with real foreign
// keys, reservations and custody. The separate planning journeys use actual
// admission and scheduling with injected native/AWS I/O.
func newExecutionFixture(t *testing.T) *executionFixture {
	t.Helper()
	return newExecutionFixtureForSource(t, map[string]any{"kind": "branch"})
}
func newExecutionFixtureForSource(t *testing.T, source map[string]any) *executionFixture {
	return newExecutionFixtureForSourceProfile(t, source, false)
}
func newExecutionFixtureForSourceProfile(t *testing.T, source map[string]any, review bool, minimum ...int) *executionFixture {
	return newExecutionFixtureForSourcePolicy(t, source, review, false, minimum...)
}
func newExecutionFixtureForSourcePolicy(t *testing.T, source map[string]any, review, allowPR bool, minimum ...int) *executionFixture {
	t.Helper()
	var ticks *testClock
	if len(minimum) > 0 {
		ticks = &testClock{tick: time.Second, start: time.Now()}
	}
	f := newPreflightFixtureForSourcePolicyClock(t, syntheticNativeEvidence{}, source, ticks, allowPR)
	ctx := context.Background()
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	f.agent.HTTP.Transport = f.wire.base
	f.exec(t, `UPDATE cosmoformer.stacks SET assessment='compatible' WHERE id=$1`, f.stack)
	var oldSpec, prepared []byte
	var operation, run, instance string
	if e := f.db.Owner.QueryRow(ctx, `SELECT wi.assignment_spec,wi.resolved_preflight,a.operation_id::text,o.run_id::text,a.budget_platform_instance::text FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id JOIN cosmoformer.work_items wi ON wi.id=a.work_item_id WHERE a.stack_id=$1`, f.stack).Scan(&oldSpec, &prepared, &operation, &run, &instance); e != nil {
		t.Fatal(e)
	}
	var p runs.PreflightSpec
	var result runs.Prepare
	if runs.Decode(oldSpec, &p) != nil || runs.Decode(prepared, &result) != nil {
		t.Fatal("source fixture did not prepare")
	}
	var project string
	if e := f.db.Owner.QueryRow(ctx, `SELECT project_id::text FROM cosmoformer.stacks WHERE id=$1`, f.stack).Scan(&project); e != nil {
		t.Fatal(e)
	}
	floor := 1
	if len(minimum) > 0 {
		floor = minimum[0]
		p.Review.Minimum = max(p.Review.Minimum, floor)
		if len(minimum) > 1 {
			p.Review.Minimum = max(p.Review.Minimum, minimum[1])
		}
		f.exec(t, `UPDATE cosmoformer.projects SET minimum_approvals=$2,revision=revision+1 WHERE id=$1`, project, floor)
	}
	secretValue := `{"credential":"synthetic-native-value-74","number":92713,"flag":true}`
	secret := id(f.admin.call("POST", "/organizations/"+f.org+"/secrets", map[string]any{"project_id": project, "name": "native input", "value": secretValue}, 200))
	ref := runs.SecretRef{ID: secret, Scope: "project", Revision: 1, Destination: "TF_VAR_credential"}
	grant := id(f.admin.call("POST", "/organizations/"+f.org+"/resource-grants", map[string]any{"project_id": project, "kind": "secret", "resource_id": secret}, 200))
	if review {
		p.RuleVersion = plan.RuleVersion
		p.ProfileID = plan.ProfileID("opentofu")
	}
	p.Secrets = []runs.SecretRef{ref}
	p.Grants = append(p.Grants, runs.ResourceRef{ID: grant, Kind: "secret", ResourceID: secret, Revision: 1})
	f.exec(t, `UPDATE cosmoformer.aws_connections SET expected_bootstrap_principal='arn:aws:iam::111122223333:role/synthetic-bootstrap',session_seconds=3600,revision=revision+1 WHERE id=$1`, p.AWS.ID)
	p.AWS.Revision++
	tx, e := f.db.App.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, f.org); e != nil {
		t.Fatal(e)
	}
	spec, e := coordinator.ResolveExecutionSpec(ctx, tx, p, result.Backend)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(spec)
	a := runs.Assignment{AttemptID: runs.ID(), OperationID: operation, WorkItemID: runs.ID(), Kind: "plan", Phase: "execution", RunID: run, StackID: f.stack, Source: p.Source, StackRevisionID: p.StackRevisionID, RuntimeID: p.Engine.ID, CapSeconds: 1200, Execution: &spec, Spec: spec.Preflight(), Digest: runs.Digest(spec), RemainingMS: 1200000, Platform: instance, NextSeq: 1}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.work_items(id,org_id,operation_id,phase,status,assignment_spec,attempt_count) VALUES($1,$2,$3,'execution','claimed',$4,1)`, a.WorkItemID, f.org, operation, raw); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.attempts(id,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,execution_spec,execution_spec_digest) SELECT $1,$2,$3,$4,$5,target_id,$6,$7,$8,'claimed',$9,$10,$11,1200,clock_timestamp()+interval '1200 seconds',$12,decode($13,'hex') FROM cosmoformer.stack_bindings WHERE stack_id=$5`, a.AttemptID, f.org, a.WorkItemID, operation, f.stack, f.workerID, f.agent.Identity.Generation, f.agent.Identity.Agent, instance, int64(time.Second), int64(1201*time.Second), raw, a.Digest); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) VALUES($1,$2,$3,$4,$5)`, f.org, f.workerID, a.AttemptID, f.agent.Identity.Generation, f.agent.Identity.Agent); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, a.AttemptID); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='preparing',terminal_reason=NULL WHERE id=$1`, operation); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.plans(org_id,operation_id,attempt_id,stack_id,target_id,status,provenance,source_facts_revision,state_epoch,rule_version,profile_id) SELECT org_id,operation_id,id,stack_id,target_id,'assembling','normal',$2,0,$3,$4 FROM cosmoformer.attempts WHERE id=$1`, a.AttemptID, spec.Source.FactsRevision, spec.RuleVersion, spec.ProfileID); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	b := spec.Backend
	nativeSource := []byte("variable \"credential\" {\n type=any\n sensitive=true\n}\n")
	nativeSourceHash := sha256.Sum256(nativeSource)
	reviewed := runs.ReviewedInputs{Version: 1, Stack: f.stack, StackRevision: spec.StackRevisionID, Subject: spec.Source.SubjectID, Facts: spec.Source.FactsRevision, Commit: spec.Source.Commit, Engine: runs.ManifestEngine{Name: spec.Engine.Name, Version: spec.Engine.Version, Arch: spec.Engine.Arch, Image: spec.Engine.ImageDigest, Git: "1:2.47.3-0+deb13u1"}, Backend: runs.ManifestBackend{Partition: b.Target.Partition, Bucket: b.Target.Bucket, Key: b.Target.Key, Region: b.Region, Owner: b.ExpectedOwner, Workspace: spec.Workspace, Profile: b.Profile, Table: b.TableARN, AttributesDigest: runs.Digest(b.Attributes)}, AWS: runs.ManifestAWS{ID: spec.AWS.ID, Revision: spec.AWS.Revision, Role: spec.AWS.RoleARN}, Pool: spec.Pool, Files: []runs.ManifestFile{{Repository: spec.Source.RepositoryID, Commit: spec.Source.Commit, Path: "main.tf", Kind: "regular", Size: int64(len(nativeSource)), Hash: hex.EncodeToString(nativeSourceHash[:])}}, Modules: []runs.ManifestModule{}, VarFiles: []runs.ManifestInputFile{}, BackendFiles: []runs.ManifestInputFile{}, Secrets: spec.Secrets, Grants: spec.Grants, Approval: runs.ApprovalPolicy{Floor: floor, Requested: p.Review.Minimum, Minimum: p.Review.Minimum, Revision: spec.Review.Revision}, AllowPR: spec.Review.AllowPRApply, Credentials: spec.SourceCredentials, Rules: spec.RuleVersion, Profile: spec.ProfileID, Epoch: 0, Provenance: runs.Provenance{Kind: "normal"}}
	pre := runs.PreInitSpec{ReviewedInputs: reviewed, Kind: "plan", SpecDigest: a.Digest, LockOrigin: "absent"}
	return &executionFixture{preflightFixture: f, assignment: a, pre: pre, seq: 1, secret: secretValue}
}
func (f *executionFixture) message(payload any) runs.Message {
	b, _ := json.Marshal(payload)
	return runs.Message{Protocol: 1, Identity: f.agent.Identity, AttemptID: f.assignment.AttemptID, MessageID: runs.ID(), Seq: f.seq, Payload: b}
}
func (f *executionFixture) transition(t *testing.T, route string, p any, status int) runs.Transition {
	t.Helper()
	if result, ok := p.(runs.ExecutionResult); ok && result.LogFinal == nil {
		result.LogFinal = &runs.LogFinal{Stream: "console", LastSequence: -1, Unknown: true}
		p = result
	}
	raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/"+route, f.message(p), status)
	var out runs.Transition
	if status == 200 {
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		delete(fields, "$schema")
		raw, _ = json.Marshal(fields)
		if e := json.Unmarshal(raw, &out); e != nil {
			t.Fatal(e)
		}
		f.seq++
	}
	return out
}
func (f *executionFixture) artifact(t *testing.T, kind string, value any) (string, string) {
	t.Helper()
	var raw []byte
	if b, ok := value.([]byte); ok {
		raw = b
	} else {
		var e error
		raw, e = catalog.Canonical(value)
		if e != nil {
			t.Fatal(e)
		}
	}
	h := sha256.Sum256(raw)
	hash := hex.EncodeToString(h[:])
	upload := runs.ID()
	req, e := http.NewRequest("PUT", f.agent.Origin+"/worker/v1/attempts/"+f.assignment.AttemptID+"/artifacts/"+upload, bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-Worker-ID", f.workerID)
	req.Header.Set("X-Worker-Generation", "1")
	req.Header.Set("X-Agent-Instance", f.agent.Identity.Agent)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Artifact-Kind", kind)
	req.Header.Set("Expected-SHA256", hash)
	response, e := f.agent.HTTP.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("upload %s: %d %s", kind, response.StatusCode, b)
	}
	var receipt evidence.Artifact
	if json.Unmarshal(b, &receipt) != nil {
		t.Fatal("artifact receipt")
	}
	f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/artifacts/"+upload+"/finalize", map[string]any{"request_id": runs.ID(), "kind": kind, "sha256": hash, "size": len(raw)}, 200)
	return receipt.ID, hash
}
func (f *executionFixture) prepare(t *testing.T) runs.ExecutionPrepare {
	t.Helper()
	f.transition(t, "ack", map[string]any{"work_item_id": f.assignment.WorkItemID}, 200)
	id, hash := f.artifact(t, "pre_init_spec", f.pre)
	p := runs.ExecutionPrepare{Phase: "execution", Result: "success", Artifact: id, Digest: hash, Commit: f.assignment.Source.Commit, Lock: runs.InputLock{Origin: f.pre.LockOrigin, Present: f.pre.LockOrigin != "absent", Hash: f.pre.LockHash}, SpecDigest: f.assignment.Digest, Bootstrap: f.assignment.Execution.AWS.Bootstrap, Role: f.assignment.Execution.AWS.RoleARN, CredentialMS: 3000000}
	f.transition(t, "prepare-result", p, 200)
	return p
}
func (f *executionFixture) request(t *testing.T, ordinal int64, command, manifest string) runs.CommandRequest {
	t.Helper()
	r := runs.CommandRequest{Ordinal: ordinal, Command: command, PreInitDigest: runs.Digest(f.pre), ManifestDigest: manifest}
	d, e := runs.Command(*f.assignment.Execution, "plan", r)
	if e != nil {
		t.Fatal(e)
	}
	r.Digest = runs.Digest(d)
	return r
}
func (f *executionFixture) stoppedInit(t *testing.T) runs.CommandEvent {
	t.Helper()
	f.prepare(t)
	out := f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200)
	if out.Permission == nil {
		t.Fatal("missing permit")
	}
	pid := int64(1234)
	exit := 0
	empty := true
	event := runs.CommandEvent{Ordinal: 1, Permission: out.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
	f.transition(t, "command-events", event, 200)
	event.Event = "stopped"
	event.Exit = &exit
	event.GroupEmpty = &empty
	event.Pipes = "drained"
	f.transition(t, "command-events", event, 200)
	return event
}
func (f *executionFixture) seal(t *testing.T, providers []runs.Provider, lock []byte) runs.ManifestSeal {
	t.Helper()
	manifest := runs.Manifest{ReviewedInputs: f.pre.ReviewedInputs, Providers: providers}
	lockID := ""
	if lock != nil {
		var hash string
		lockID, hash = f.artifact(t, "provider_lock", lock)
		manifest.Lock = runs.ProviderLock{Present: true, Hash: hash, Size: int64(len(lock))}
	}
	id, hash := f.artifact(t, "manifest", manifest)
	seal := runs.ManifestSeal{PreInitDigest: runs.Digest(f.pre), Artifact: id, Digest: hash, LockArtifact: lockID, Providers: providers}
	f.transition(t, "manifest-seal", seal, 200)
	return seal
}
func (f *executionFixture) retained(t *testing.T) {
	t.Helper()
	var workers, backends, open int
	e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.attempts WHERE id=$1 AND closed_at IS NULL)`, f.assignment.AttemptID).Scan(&workers, &backends, &open)
	if e != nil || workers != 1 || backends != 1 || open != 1 {
		t.Fatal("ownership was lost", workers, backends, open, e)
	}
}
func TestExecutionSecretDeliveryAndAuthority(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	registry := &evidence.Registry{}
	values, e := f.agent.FetchExecutionInputs(ctx, f.assignment, registry)
	if e != nil || len(values) != 1 || values[0].Value != f.secret {
		t.Fatal("protected input delivery", e)
	}
	// The future readiness boundary must validate independently of this worker's
	// registry, including malicious message arguments and canonical JSON leaves.
	for _, leak := range []string{`{"findings":[{"arguments":{"text":"synthetic-native-value-74"}}]}`, `{"number":9.2713e4}`, `{"address_display":"resource[\"synthetic-native-value-74\"]"}`} {
		if plan.ValidateManagedSecrets([]byte(leak), values) != plan.ErrLeak {
			t.Fatal("platform accepted leaking normalized evidence")
		}
	}
	if plan.ValidateManagedSecrets([]byte(`{"state":"hidden"}`), values) != nil {
		t.Fatal("platform rejected redacted evidence")
	}
	pipe := registry.Pipe()
	masked := string(pipe.Feed([]byte("synthetic-native-value-74 92713 true "+f.secret), true))
	if strings.Contains(masked, "synthetic-native-value-74") || strings.Contains(masked, "92713") || strings.Contains(masked, "true") {
		t.Fatal("secret leaf was not registered")
	}
	in := runs.ExecutionInputs{Identity: f.agent.Identity, RequestID: runs.ID(), SpecDigest: f.assignment.Digest, Secrets: f.assignment.Execution.ExpectedSecrets()}
	target := "/attempts/" + f.assignment.AttemptID + "/execution-inputs"
	f.workerCall(t, target, in, 200)
	in.SpecDigest = strings.Repeat("b", 64)
	f.workerCall(t, target, in, 409)
	in.SpecDigest = f.assignment.Digest
	in.Generation++
	f.workerCall(t, target, in, 409)
	in.Generation--
	in.Agent = runs.ID()
	f.workerCall(t, target, in, 409)
	in.Agent = f.agent.Identity.Agent
	// Current authority gates redelivery even for the same request ID.
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='planner'`)
	f.workerCall(t, target, in, 403)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=NULL WHERE role='planner'`)
	f.exec(t, `UPDATE cosmoformer.secrets SET semantic_revision=semantic_revision+1 WHERE id=$1`, in.Secrets[0].ID)
	f.workerCall(t, target, in, 409)
	var stored string
	if e = f.db.Owner.QueryRow(ctx, `SELECT coalesce((SELECT string_agg(payload::text,'') FROM cosmoformer.attempt_messages),'')||coalesce((SELECT string_agg(response::text,'') FROM cosmoformer.worker_requests),'')||coalesce((SELECT string_agg(execution_spec::text,'') FROM cosmoformer.attempts),'')||coalesce((SELECT string_agg(row_to_json(a)::text,'') FROM cosmoformer.audit_events a),'')`).Scan(&stored); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(stored, f.secret) || strings.Contains(stored, "synthetic-native-value-74") {
		t.Fatal("plaintext persisted")
	}
	f.retained(t)
}
func TestExecutionPrepareSealAndOrderedCommands(t *testing.T) {
	f := newExecutionFixture(t)
	// Rejected gaps leave no accepted digest.
	gap := f.message(map[string]any{"work_item_id": f.assignment.WorkItemID})
	gap.Seq = 3
	raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/ack", gap, 409)
	if !bytes.Contains(raw, []byte("EXPECTED_SEQUENCE")) {
		t.Fatal(string(raw))
	}
	stop := f.stoppedInit(t)
	seal := f.seal(t, []runs.Provider{}, nil)
	f.transition(t, "manifest-seal", seal, 200)
	// Different exact artifact identity cannot replace the original seal.
	altered := seal
	altered.Artifact = runs.ID()
	f.transition(t, "manifest-seal", altered, 409)
	schema := f.request(t, 2, "schema", seal.Digest)
	out := f.transition(t, "start-permission", schema, 200)
	if out.Permission == nil || out.Permission.RemainingMS <= 0 || out.Permission.RemainingMS > 9900 {
		t.Fatal("invalid permission", out)
	}
	// No command can close the attempt while this issued permission is unaccounted.
	f.transition(t, "result", runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{{Ordinal: 1, Permission: stop.Permission, Status: "stopped", Exit: stop.Exit, Stop: stop}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), ManifestDigest: seal.Digest}, 409)
	f.retained(t)
}
func TestExecutionPermissionResponseLossAndExpiry(t *testing.T) {
	f := newExecutionFixture(t)
	f.prepare(t)
	req := f.request(t, 1, "init", "")
	message := f.message(req)
	requestBody, _ := json.Marshal(message)
	httpRequest, e := http.NewRequest("POST", f.agent.Origin+"/worker/v1/attempts/"+f.assignment.AttemptID+"/start-permission", bytes.NewReader(requestBody))
	if e != nil {
		t.Fatal(e)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+f.token)
	httpRequest.Header.Set("Content-Type", "application/json")
	client := *f.agent.HTTP
	client.Transport = lostPermissionResponse{base: f.wire.base}
	if _, e = client.Do(httpRequest); e == nil {
		t.Fatal("response loss was not exercised")
	}
	// Inspect committed identity without having delivered the response to the
	// worker; replay must recover this same permission rather than create one.
	first := runs.Transition{Permission: &runs.Permission{}}
	if e = f.db.Owner.QueryRow(context.Background(), `SELECT id::text,display_expires_at,ordinal,encode(command_digest,'hex'),platform_instance::text,expires_tick FROM cosmoformer.start_permissions WHERE attempt_id=$1`, f.assignment.AttemptID).Scan(&first.Permission.ID, &first.Permission.Expires, &first.Permission.Ordinal, &first.Permission.Digest, &first.Permission.Platform, &first.Permission.RemainingMS); e != nil {
		t.Fatal(e)
	}
	first.Permission.RemainingMS = (first.Permission.RemainingMS - int64(time.Second)) / int64(time.Millisecond)
	f.ticks.mu.Lock()
	f.ticks.tick += 2 * time.Second
	f.ticks.mu.Unlock()
	replay := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", message, 200)
	var second runs.Transition
	json.Unmarshal(replay, &second)
	f.seq++
	if second.Permission.ID != first.Permission.ID || !second.Permission.Expires.Equal(first.Permission.Expires) || second.Permission.RemainingMS >= first.Permission.RemainingMS {
		t.Fatal("permit replay renewed authority")
	}
	f.ticks.mu.Lock()
	f.ticks.tick += 10 * time.Second
	f.ticks.mu.Unlock()
	expired := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", message, 409)
	if !bytes.Contains(expired, []byte("PERMISSION_EXPIRED")) {
		t.Fatal(string(expired))
	}
	f.retained(t)
	event := runs.CommandEvent{Ordinal: 1, Permission: first.Permission.ID, Event: "not_executed", ProofReason: "local_deadline_expired_discarded"}
	f.transition(t, "command-events", event, 200)
	f.transition(t, "result", runs.ExecutionResult{Outcome: "not_executed", Commands: []runs.CommandResult{{Ordinal: 1, Permission: event.Permission, Status: "not_executed", Stop: event}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre)}, 200)
	var open int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$1`, f.assignment.AttemptID).Scan(&open); e != nil || open != 0 {
		t.Fatal("positive not-executed did not release", e)
	}
}
func TestExecutionStopUncertaintyAndConflictingReplay(t *testing.T) {
	for _, scenario := range []string{"conflicting message", "surviving group", "unknown result"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionFixture(t)
			f.prepare(t)
			message := f.message(f.request(t, 1, "init", ""))
			raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", message, 200)
			f.seq++
			var permit runs.Transition
			json.Unmarshal(raw, &permit)
			if permit.Permission == nil {
				t.Fatal("permit")
			}
			switch scenario {
			case "conflicting message":
				changed := f.request(t, 1, "init", "")
				changed.Digest = strings.Repeat("b", 64)
				message.Payload, _ = json.Marshal(changed)
				f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", message, 409)
			case "surviving group":
				pid := int64(1234)
				f.transition(t, "command-events", runs.CommandEvent{Ordinal: 1, Permission: permit.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}, 200)
				exit := 0
				empty := false
				f.transition(t, "command-events", runs.CommandEvent{Ordinal: 1, Permission: permit.Permission.ID, Event: "stopped", PID: &pid, PGID: &pid, Exit: &exit, GroupEmpty: &empty, Pipes: "drained"}, 409)
			case "unknown result":
				f.transition(t, "result", runs.ExecutionResult{Outcome: "unknown", Commands: []runs.CommandResult{}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre)}, 409)
			}
			f.retained(t)
			var state string
			var holds int
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT state,(SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=$1 AND status='open') FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID).Scan(&state, &holds); e != nil || state != "uncertain" || holds != 1 {
				t.Fatal("uncertain execution not fenced", state, holds, e)
			}
		})
	}
}

// This injected boundary validates only the exact synthetic native fixture. No
// production profile is enabled by supplying these test bytes.
type syntheticNativeEvidence struct{}

func (syntheticNativeEvidence) VerifySchema(_ context.Context, _ runs.ExecutionSpec, m runs.Manifest, b []byte) error {
	var schema struct {
		Format    string                     `json:"format_version"`
		Providers map[string]json.RawMessage `json:"provider_schemas"`
	}
	if runs.Decode(b, &schema) != nil || schema.Format != "1.0" || schema.Providers == nil || len(schema.Providers) != len(m.Providers) {
		return runs.Fail("UNSUPPORTED_SCHEMA")
	}
	for _, p := range m.Providers {
		if len(schema.Providers[p.Address]) == 0 {
			return runs.Fail("UNSUPPORTED_SCHEMA")
		}
	}
	return nil
}
func (f *executionFixture) commandStopped(t *testing.T, permission *runs.Permission, exit int) runs.CommandEvent {
	t.Helper()
	pid := int64(1200 + permission.Ordinal)
	empty := true
	event := runs.CommandEvent{Ordinal: permission.Ordinal, Permission: permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
	f.transition(t, "command-events", event, 200)
	event.Event = "stopped"
	event.Exit = &exit
	event.GroupEmpty = &empty
	event.Pipes = "drained"
	f.transition(t, "command-events", event, 200)
	return event
}
func TestExecutionCommandEvidenceAndFullOrder(t *testing.T) {
	f := newExecutionFixture(t)
	init := f.stoppedInit(t)
	seal := f.seal(t, []runs.Provider{}, nil)
	schema := f.transition(t, "start-permission", f.request(t, 2, "schema", seal.Digest), 200)
	stoppedSchema := f.commandStopped(t, schema.Permission, 0)
	request := f.request(t, 3, "plan", seal.Digest)
	f.transition(t, "start-permission", request, 409)
	schemaID, _ := f.artifact(t, "provider_schema", []byte(`{"format_version":"1.0","provider_schemas":{}}`))
	plan := f.transition(t, "start-permission", request, 200)
	stoppedPlan := f.commandStopped(t, plan.Permission, 2)
	showRequest := f.request(t, 4, "show", seal.Digest)
	f.transition(t, "start-permission", showRequest, 409)
	binaryID, _ := f.artifact(t, "binary_plan", []byte("synthetic opaque protected plan bytes"))
	show := f.transition(t, "start-permission", showRequest, 200)
	stoppedShow := f.commandStopped(t, show.Permission, 0)
	commands := []runs.CommandResult{}
	for _, p := range []runs.CommandEvent{init, stoppedSchema, stoppedPlan, stoppedShow} {
		commands = append(commands, runs.CommandResult{Ordinal: p.Ordinal, Permission: p.Permission, Status: "stopped", Exit: p.Exit, Stop: p})
	}
	result := runs.ExecutionResult{Outcome: "success", Commands: commands, Artifacts: []string{schemaID, binaryID}, PreInitDigest: runs.Digest(f.pre), ManifestDigest: seal.Digest, LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	// Command stop facts alone cannot substitute for the complete retained
	// evidence required by the planning pipeline.
	f.transition(t, "result", result, 409)
	result.Outcome = "failure"
	f.transition(t, "result", result, 200)
	var reservations, ready int
	e := f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.plans WHERE attempt_id=$1 AND status='ready')`, f.assignment.AttemptID).Scan(&reservations, &ready)
	if e != nil || reservations != 0 || ready != 0 {
		t.Fatal("finalization fabricated readiness or retained accounted ownership", e)
	}
}

func TestExecutionCurrentGuardsAndRestart(t *testing.T) {
	f := newExecutionFixture(t)
	f.prepare(t)
	request := f.request(t, 1, "init", "")
	secret := f.assignment.Execution.Secrets[0]
	f.exec(t, `UPDATE cosmoformer.secrets SET enabled=false WHERE id=$1`, secret.ID)
	f.transition(t, "start-permission", request, 409)
	f.exec(t, `UPDATE cosmoformer.secrets SET enabled=true WHERE id=$1`, secret.ID)
	f.exec(t, `UPDATE cosmoformer.operations SET cancel_requested=true WHERE id=$1`, f.assignment.OperationID)
	f.transition(t, "start-permission", request, 409)
	f.exec(t, `UPDATE cosmoformer.operations SET cancel_requested=false WHERE id=$1`, f.assignment.OperationID)
	f.exec(t, `DELETE FROM cosmoformer.backend_reservations WHERE attempt_id=$1`, f.assignment.AttemptID)
	f.transition(t, "start-permission", request, 409)
	f.exec(t, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID)
	message := f.message(request)
	raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", message, 200)
	f.seq++
	var permit runs.Transition
	if json.Unmarshal(raw, &permit) != nil || permit.Permission == nil {
		t.Fatal("permission response")
	}
	restarted := coordinator.New(f.db.App, f.ticks)
	_, e := restarted.Transition(context.Background(), f.token, "start-permission", message)
	if runs.Code(e) != "PERMISSION_INSTANCE_ENDED" {
		t.Fatal("platform restart renewed old permission", e)
	}
	f.retained(t)
	// Revoking the worker token cannot account for its issued permission.
	f.exec(t, `UPDATE cosmoformer.workers SET verifier=$2 WHERE id=$1`, f.workerID, identity.Digest(identity.Token()))
	f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/command-events", f.message(runs.CommandEvent{Ordinal: 1, Permission: permit.Permission.ID, Event: "not_executed", ProofReason: "local_deadline_expired_discarded"}), 401)
	f.retained(t)
}
func TestExecutionClockFailureRetainsOwnership(t *testing.T) {
	for _, kind := range []string{"elapsed backward", "database backward"} {
		t.Run(kind, func(t *testing.T) {
			f := newExecutionFixture(t)
			f.prepare(t)
			if kind == "elapsed backward" {
				// Establish the process's clock observation without issuing a permission.
				f.transition(t, "start-permission", runs.CommandRequest{Ordinal: 1, Command: "init", PreInitDigest: runs.Digest(f.pre), Digest: strings.Repeat("b", 64)}, 409)
				f.ticks.mu.Lock()
				f.ticks.tick = 0
				f.ticks.mu.Unlock()
			} else {
				f.exec(t, `UPDATE cosmoformer.installation SET authority_highwater=clock_timestamp()+interval '2 seconds'`)
			}
			raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", f.message(f.request(t, 1, "init", "")), 409)
			if !bytes.Contains(raw, []byte("CLOCK_UNRELIABLE")) {
				t.Fatal(string(raw))
			}
			var unreliable bool
			if e := f.db.Owner.QueryRow(context.Background(), `SELECT clock_unreliable FROM cosmoformer.installation`).Scan(&unreliable); e != nil || !unreliable {
				t.Fatal("clock failure was rolled back", e)
			}
			f.retained(t)
		})
	}
}
func TestExecutionNativeLocksAndImmutablePreparation(t *testing.T) {
	for _, origin := range []string{"tracked", "generated", "empty"} {
		t.Run(origin, func(t *testing.T) {
			f := newExecutionFixture(t)
			lock := []byte("# retained native representation\nprovider \"registry.opentofu.org/hashicorp/null\" {\n version=\"3.2.4\"\n hashes=[\"zh:" + strings.Repeat("b", 64) + "\"]\n}\n")
			providers := []runs.Provider{{Address: "registry.opentofu.org/hashicorp/null", Version: "3.2.4", Platform: "linux_amd64", Hashes: []string{"zh:" + strings.Repeat("b", 64)}}}
			if origin == "empty" {
				lock = nil
				providers = []runs.Provider{}
			}
			if origin == "tracked" {
				h := sha256.Sum256(lock)
				hash := hex.EncodeToString(h[:])
				f.pre.LockOrigin = "tracked"
				f.pre.LockHash = hash
				f.pre.Files = append([]runs.ManifestFile{{Repository: f.assignment.Source.RepositoryID, Commit: f.assignment.Source.Commit, Path: ".terraform.lock.hcl", Kind: "regular", Size: int64(len(lock)), Hash: hash}}, f.pre.Files...)
			}
			f.stoppedInit(t)
			seal := f.seal(t, providers, lock)
			f.transition(t, "manifest-seal", seal, 200)
			// Runtime DML cannot replace either attestation identity after binding.
			if _, e := f.db.Owner.Exec(context.Background(), `UPDATE cosmoformer.attempts SET pre_init_spec_digest=decode($2,'hex') WHERE id=$1`, f.assignment.AttemptID, strings.Repeat("f", 64)); e == nil {
				t.Fatal("pre-init identity was mutable")
			}
			if _, e := f.db.Owner.Exec(context.Background(), `UPDATE cosmoformer.attempts SET manifest_stage='pre_init' WHERE id=$1`, f.assignment.AttemptID); e == nil {
				t.Fatal("manifest unsealed")
			}
			altered := runs.Manifest{ReviewedInputs: f.pre.ReviewedInputs, Providers: providers}
			if lock != nil {
				h := sha256.Sum256(lock)
				altered.Lock = runs.ProviderLock{Present: true, Hash: hex.EncodeToString(h[:]), Size: int64(len(lock))}
			}
			altered.Files = append([]runs.ManifestFile{}, altered.Files...)
			altered.Files[len(altered.Files)-1].Hash = strings.Repeat("f", 64)
			id, hash := f.artifact(t, "manifest", altered)
			changed := seal
			changed.Artifact = id
			changed.Digest = hash
			f.transition(t, "manifest-seal", changed, 409)
			f.retained(t)
		})
	}
}
func TestExecutionRedactionCapacityFailsClosed(t *testing.T) {
	f := newExecutionFixture(t)
	registry := &evidence.Registry{}
	terms := make([]string, 4096)
	for i := range terms {
		terms[i] = fmt.Sprintf("registered-fixture-%d", i)
	}
	if e := registry.Register(terms...); e != nil {
		t.Fatal(e)
	}
	values, e := f.agent.FetchExecutionInputs(context.Background(), f.assignment, registry)
	if e == nil || values != nil || strings.Contains(e.Error(), f.secret) {
		t.Fatal("registry exhaustion released secret inputs")
	}
	f.retained(t)
}

func TestExecutionEvidenceIOAndPreparationRetry(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	// Exact-version storage verification must occur with no coordination lock or
	// database transaction held by the route doing the external read.
	f.storage.mu.Lock()
	f.storage.onGet = func() error {
		tx, e := f.db.App.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		_, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE NOWAIT`)
		return e
	}
	f.storage.mu.Unlock()
	prepared := f.prepare(t)
	old := f.assignment.AttemptID
	f.transition(t, "result", runs.ExecutionResult{Outcome: "preparation_failed", Commands: []runs.CommandResult{}, Artifacts: []string{prepared.Artifact}, PreInitDigest: prepared.Digest, NoStateChild: true, Reaped: true}, 200)
	op, status, revision := f.operation(t)
	if status != "failed" {
		t.Fatal(status)
	}
	f.admin.call("POST", "/organizations/"+f.org+"/operations/"+op+"/retry", map[string]any{"expected_revision": revision}, 200)
	var phase string
	if e := f.db.Owner.QueryRow(ctx, `SELECT phase FROM cosmoformer.work_items WHERE operation_id=$1 AND status='queued'`, op).Scan(&phase); e != nil || phase != "execution" {
		t.Fatal("retry requeued the wrong phase", phase, e)
	}
	// Claiming execution is injected at this boundary: the production scheduler
	// intentionally cannot claim the queued execution item.
	next := runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.attempts(id,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,execution_spec,execution_spec_digest) SELECT $2,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,'claimed',budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,execution_spec,execution_spec_digest FROM cosmoformer.attempts WHERE id=$1`, old, next)
	f.exec(t, `UPDATE cosmoformer.work_items SET status='claimed',attempt_count=attempt_count+1 WHERE id=$1`, f.assignment.WorkItemID)
	f.exec(t, `UPDATE cosmoformer.operations SET status='preparing' WHERE id=$1`, op)
	f.exec(t, `UPDATE cosmoformer.plans SET attempt_id=$2 WHERE attempt_id=$1 AND status='assembling'`, old, next)
	f.exec(t, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) SELECT org_id,worker_id,id,generation,agent_instance FROM cosmoformer.attempts WHERE id=$1`, next)
	f.exec(t, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, next)
	f.assignment.AttemptID = next
	f.seq = 1
	f.transition(t, "ack", map[string]any{"work_item_id": f.assignment.WorkItemID}, 200)
	f.transition(t, "prepare-result", prepared, 409)
	prepared.Artifact, prepared.Digest = f.artifact(t, "pre_init_spec", f.pre)
	f.transition(t, "prepare-result", prepared, 200)
	var retained int
	if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.artifacts WHERE attempt_id=$1 AND state='finalized' AND kind='pre_init_spec'`, old).Scan(&retained); e != nil || retained != 1 {
		t.Fatal("retry replaced historical evidence", e)
	}
	f.retained(t)
}

type lostPermissionResponse struct{ base http.RoundTripper }

func (l lostPermissionResponse) RoundTrip(r *http.Request) (*http.Response, error) {
	response, e := l.base.RoundTrip(r)
	if e != nil {
		return nil, e
	}
	response.Body.Close()
	return nil, errors.New("synthetic permission response loss after commit")
}

func TestExecutionPlatformRestartOnlyAcceptsStoppedFinalization(t *testing.T) {
	f := newExecutionFixture(t)
	stopped := f.stoppedInit(t)
	restarted := coordinator.New(f.db.App, f.ticks)
	request := f.message(f.request(t, 2, "schema", strings.Repeat("b", 64)))
	if _, e := restarted.Transition(context.Background(), f.token, "start-permission", request); runs.Code(e) != "ATTEMPT_BUDGET_INSTANCE_ENDED" {
		t.Fatal("restarted platform issued new command authority", e)
	}
	result := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{{Ordinal: 1, Permission: stopped.Permission, Status: "stopped", Exit: stopped.Exit, Stop: stopped}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	if _, e := restarted.Transition(context.Background(), f.token, "result", f.message(result)); e != nil {
		t.Fatal("positive stopped result was rejected after restart", e)
	}
	var reason string
	var reservations int
	e := f.db.Owner.QueryRow(context.Background(), `SELECT terminal_reason,(SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$2) FROM cosmoformer.operations WHERE id=$1`, f.assignment.OperationID, f.assignment.AttemptID).Scan(&reason, &reservations)
	if e != nil || reason != "ATTEMPT_BUDGET_INSTANCE_ENDED" || reservations != 0 {
		t.Fatal("restart failure did not preserve its reason or account for ownership", reason, reservations, e)
	}
}
