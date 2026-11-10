//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// Local custody/protocol fixtures, never seeded in development data. The generic
// provider deliberately needs no AWS schema or cloud service to verify coverage.
func finishReviewFixture(t *testing.T, f *executionFixture, forge bool) map[string]any {
	t.Helper()
	init := f.stoppedInit(t)
	lock := []byte("provider \"registry.opentofu.org/example/other\" {\n version=\"2.0.0\"\n hashes=[\"zh:" + strings.Repeat("b", 64) + "\"]\n}\n")
	providers := []runs.Provider{{Address: "registry.opentofu.org/example/other", Version: "2.0.0", Platform: "linux_amd64", Hashes: []string{"zh:" + strings.Repeat("b", 64)}}}
	seal := f.seal(t, providers, lock)
	schema := []byte(`{"format_version":"1.0","provider_schemas":{"registry.opentofu.org/example/other":{"provider":{"version":0,"block":{}},"resource_schemas":{"other_item":{"version":0,"block":{"attributes":{"value":{"type":"number","optional":true},"label":{"type":"string","optional":true}}}}}}}}`)
	schemaPermit := f.transition(t, "start-permission", f.request(t, 2, "schema", seal.Digest), 200).Permission
	schemaStop := f.commandStopped(t, schemaPermit, 0)
	schemaID, _ := f.artifact(t, "provider_schema", schema)
	planPermit := f.transition(t, "start-permission", f.request(t, 3, "plan", seal.Digest), 200).Permission
	planStop := f.commandStopped(t, planPermit, 2)
	binaryID, _ := f.artifact(t, "binary_plan", []byte("synthetic protected saved plan"))
	showPermit := f.transition(t, "start-permission", f.request(t, 4, "show", seal.Digest), 200).Permission
	showStop := f.commandStopped(t, showPermit, 0)
	raw := []byte(`{"format_version":"1.2","terraform_version":"1.13.8","planned_values":{},"configuration":{},"resource_changes":[{"address":"other_item.fixture","mode":"managed","name":"fixture","type":"other_item","provider_name":"registry.opentofu.org/example/other","change":{"actions":["update"],"before":{"value":9007199254740993,"label":"safe sibling"},"after":{"value":9007199254740994,"label":"safe sibling"},"after_unknown":{"label":true},"before_sensitive":{},"after_sensitive":{}}}]}`)
	rawID, _ := f.artifact(t, "raw_plan_json", raw)
	o, ids, err := plan.Allocate(raw, plan.Options{Engine: "opentofu", Version: "1.13.8", ProfileID: f.pre.Profile, Providers: providers, Secrets: &evidence.Registry{}}, nil, runs.ID)
	if err != nil {
		t.Fatal(err)
	}
	safe, err := plan.Analyze(raw, schema, o)
	if err != nil {
		t.Fatal(err)
	}
	if safe.Coverage[0].Status != "generic_only" || len(safe.Coverage[0].UnknownHidden) != 1 {
		t.Fatal("independent coverage fixture incorrect")
	}
	if forge {
		safe.Coverage[0].Status = "supported"
		safe.Coverage[0].UnknownHidden = []string{}
	}
	changesID, _ := f.artifact(t, "normalized_changes", plan.Analysis{Identities: ids, Changes: safe})
	findingsID, _ := f.artifact(t, "findings", safe.Findings)
	var preID string
	if err = f.db.Owner.QueryRow(context.Background(), `SELECT pre_init_spec_artifact_id::text FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID).Scan(&preID); err != nil {
		t.Fatal(err)
	}
	commands := []runs.CommandResult{}
	for _, e := range []runs.CommandEvent{init, schemaStop, planStop, showStop} {
		commands = append(commands, runs.CommandResult{Ordinal: e.Ordinal, Permission: e.Permission, Status: "stopped", Exit: e.Exit, Stop: e})
	}
	result := runs.ExecutionResult{Outcome: "success", Commands: commands, Artifacts: []string{preID, seal.Artifact, seal.LockArtifact, schemaID, binaryID, rawID, changesID, findingsID}, PreInitDigest: runs.Digest(f.pre), ManifestDigest: seal.Digest, LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	if forge {
		response := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/result", f.message(result), 409)
		if !bytes.Contains(response, []byte("PLAN_ANALYSIS_MISMATCH")) {
			t.Fatal("forged coverage bypassed independent readiness", string(response))
		}
		f.retained(t)
		var count int
		if err = f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.plans WHERE ready_at IS NOT NULL`).Scan(&count); err != nil || count != 0 {
			t.Fatal("forged coverage trusted", err)
		}
		return nil
	}
	f.transition(t, "result", result, 200)
	return f.admin.call("GET", "/organizations/"+f.org+"/runs/"+f.assignment.RunID, nil, 200)
}
func TestReadinessRejectsForgedCoverage(t *testing.T) {
	finishReviewFixture(t, newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true), true)
}

