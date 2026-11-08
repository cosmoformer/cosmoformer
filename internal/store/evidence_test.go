//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var evidenceCtx = context.Background()

func sqlOK(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(evidenceCtx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
func sqlReject(t *testing.T, c *pgx.Conn, code, sql string, args ...any) {
	t.Helper()
	_, err := c.Exec(evidenceCtx, sql, args...)
	var p *pgconn.PgError
	if !errors.As(err, &p) || p.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %v", code, err)
	}
}
func evidenceFixture(t *testing.T, db *testsupport.Database) map[string]string {
	t.Helper()
	c := db.Owner
	ids := map[string]string{}
	add := func(name, query string, args ...any) string {
		t.Helper()
		var id string
		if err := c.QueryRow(evidenceCtx, query+" RETURNING id::text", args...).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		return id
	}
	org := add("org", `INSERT INTO cosmoformer.organizations(name) VALUES('Evidence fixture')`)
	sqlOK(t, c, `SELECT set_config('app.org_id',$1,false)`, org)
	account := add("account", `INSERT INTO cosmoformer.accounts(username,display_name,password_phc) VALUES('evidence-'||substr(gen_random_uuid()::text,1,8),'Evidence','synthetic-verifier')`)
	project := add("project", `INSERT INTO cosmoformer.projects(org_id,name) VALUES($1,'Fixture')`, org)
	env := add("env", `INSERT INTO cosmoformer.environments(org_id,project_id,name) VALUES($1,$2,'Fixture')`, org, project)
	integration := add("integration", `INSERT INTO cosmoformer.integrations(org_id,name,provider) VALUES($1,'Fixture','bitbucket_cloud')`, org)
	repo := add("repo", `INSERT INTO cosmoformer.repositories(org_id,project_id,integration_id,external_id,url,tracked_branch) VALUES($1,$2,$3,'fixture','https://bitbucket.org/fictional/repo.git','main')`, org, project, integration)
	stack := add("stack", `INSERT INTO cosmoformer.stacks(org_id,project_id,environment_id,repository_id,declaration_id,display_name) VALUES($1,$2,$3,$4,gen_random_uuid(),'Fixture')`, org, project, env, repo)
	rev := add("revision", `INSERT INTO cosmoformer.stack_revisions(org_id,stack_id,commit,parsed_config,effective_config,semantic_digest,inventory_revision) VALUES($1,$2,'synthetic','{}','{}',decode(repeat('ab',32),'hex'),1)`, org, stack)
	sqlOK(t, c, `UPDATE cosmoformer.stacks SET effective_revision_id=$1 WHERE id=$2`, rev, stack)
	sqlOK(t, c, `SELECT cosmoformer.bind_backend($1,$2,'aws','fictional-evidence-bucket',$1::uuid::text,'s3',NULL)`, org, stack)
	var target string
	if err := c.QueryRow(evidenceCtx, `SELECT target_id::text FROM cosmoformer.stack_bindings WHERE stack_id=$1`, stack).Scan(&target); err != nil {
		t.Fatal(err)
	}
	ids["target"] = target
	subject := add("subject", `INSERT INTO cosmoformer.source_subjects(org_id,repository_id,kind,external_key,facts) VALUES($1,$2,'branch','main','{}')`, org, repo)
	run := add("run", `INSERT INTO cosmoformer.runs(org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,requester_id) VALUES($1,$2,$3,1,'synthetic','tracked',$4)`, org, stack, subject, account)
	op := add("operation", `INSERT INTO cosmoformer.operations(org_id,stack_id,run_id,kind,status,accepted_at,ordering_at,requester_id) VALUES($1,$2,$3,'plan','queued',now(),now(),$4)`, org, stack, run, account)
	work := add("work", `INSERT INTO cosmoformer.work_items(org_id,operation_id,phase,status) VALUES($1,$2,'execution','queued')`, org, op)
	pool := add("pool", `INSERT INTO cosmoformer.worker_pools(org_id,name) VALUES($1,'Fixture')`, org)
	runtime := add("runtime", `INSERT INTO cosmoformer.runtime_catalog(engine,version,architecture,image_digest) VALUES('opentofu','synthetic','linux_amd64',gen_random_uuid()::text)`)
	worker := add("worker", `INSERT INTO cosmoformer.workers(org_id,pool_id,name,verifier,runtime_id) VALUES($1,$2,'Fixture',decode(repeat('cd',32),'hex'),$3)`, org, pool, runtime)
	attempt := add("attempt", `INSERT INTO cosmoformer.attempts(org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline) VALUES($1,$2,$3,$4,$5,$6,1,gen_random_uuid(),'claimed',gen_random_uuid(),1,100,10,now())`, org, work, op, stack, target, worker)
	add("plan", `INSERT INTO cosmoformer.plans(org_id,operation_id,attempt_id,stack_id,target_id,status,provenance,source_facts_revision,state_epoch,rule_version,profile_id) VALUES($1,$2,$3,$4,$5,'assembling','normal',1,0,'synthetic','synthetic')`, org, op, attempt, stack, target)
	add("recovery", `INSERT INTO cosmoformer.recovery_cases(org_id,stack_id,target_id,origin_attempt_id,status,evidence,opened_at) VALUES($1,$2,$3,$4,'open','{}',now())`, org, stack, target, attempt)
	return ids
}
func TestEvidenceLifecycleAndHolds(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	c := db.Owner
	var artifact string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,$3,'manifest',gen_random_uuid(),'fictional','opaque','uploading') RETURNING id::text`, x["org"], x["attempt"], x["plan"]).Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='pending' WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='finalized' WHERE id=$1`, artifact)
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='finalized',version_id='v1',sha256=decode(repeat('ab',32),'hex'),byte_count=42,finalized_at=now() WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET id=gen_random_uuid() WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET version_id='v2' WHERE id=$1`, artifact)
	sqlReject(t, c, "23503", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'plan',gen_random_uuid(),'review')`, x["org"], artifact)
	for _, kind := range []string{"plan", "operation", "attempt", "recovery"} {
		sqlOK(t, c, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,$3,$4,'use')`, x["org"], artifact, kind, x[kind])
		sqlReject(t, c, "23505", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,$3,$4,'use')`, x["org"], artifact, kind, x[kind])
	}

	// A second PostgreSQL connection cannot slip cleanup past an uncommitted
	// evidence use. The trigger must wait for the shared gate before checking.
	use, err := db.App.Pool.Begin(evidenceCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer use.Rollback(evidenceCtx)
	if _, err = use.Exec(evidenceCtx, `SELECT set_config('app.org_id',$1,true)`, x["org"]); err != nil {
		t.Fatal(err)
	}
	if _, err = use.Exec(evidenceCtx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'attempt',$3,'concurrent-use')`, x["org"], artifact, x["attempt"]); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `SET lock_timeout='100ms'`)
	sqlReject(t, c, "55P03", `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'v1',gen_random_uuid(),now())`, x["org"], artifact)
	sqlOK(t, c, `SET lock_timeout=0`)
	if err = use.Commit(evidenceCtx); err != nil {
		t.Fatal(err)
	}
	var backup string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.backup_sets(cutoff,set_digest,artifact_count) VALUES(now(),decode(repeat('ef',32),'hex'),1) RETURNING id::text`).Scan(&backup); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'backup',$3,'capture')`, x["org"], artifact, backup)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'v1',gen_random_uuid(),now())`, x["org"], artifact)
	app, err := db.App.Pool.Acquire(evidenceCtx)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Release()
	a := app.Conn()
	sqlOK(t, a, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	for _, query := range []string{`SELECT * FROM cosmoformer.backup_sets`, `INSERT INTO cosmoformer.backup_sets(cutoff,set_digest,artifact_count) VALUES(now(),decode(repeat('ab',32),'hex'),0)`} {
		sqlReject(t, a, "42501", query)
	}
	sqlReject(t, a, "42501", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'backup',$3,'escape')`, x["org"], artifact, backup)
	sqlOK(t, a, `UPDATE cosmoformer.artifact_holds SET released_at=now() WHERE owner_kind='backup'`)
	var count int
	if err = c.QueryRow(evidenceCtx, `SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='backup' AND released_at IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatal("runtime released backup protection", err)
	}
	sqlOK(t, a, `SELECT set_config('app.org_id',gen_random_uuid()::text,false)`)
	if err = a.QueryRow(evidenceCtx, `SELECT count(*) FROM cosmoformer.artifacts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("cross tenant evidence visible", err)
	}
	sqlReject(t, a, "42501", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'attempt',$3,'escape')`, x["org"], artifact, x["attempt"])
	sqlOK(t, c, `UPDATE cosmoformer.artifact_holds SET released_at=now()`)
	sqlReject(t, c, "23503", `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'wrong',gen_random_uuid(),now())`, x["org"], artifact)
	sqlOK(t, c, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'v1',gen_random_uuid(),now())`, x["org"], artifact)
	sqlReject(t, c, "23514", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'plan',$3,'too late')`, x["org"], artifact, x["plan"])
	sqlReject(t, c, "23514", `DELETE FROM cosmoformer.cleanup_claims WHERE artifact_id=$1`, artifact)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.cleanup_claims SET claim_id=gen_random_uuid() WHERE artifact_id=$1`, artifact)
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='finalized' WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='deleted' WHERE id=$1`, artifact)
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='deleted',deleted_at=now(),tombstone='{}' WHERE id=$1`, artifact)
	sqlReject(t, c, "23514", `DELETE FROM cosmoformer.artifacts WHERE id=$1`, artifact)

	// Unavailable evidence is retained and cannot gain new protection or revive.
	var missing string
	if err = c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,'log_segment',gen_random_uuid(),'fictional','missing','uploading') RETURNING id::text`, x["org"], x["attempt"]).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'attempt',$3,'upload')`, x["org"], missing, x["attempt"])
	sqlOK(t, c, `UPDATE cosmoformer.artifact_holds SET released_at=now() WHERE artifact_id=$1`, missing)
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='unavailable' WHERE id=$1`, missing)
	sqlReject(t, c, "23514", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'attempt',$3,'reuse')`, x["org"], missing, x["attempt"])
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='uploading' WHERE id=$1`, missing)
	sqlReject(t, c, "23514", `INSERT INTO cosmoformer.log_segments(org_id,attempt_id,stream,sequence,first_offset,last_offset) VALUES($1,$2,'stdout',0,0,0)`, x["org"], x["attempt"])
	sqlOK(t, c, `INSERT INTO cosmoformer.log_segments(org_id,attempt_id,stream,sequence,first_offset,last_offset,gap,gap_reason,unknown_extent) VALUES($1,$2,'stdout',0,0,0,true,'transport_loss',true)`, x["org"], x["attempt"])
	other := evidenceFixture(t, db)
	sqlReject(t, c, "23503", `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,'manifest',gen_random_uuid(),'fictional','cross','uploading')`, other["org"], x["attempt"])
	var otherArtifact string
	if err = c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,'manifest',gen_random_uuid(),'fictional','second','uploading') RETURNING id::text`, other["org"], other["attempt"]).Scan(&otherArtifact); err != nil {
		t.Fatal(err)
	}
	sqlReject(t, c, "23503", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'plan',$3,'cross')`, other["org"], otherArtifact, x["plan"])
	// A single installation capture can protect artifacts in multiple tenants.
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='finalized',version_id='v2',sha256=decode(repeat('ab',32),'hex'),byte_count=0,finalized_at=now() WHERE id=$1`, otherArtifact)
	sqlOK(t, c, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'backup',$3,'capture')`, other["org"], otherArtifact, backup)
	if err = db.App.Ready(evidenceCtx); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `ALTER TABLE cosmoformer.artifact_holds NO FORCE ROW LEVEL SECURITY`)
	if err = db.App.Ready(evidenceCtx); err == nil {
		t.Fatal("startup accepted weakened tenant isolation")
	}
}

