//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestHistoricalChangesIgnoreNewerUnacceptedAnalysis(t *testing.T) {
	f := newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true)
	body := finishReviewFixture(t, f, false)
	p := body["run"].(map[string]any)["plan"].(map[string]any)
	base := "/organizations/" + f.org + "/runs/" + f.assignment.RunID
	path := base + "/changes?plan_id=" + p["id"].(string) + "&revision=2"
	accepted := f.admin.call("GET", path, nil, 200)
	var acceptedID, key string
	if err := f.db.Owner.QueryRow(context.Background(), `SELECT id::text,object_key FROM cosmoformer.artifacts WHERE attempt_id=$1 AND kind='normalized_changes'`, f.assignment.AttemptID).Scan(&acceptedID, &key); err != nil {
		t.Fatal(err)
	}
	// An unaccepted analysis can exist after an interrupted/repeated upload. It
	// must never supersede the immutable artifact named by the successful result.
	f.exec(t, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,version_id,sha256,byte_count,state,finalized_at,created_at) SELECT org_id,attempt_id,plan_id,kind,gen_random_uuid(),bucket,object_key,version_id,sha256,byte_count,'finalized',clock_timestamp(),clock_timestamp()+interval '1 hour' FROM cosmoformer.artifacts WHERE id=$1`, acceptedID)
	current := f.admin.call("GET", path, nil, 200)
	a, _ := json.Marshal(accepted["changes"])
	b, _ := json.Marshal(current["changes"])
	if !bytes.Equal(a, b) {
		t.Fatal("unaccepted normalized artifact changed review")
	}
	// Simulate the expired historical state and released usability holds. The
	// scheduled expiry/cleanup workflow remains outside this milestone.
	f.exec(t, `UPDATE cosmoformer.plans SET status='expired',revision=revision+1 WHERE id=$1`, p["id"])
	f.exec(t, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='plan' AND owner_id=$1 AND released_at IS NULL`, p["id"])
	historicalPath := strings.Replace(path, "revision=2", "revision=3", 1)
	historical := f.admin.call("GET", historicalPath, nil, 200)
	b, _ = json.Marshal(historical["changes"])
	if !bytes.Equal(a, b) {
		t.Fatal("expiry/hold release changed retained review")
	}
	status := f.admin.call("GET", base, nil, 200)["run"].(map[string]any)["plan"].(map[string]any)
	if status["status"] != "expired" || status["evidence_status"] != "retained" {
		t.Fatal("historical usability conflated with retention", status)
	}
	// The original ID becoming unavailable must fail even though the newer
	// unaccepted artifact still names the same retained, readable bytes.
	f.exec(t, `UPDATE cosmoformer.artifacts SET state='unavailable' WHERE id=$1`, acceptedID)
	f.admin.call("GET", historicalPath, nil, 503)
	status = f.admin.call("GET", base, nil, 200)["run"].(map[string]any)["plan"].(map[string]any)
	if status["evidence_status"] != "unavailable" || status["status"] != "expired" {
		t.Fatal("missing exact evidence fell back", status)
	}
	_ = key
}

func TestArchivedLogMetadataMismatchIsAnExplicitGap(t *testing.T) {
	for _, mismatch := range []string{"sequence", "start", "end"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newExecutionFixture(t)
			f.stoppedInit(t)
			text := "first"
			raw, _ := json.Marshal(evidence.Segment{Sequence: 0, Start: 0, End: 5, Text: &text})
			artifact, _ := f.artifact(t, "log_segment", raw)
			// Custody-valid payload with a deliberately inconsistent metadata binding.
			switch mismatch {
			case "sequence":
				f.exec(t, `UPDATE cosmoformer.log_segments SET sequence=2 WHERE artifact_id=$1`, artifact)
			case "start":
				f.exec(t, `UPDATE cosmoformer.log_segments SET first_offset=1 WHERE artifact_id=$1`, artifact)
			case "end":
				f.exec(t, `UPDATE cosmoformer.log_segments SET last_offset=6 WHERE artifact_id=$1`, artifact)
			}
			path := "/organizations/" + f.org + "/runs/" + f.assignment.RunID + "/logs?attempt_id=" + f.assignment.AttemptID
			batch := f.admin.call("GET", path, nil, 200)
			segments := batch["segments"].([]any)
			if len(segments) != 1 || batch["archive_complete"] != false {
				t.Fatal("invalid archive became complete", batch)
			}
			gap := segments[0].(map[string]any)
			if gap["gap"].(map[string]any)["reason"] != "archive_invalid" || gap["text"] != nil {
				t.Fatal("misbound bytes exposed", gap)
			}
			wantStart, wantEnd := float64(0), float64(5)
			if mismatch == "start" {
				wantStart = 1
			}
			if mismatch == "end" {
				wantEnd = 6
			}
			if gap["byte_start"] != wantStart || gap["byte_end"] != wantEnd {
				t.Fatal("gap invented bounds", gap)
			}
			next := f.admin.call("GET", path+"&cursor="+batch["cursor"].(string), nil, 200)
			if len(next["segments"].([]any)) != 0 {
				t.Fatal("cursor did not advance past corrupt segment", next)
			}
		})
	}
}