func TestWorkspaceExactHistoricalEvidenceAndCurrentPermissions(t *testing.T) {
	f := newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true)
	body := finishReviewFixture(t, f, false)
	r := body["run"].(map[string]any)
	p := r["plan"].(map[string]any)
	base := "/organizations/" + f.org
	if r["commit"] != f.assignment.Source.Commit || p["evidence_status"] != "retained" || body["can_read_raw"] != false || len(body["raw_artifacts"].([]any)) != 0 {
		t.Fatal("invalid scoped review context", body)
	}
	changes := base + "/runs/" + f.assignment.RunID + "/changes?plan_id=" + p["id"].(string) + "&revision=2&manifest_digest=" + p["manifest_digest"].(string)
	original := f.admin.call("GET", changes, nil, 200)
	f.admin.call("GET", strings.Replace(changes, "revision=2", "revision=1", 1), nil, 409)
	f.admin.call("GET", strings.Replace(changes, p["manifest_digest"].(string), strings.Repeat("a", 64), 1), nil, 409)
	f.admin.call("GET", strings.Replace(changes, f.assignment.RunID, runs.ID(), 1), nil, 404)
	// Retention is distinct from usability, including after revision changes.
	f.exec(t, `UPDATE cosmoformer.plans SET status='invalidated',revision=revision+1,invalidation_reason='SOURCE_CHANGED' WHERE id=$1`, p["id"])
	f.admin.call("GET", changes, nil, 409)
	historical := f.admin.call("GET", strings.Replace(changes, "revision=2", "revision=3", 1), nil, 200)
	a, _ := json.Marshal(original["changes"])
	b, _ := json.Marshal(historical["changes"])
	if !bytes.Equal(a, b) {
		t.Fatal("historical evidence rewritten")
	}
	// Administration never acquires execution or raw evidence visibility.
	f.exec(t, `UPDATE cosmoformer.grants SET role='administrator'`)
	f.admin.call("GET", base+"/runs/"+f.assignment.RunID, nil, 404)
	f.admin.call("GET", strings.Replace(changes, "revision=2", "revision=3", 1), nil, 403)
	if got := f.admin.call("GET", base+"/runs", nil, 200)["runs"].([]any); len(got) != 0 {
		t.Fatal("admin saw execution history")
	}
	if got := f.admin.call("GET", base+"/plans", nil, 200)["plans"].([]any); len(got) != 0 {
		t.Fatal("admin saw plans")
	}
}