func TestVersionFiveEvidenceUpgrade(t *testing.T) {
	db := testsupport.NewDatabase(t)
	c := db.Owner
	sqlOK(t, c, `DROP SCHEMA cosmoformer CASCADE`)
	for _, name := range []string{"001_identity_config.sql", "002_execution.sql", "003_evidence_catalog.sql", "004_security.sql", "005_preflight.sql"} {
		raw, err := os.ReadFile(filepath.Join("migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		sqlOK(t, c, string(raw))
	}
	sqlOK(t, c, `CREATE TABLE cosmoformer.schema_version(version integer NOT NULL); INSERT INTO cosmoformer.schema_version VALUES(5); GRANT SELECT ON cosmoformer.schema_version TO cosmoformer_app`)
	x := evidenceFixture(t, db)
	snapshot := func() string {
		t.Helper()
		var s string
		err := c.QueryRow(evidenceCtx, `SELECT jsonb_build_array(
 (SELECT jsonb_agg(to_jsonb(t)) FROM cosmoformer.accounts t),
 (SELECT jsonb_agg(to_jsonb(t)) FROM cosmoformer.repositories t),
 (SELECT jsonb_agg(to_jsonb(t)) FROM cosmoformer.stack_revisions t),
 (SELECT jsonb_agg(to_jsonb(t)) FROM cosmoformer.stack_bindings t),
 (SELECT jsonb_agg(to_jsonb(t)) FROM cosmoformer.backend_targets t),
 (SELECT jsonb_agg(to_jsonb(t)) FROM cosmoformer.backend_resource_claims t))::text`).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	// Legacy metadata cannot be fabricated. Failed migration leaves version and
	// tenant records intact; removing these synthetic test rows permits retry.
	sqlOK(t, c, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,object_key,version_id,sha256,byte_count,state) VALUES($1,$2,'manifest',gen_random_uuid(),'legacy','v1',decode(repeat('ab',32),'hex'),0,'pending')`, x["org"], x["attempt"])
	if err := store.Migrate(evidenceCtx, db.OwnerDSN); err == nil || !strings.Contains(err.Error(), "EVIDENCE_METADATA_RECONCILIATION_REQUIRED") {
		t.Fatal("legacy evidence migration did not report missing verified metadata", err)
	}
	var version int
	if err := c.QueryRow(evidenceCtx, `SELECT version FROM cosmoformer.schema_version`).Scan(&version); err != nil || version != 5 {
		t.Fatal("failed migration advanced version", err)
	}
	sqlOK(t, c, `DELETE FROM cosmoformer.artifacts`)

	if err := db.App.Ready(evidenceCtx); err == nil {
		t.Fatal("version 5 accepted by version 6 runtime")
	}
	if err := store.Migrate(evidenceCtx, db.OwnerDSN); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	if after := snapshot(); after != before {
		t.Fatal("upgrade changed identities, source configuration or permanent bindings")
	}
	if err := db.App.Ready(evidenceCtx); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(evidenceCtx, db.OwnerDSN); err != nil {
		t.Fatal("migration replay", err)
	}
}

func TestPreparationRetryRetainsFinalizedEvidence(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	c := db.Owner
	var artifact string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,$3,'pre_init_spec',gen_random_uuid(),'fictional','first-preparation','uploading') RETURNING id::text`, x["org"], x["attempt"], x["plan"]).Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='finalized',version_id='first-version',sha256=decode(repeat('ab',32),'hex'),byte_count=42,finalized_at=now() WHERE id=$1`, artifact)
	sqlOK(t, c, `UPDATE cosmoformer.attempts SET pre_init_spec_artifact_id=$1,state='terminal',outcome='preparation_failed',closed_at=now(),stop_proof='{"no_state_child_created":true,"source_processes_reaped":true}' WHERE id=$2 AND NOT ever_permitted`, artifact, x["attempt"])
	sqlOK(t, c, `UPDATE cosmoformer.operations SET status='failed',terminal_reason='preparation_failed' WHERE id=$1`, x["operation"])
	var before string
	if err := c.QueryRow(evidenceCtx, `SELECT to_jsonb(a)::text FROM cosmoformer.artifacts a WHERE id=$1`, artifact).Scan(&before); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `UPDATE cosmoformer.operations SET status='queued' WHERE id=$1`, x["operation"])
	var replacement string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.attempts(org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline)
 SELECT org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,'claimed',budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline FROM cosmoformer.attempts WHERE id=$1 RETURNING id::text`, x["attempt"]).Scan(&replacement); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `UPDATE cosmoformer.plans SET attempt_id=$1 WHERE id=$2 AND status='assembling'`, replacement, x["plan"])
	var after string
	if err := c.QueryRow(evidenceCtx, `SELECT to_jsonb(a)::text FROM cosmoformer.artifacts a WHERE id=$1`, artifact).Scan(&after); err != nil || after != before {
		t.Fatal("retry changed historical evidence", err)
	}
	// History stays attached to the original attempt; it cannot masquerade as
	// the replacement's pre-init or reviewed manifest evidence.
	sqlReject(t, c, "23503", `UPDATE cosmoformer.attempts SET pre_init_spec_artifact_id=$1 WHERE id=$2`, artifact, replacement)
	sqlReject(t, c, "23503", `UPDATE cosmoformer.plans SET manifest_artifact_id=$1 WHERE id=$2`, artifact, x["plan"])
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET attempt_id=$1 WHERE id=$2`, replacement, artifact)
	sqlOK(t, c, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,$3,'pre_init_spec',gen_random_uuid(),'fictional','replacement-preparation','uploading')`, x["org"], replacement, x["plan"])
	// A different plan operation on the same stack is not an owning-chain match.
	var unrelated string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.operations(org_id,stack_id,run_id,kind,status,accepted_at,ordering_at,requester_id) SELECT org_id,stack_id,run_id,kind,status,accepted_at,ordering_at,requester_id FROM cosmoformer.operations WHERE id=$1 RETURNING id::text`, x["operation"]).Scan(&unrelated); err != nil {
		t.Fatal(err)
	}
	sqlReject(t, c, "23503", `INSERT INTO cosmoformer.artifacts(org_id,operation_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,$3,$4,'pre_init_spec',gen_random_uuid(),'fictional','wrong-operation','uploading')`, x["org"], unrelated, replacement, x["plan"])
	sqlReject(t, c, "23503", `UPDATE cosmoformer.plans SET operation_id=$1 WHERE id=$2`, unrelated, x["plan"])
}

func TestOrphanUploadCleanupWithoutFinalization(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	c := db.Owner
	var orphan string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,version_id,state) VALUES($1,$2,'binary_plan',gen_random_uuid(),'fictional','failed-upload','orphan-version','uploading') RETURNING id::text`, x["org"], x["attempt"]).Scan(&orphan); err != nil {
		t.Fatal(err)
	}
	// A known version is insufficient while the owner could still write.
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, orphan)
	// Even an exact claim cannot permit deletion while the owning attempt is open.
	var inflight string
	if err := c.QueryRow(evidenceCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,version_id,state) VALUES($1,$2,'binary_plan',gen_random_uuid(),'fictional','inflight-upload','inflight-version','uploading') RETURNING id::text`, x["org"], x["attempt"]).Scan(&inflight); err != nil {
		t.Fatal(err)
	}
	sqlOK(t, c, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'inflight-version',gen_random_uuid(),now())`, x["org"], inflight)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, inflight)
	sqlOK(t, c, `UPDATE cosmoformer.attempts SET state='terminal',outcome='preparation_failed',closed_at=now() WHERE id=$1`, x["attempt"])
	// Closing the attempt does not bypass holds or the durable exact-version claim.
	sqlOK(t, c, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'attempt',$3,'upload')`, x["org"], orphan, x["attempt"])
	sqlReject(t, c, "23514", `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'orphan-version',gen_random_uuid(),now())`, x["org"], orphan)
	sqlOK(t, c, `UPDATE cosmoformer.artifact_holds SET released_at=now() WHERE artifact_id=$1`, orphan)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, orphan)
	sqlReject(t, c, "23503", `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'wrong-version',gen_random_uuid(),now())`, x["org"], orphan)
	sqlOK(t, c, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'orphan-version',gen_random_uuid(),now())`, x["org"], orphan)
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, orphan)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET object_key='other-object' WHERE id=$1`, orphan)
	sqlReject(t, c, "23514", `UPDATE cosmoformer.artifacts SET state='finalized',sha256=decode(repeat('ab',32),'hex'),byte_count=42,finalized_at=now() WHERE id=$1`, orphan)
	sqlReject(t, c, "23514", `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'attempt',$3,'reuse')`, x["org"], orphan, x["attempt"])
	sqlOK(t, c, `UPDATE cosmoformer.cleanup_claims SET status='failed',retry_at=now(),error_code='transient' WHERE artifact_id=$1`, orphan)
	sqlOK(t, c, `UPDATE cosmoformer.artifacts SET state='deleted',deleted_at=now(),tombstone='{"version_id":"orphan-version"}' WHERE id=$1`, orphan)
	sqlOK(t, c, `UPDATE cosmoformer.cleanup_claims SET status='deleted',completed_at=now() WHERE artifact_id=$1`, orphan)
	var truthful bool
	if err := c.QueryRow(evidenceCtx, `SELECT state='deleted' AND finalized_at IS NULL AND sha256 IS NULL AND byte_count IS NULL AND version_id='orphan-version' FROM cosmoformer.artifacts WHERE id=$1`, orphan).Scan(&truthful); err != nil || !truthful {
		t.Fatal("orphan cleanup fabricated finalized evidence", err)
	}
}