func TestLogsRecoverPreviewEpochThroughDurableArchive(t *testing.T) {
	f := newExecutionFixture(t)
	f.stoppedInit(t)
	text := "retained"
	raw, _ := json.Marshal(evidence.Segment{Sequence: 0, Start: 0, End: 8, Text: &text})
	f.artifact(t, "log_segment", raw)
	path := "/organizations/" + f.org + "/runs/" + f.assignment.RunID + "/logs?attempt_id=" + f.assignment.AttemptID
	first := f.admin.call("GET", path+"&limit_bytes=3", nil, 200)
	if first["has_more"] != true || first["segments"].([]any)[0].(map[string]any)["text"] != "ret" {
		t.Fatal("archive byte page incorrect", first)
	}
	// Worker preview is platform-epoch managed. Budget eviction changes the
	// preview epoch; the API resumes the outstanding archive sequence and marks
	// unknown continuity instead of inventing missing preview bytes.
	// Exceed the platform preview budget to cause an actual epoch eviction.
	for i := 0; i < 20; i++ {
		f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/log-preview", map[string]any{"stream": "console", "epoch": "agent-one", "sequence": i + 1, "byte_start": 8 + i*60000, "text": strings.Repeat("p", 60000)}, 200)
	}
	reset := f.admin.call("GET", path+"&cursor="+first["cursor"].(string), nil, 200)
	if reset["reset"] != true || reset["reset_reason"] != "epoch_changed" {
		t.Fatal("preview epoch change was hidden", reset)
	}
	known, unknown := false, false
	for _, entry := range reset["segments"].([]any) {
		s := entry.(map[string]any)
		known = known || s["text"] == "retained"
		if g, ok := s["gap"].(map[string]any); ok {
			unknown = unknown || g["reason"] == "preview_reset" && g["unknown_extent"] == true
		}
	}
	if !known || !unknown {
		t.Fatal("reset lost archive or omitted unknown continuity", reset)
	}
	cursor, err := evidence.DecodeCursor(reset["cursor"].(string), f.assignment.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Next = 1 << 40
	f.admin.call("GET", path+"&cursor="+cursor.Encode(), nil, 422)
}

func TestStackAndPlanPagesFilterUnauthorizedRowsBeforeLimits(t *testing.T) {
	f := newExecutionFixtureForSourceProfile(t, map[string]any{"kind": "branch"}, true)
	body := finishReviewFixture(t, f, false)
	r := body["run"].(map[string]any)
	project := r["project_id"].(string)
	base := "/organizations/" + f.org
	hidden := map[string]string{}
	hiddenEnv := id(f.admin.call("POST", base+"/projects/"+project+"/environments", map[string]any{"name": "Hidden"}, 200))
	for i := 0; i < 3; i++ {
		f.exec(t, `INSERT INTO cosmoformer.stacks(org_id,project_id,environment_id,repository_id,declaration_id,display_name) SELECT org_id,project_id,$2,repository_id,gen_random_uuid(),$3 FROM cosmoformer.stacks WHERE id=$1`, f.stack, hiddenEnv, fmt.Sprintf("A hidden %d", i))
	}
	page := f.admin.call("GET", base+"/stacks?limit=1", nil, 200)
	if len(page["stacks"].([]any)) != 1 || page["stacks"].([]any)[0].(map[string]any)["id"] != f.stack {
		t.Fatal("hidden stacks consumed visible page", page)
	}
	// Clone independent immutable plan records into queued operations. Their
	// retained evidence is not finalized: only list scoping/order is under test.
	for i := 0; i < 3; i++ {
		op, run := runs.ID(), runs.ID()
		work, attempt := runs.ID(), runs.ID()
		stack := f.stack
		if i == 2 {
			if err := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.stacks WHERE environment_id=$1 LIMIT 1`, hiddenEnv).Scan(&stack); err != nil {
				t.Fatal(err)
			}
			revision, target := runs.ID(), runs.ID()
			f.exec(t, `INSERT INTO cosmoformer.stack_revisions(id,org_id,stack_id,commit,parsed_config,effective_config,semantic_digest,inventory_revision) SELECT $2,org_id,$3,commit,parsed_config,effective_config,semantic_digest,inventory_revision FROM cosmoformer.stack_revisions WHERE id=(SELECT effective_revision_id FROM cosmoformer.stacks WHERE id=$1)`, f.stack, revision, stack)
			f.exec(t, `INSERT INTO cosmoformer.backend_targets(id,partition,bucket,effective_key) SELECT $2,partition,bucket,$3 FROM cosmoformer.backend_targets WHERE id=(SELECT target_id FROM cosmoformer.stack_bindings WHERE stack_id=$1)`, f.stack, target, "synthetic-hidden/"+stack)
			f.exec(t, `INSERT INTO cosmoformer.stack_bindings(org_id,stack_id,target_id,profile,bound_revision_id) SELECT org_id,$2,$3,profile,$4 FROM cosmoformer.stack_bindings WHERE stack_id=$1`, f.stack, stack, target, revision)
			hidden["stacks"], hidden["runs"] = stack, run
		}
		f.exec(t, `INSERT INTO cosmoformer.runs(id,org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,requester_id) SELECT $2,org_id,$3,source_subject_id,source_facts_revision,source_commit,purpose,requester_id FROM cosmoformer.runs WHERE id=$1`, f.assignment.RunID, run, stack)
		f.exec(t, `INSERT INTO cosmoformer.operations(id,org_id,run_id,stack_id,kind,status,priority,accepted_at,ordering_at,requester_id) SELECT $2,org_id,$3,$4,'plan','queued',priority,accepted_at,ordering_at,requester_id FROM cosmoformer.operations WHERE id=$1`, f.assignment.OperationID, op, run, stack)
		f.exec(t, `INSERT INTO cosmoformer.work_items(id,org_id,operation_id,phase,status,assignment_spec,attempt_count) SELECT $2,org_id,$3,'execution','done',assignment_spec,1 FROM cosmoformer.work_items WHERE id=$1`, f.assignment.WorkItemID, work, op)
		f.exec(t, `INSERT INTO cosmoformer.attempts(id,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,outcome,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,closed_at) SELECT $2,org_id,$3,$4,$5,(SELECT target_id FROM cosmoformer.stack_bindings WHERE stack_id=$5),worker_id,generation,agent_instance,'terminal','failure',budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,clock_timestamp() FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID, attempt, work, op, stack)
		var planID string
		if err := f.db.Owner.QueryRow(context.Background(), `INSERT INTO cosmoformer.plans(org_id,operation_id,attempt_id,stack_id,target_id,status,provenance,source_facts_revision,state_epoch,rule_version,profile_id,created_at) SELECT org_id,$2,$4,$3,(SELECT target_id FROM cosmoformer.stack_bindings WHERE stack_id=$3),'assembling',provenance,source_facts_revision,state_epoch,rule_version,profile_id,'2999-01-01 12:00:00+00'::timestamptz FROM cosmoformer.plans WHERE id=$1 RETURNING id::text`, r["plan"].(map[string]any)["id"], op, stack, attempt).Scan(&planID); err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			hidden["plans"] = planID
		}
	}
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 4; i++ {
		query := base + "/plans?limit=1"
		if cursor != "" {
			query += "&before=" + cursor
		}
		p := f.admin.call("GET", query, nil, 200)
		items := p["plans"].([]any)
		if len(items) != 1 {
			t.Fatal("visible plan omitted", p)
		}
		id := items[0].(map[string]any)["id"].(string)
		if seen[id] {
			t.Fatal("plan cursor repeated item")
		}
		seen[id] = true
		next, ok := p["next_cursor"].(string)
		if !ok {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatal("plan scope/page tie-break lost visible rows", seen)
	}
	// Hidden and nonexistent cursors must have the same empty response. A hidden
	// record must not become a boundary for otherwise visible rows.
	assertEmptyPage := func(kind, cursor, filters string) {
		t.Helper()
		page := f.admin.call("GET", base+"/"+kind+"?limit=1&before="+cursor+filters, nil, 200)
		if len(page[kind].([]any)) != 0 || page["next_cursor"] != nil {
			t.Errorf("%s resolved an out-of-scope cursor with filters %q: %v", kind, filters, page)
		}
	}
	for _, kind := range []string{"stacks", "runs", "plans"} {
		assertEmptyPage(kind, runs.ID(), "")
		assertEmptyPage(kind, hidden[kind], "")
	}
	// Even a currently visible cursor must belong to the requested environment
	// and stack. Broader grants must not bypass the list's explicit filters.
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,$2,'viewer' FROM cosmoformer.grants WHERE project_id=$1 LIMIT 1`, project, hiddenEnv)
	for _, kind := range []string{"stacks", "runs", "plans"} {
		visible := f.admin.call("GET", base+"/"+kind+"?stack_id="+hidden["stacks"], nil, 200)[kind].([]any)
		if len(visible) != 1 || visible[0].(map[string]any)["id"] != hidden[kind] {
			t.Fatalf("%s cursor fixture did not become visible after grant", kind)
		}
		assertEmptyPage(kind, hidden[kind], "&environment_id="+r["environment_id"].(string))
		assertEmptyPage(kind, hidden[kind], "&stack_id="+f.stack)
	}
	// Valid stack cursors still advance to another visible row.
	stackPage := f.admin.call("GET", base+"/stacks?limit=1", nil, 200)
	stackCursor := stackPage["next_cursor"].(string)
	stackNext := f.admin.call("GET", base+"/stacks?limit=1&before="+stackCursor, nil, 200)
	if len(stackNext["stacks"].([]any)) != 1 || stackNext["stacks"].([]any)[0].(map[string]any)["id"] == stackCursor {
		t.Fatal("valid stack cursor failed to advance", stackNext)
	}
	// Recheck current grants for an already issued cursor, not just page rows.
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE environment_id=$1`, hiddenEnv)
	for _, kind := range []string{"stacks", "runs", "plans"} {
		assertEmptyPage(kind, hidden[kind], "")
	}
}