func TestWorkspaceScopesBeforeLimitsAndQueueContext(t *testing.T) {
	f := newPreflightFixture(t)
	base := "/organizations/" + f.org
	first := f.admin.call("GET", base+"/runs?limit=1", nil, 200)["runs"].([]any)
	if len(first) != 1 || first[0].(map[string]any)["queue_reason"] != "NO_COMPATIBLE_WORKER" {
		t.Fatal("queued context missing", first)
	}
	project := first[0].(map[string]any)["project_id"].(string)
	env := first[0].(map[string]any)["environment_id"].(string)
	// Newer runs in a different environment must not consume the authorized page.
	otherEnv := id(f.admin.call("POST", base+"/projects/"+project+"/environments", map[string]any{"name": "Restricted"}, 200))
	var otherStack string
	if err := f.db.Owner.QueryRow(context.Background(), `INSERT INTO cosmoformer.stacks(org_id,project_id,environment_id,repository_id,declaration_id,display_name) SELECT org_id,project_id,$2,repository_id,gen_random_uuid(),'A restricted stack' FROM cosmoformer.stacks WHERE id=$1 RETURNING id::text`, f.stack, otherEnv).Scan(&otherStack); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `INSERT INTO cosmoformer.runs(org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,requester_id) SELECT r.org_id,$2,r.source_subject_id,r.source_facts_revision,r.source_commit,r.purpose,r.requester_id FROM cosmoformer.runs r,generate_series(1,105) WHERE r.stack_id=$1`, f.stack, otherStack)
	f.exec(t, `INSERT INTO cosmoformer.operations(org_id,run_id,stack_id,kind,status,priority,accepted_at,ordering_at,requester_id) SELECT org_id,id,stack_id,'plan','queued','medium',clock_timestamp(),clock_timestamp(),requester_id FROM cosmoformer.runs WHERE stack_id=$1`, otherStack)
	got := f.admin.call("GET", base+"/runs?limit=1", nil, 200)["runs"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["id"] != first[0].(map[string]any)["id"] {
		t.Fatal("scope filtering happened after page limit")
	}
	stacks := f.admin.call("GET", base+"/stacks?project_id="+project+"&environment_id="+env+"&limit=1", nil, 200)["stacks"].([]any)
	if len(stacks) != 1 || stacks[0].(map[string]any)["id"] != f.stack {
		t.Fatal("stack context filter failed", stacks)
	}
	// Stable cursor traverses multiple accepted requests in visible scope only.
	f.exec(t, `INSERT INTO cosmoformer.runs(org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,requester_id) SELECT org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,requester_id FROM cosmoformer.runs WHERE stack_id=$1 LIMIT 1`, f.stack)
	f.exec(t, `INSERT INTO cosmoformer.operations(org_id,run_id,stack_id,kind,status,priority,accepted_at,ordering_at,requester_id) SELECT r.org_id,r.id,r.stack_id,'plan','queued','medium',clock_timestamp(),clock_timestamp(),r.requester_id FROM cosmoformer.runs r WHERE r.stack_id=$1 AND NOT EXISTS(SELECT FROM cosmoformer.operations o WHERE o.run_id=r.id)`, f.stack)
	page := f.admin.call("GET", base+"/runs?limit=1&project_id="+project, nil, 200)
	cursor := page["next_cursor"].(string)
	second := f.admin.call("GET", base+"/runs?limit=1&before="+cursor, nil, 200)
	if second["runs"].([]any)[0].(map[string]any)["id"] == page["runs"].([]any)[0].(map[string]any)["id"] {
		t.Fatal("cursor repeated a run")
	}
	// Wrong tenant never obtains even an authorized subject in a different org.
	otherOrg := id(f.admin.call("POST", "/organizations", map[string]any{"name": "Other tenant"}, 200))
	f.admin.call("GET", "/organizations/"+otherOrg+"/runs/"+cursor, nil, 404)
}

func TestWorkspaceUnavailableExactEvidenceAndIORevocation(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true)
			body := finishReviewFixture(t, f, false)
			p := body["run"].(map[string]any)["plan"].(map[string]any)
			path := "/organizations/" + f.org + "/runs/" + f.assignment.RunID + "/changes?plan_id=" + p["id"].(string) + "&revision=2"
			var key string
			if err := f.db.Owner.QueryRow(context.Background(), `SELECT object_key FROM cosmoformer.artifacts WHERE attempt_id=$1 AND kind='normalized_changes'`, f.assignment.AttemptID).Scan(&key); err != nil {
				t.Fatal(err)
			}
			f.storage.mu.Lock()
			if scenario == "missing" {
				delete(f.storage.objects, key)
			}
			if scenario == "corrupt" {
				f.storage.objects[key] = "corrupt retained bytes"
			}
			if scenario == "revoked" {
				f.storage.onGet = func() error { f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp()`); return nil }
			}
			f.storage.mu.Unlock()
			status := 503
			if scenario == "revoked" {
				status = 403
			}
			f.admin.call("GET", path, nil, status)
			f.storage.mu.Lock()
			f.storage.onGet = nil
			f.storage.mu.Unlock()
			if scenario != "revoked" {
				current := f.admin.call("GET", "/organizations/"+f.org+"/runs/"+f.assignment.RunID, nil, 200)
				plan := current["run"].(map[string]any)["plan"].(map[string]any)
				if plan["status"] != "invalidated" || plan["reason"] != "EVIDENCE_UNAVAILABLE" || plan["evidence_status"] != "unavailable" || plan["manifest_digest"] != p["manifest_digest"] {
					t.Fatal("invalid exact evidence retained eligibility or changed identity", plan)
				}
			}
		})
	}
}

func TestWorkspaceAttemptAndRawEvidencePages(t *testing.T) {
	f := newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true)
	finishReviewFixture(t, f, false)
	ctx := context.Background()
	f.exec(t, `INSERT INTO cosmoformer.attempts(org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,outcome,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,closed_at,execution_spec,execution_spec_digest) SELECT a.org_id,a.work_item_id,a.operation_id,a.stack_id,a.target_id,a.worker_id,a.generation,a.agent_instance,'terminal','failure',a.budget_platform_instance,a.accepted_tick,a.deadline_tick,a.cap_seconds,a.display_deadline,clock_timestamp(),a.execution_spec,a.execution_spec_digest FROM cosmoformer.attempts a,generate_series(1,105) WHERE a.id=$1`, f.assignment.AttemptID)
	base := "/organizations/" + f.org + "/runs/" + f.assignment.RunID
	first := f.admin.call("GET", base, nil, 200)
	if len(first["attempts"].([]any)) != 100 {
		t.Fatal("attempt page not bounded")
	}
	before := first["attempts_next"].(string)
	older := f.admin.call("GET", base+"?attempt_before="+before, nil, 200)
	seen := map[string]bool{}
	for _, a := range first["attempts"].([]any) {
		seen[a.(map[string]any)["id"].(string)] = true
	}
	for _, a := range older["attempts"].([]any) {
		if seen[a.(map[string]any)["id"].(string)] {
			t.Fatal("attempt cursor repeated data")
		}
	}
	if len(older["attempts"].([]any)) != 7 {
		t.Fatal("older attempts lost", older)
	}
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'raw_evidence_read' FROM cosmoformer.grants LIMIT 1`)
	f.exec(t, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,version_id,sha256,byte_count,state,finalized_at) SELECT org_id,attempt_id,plan_id,'raw_plan_json',gen_random_uuid(),bucket,gen_random_uuid()::text,version_id,sha256,byte_count,'finalized',clock_timestamp() FROM cosmoformer.artifacts,generate_series(1,105) WHERE attempt_id=$1 AND kind='raw_plan_json'`, f.assignment.AttemptID)
	raw := f.admin.call("GET", base, nil, 200)
	if raw["can_read_raw"] != true || len(raw["raw_artifacts"].([]any)) != 100 {
		t.Fatal("raw page capability/bounds incorrect")
	}
	rest := f.admin.call("GET", base+"?artifact_before="+raw["raw_artifacts_next"].(string), nil, 200)
	if len(rest["raw_artifacts"].([]any)) == 0 {
		t.Fatal("older raw evidence omitted")
	}
	// The successful result still selects its own normalized evidence only.
	var p string
	if err := f.db.Owner.QueryRow(ctx, `SELECT id::text FROM cosmoformer.plans WHERE attempt_id=$1`, f.assignment.AttemptID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	f.admin.call("GET", base+"/changes?plan_id="+p+"&revision=2", nil, 200)
}
