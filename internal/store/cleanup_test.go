//go:build integration

package store_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

type cleanupStorage struct {
	deleted, missing, fail, lost, wrong bool
	gets, deletes                       int
	key, version                        string
	outside                             func()
}

func (s *cleanupStorage) Check(context.Context) error {
	if s.outside != nil {
		s.outside()
	}
	return nil
}
func (s *cleanupStorage) Put(context.Context, string, io.Reader, int64) (string, error) {
	panic("cleanup cannot upload")
}
func (s *cleanupStorage) Get(_ context.Context, key, version string) (evidence.Object, error) {
	s.gets++
	s.key = key
	s.version = version
	if s.missing || s.deleted {
		return evidence.Object{}, evidence.ErrInvalid
	}
	if s.wrong {
		version = "wrong"
	}
	return evidence.Object{Version: version, Size: 4, Encryption: "AES256", Body: io.NopCloser(strings.NewReader("good"))}, nil
}
func (s *cleanupStorage) Delete(_ context.Context, key, version string) error {
	if s.outside != nil {
		s.outside()
	}
	s.deletes++
	if key != s.key || version != s.version {
		return errors.New("identity changed")
	}
	if s.fail {
		return evidence.ErrUnavailable
	}
	s.deleted = true
	if s.lost {
		s.lost = false
		return evidence.ErrUnavailable
	}
	return nil
}
func cleanupTx(t *testing.T, db *testsupport.Database, org string) evidence.Transaction {
	t.Helper()
	ctx := context.Background()
	return func(fn func(pgx.Tx) error) error {
		tx, e := db.App.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, org); e != nil {
			return e
		}
		if e = fn(tx); e != nil {
			return e
		}
		return tx.Commit(ctx)
	}
}
func cleanupArtifact(t *testing.T, db *testsupport.Database, x map[string]string, state string) string {
	t.Helper()
	id := runs.ID()
	var version any = "exact"
	if state == "uploading" {
		version = nil
	}
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.artifacts(id,org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,version_id,sha256,byte_count,state,finalized_at)
 VALUES($1,$2,$3,$4,'manifest',gen_random_uuid(),'synthetic',$1::uuid::text,$5,decode(repeat('ab',32),'hex'),4,$6,CASE WHEN $6='finalized' THEN clock_timestamp() ELSE NULL END)`, id, x["org"], x["attempt"], x["plan"], version, state)
	return id
}
func closeCleanupFixture(t *testing.T, db *testsupport.Database, x map[string]string, preservePlan ...bool) {
	t.Helper()
	sqlOK(t, db.Owner, `UPDATE cosmoformer.recovery_cases SET status='cleared',cleared_at=clock_timestamp(),cleared_by=$1 WHERE id=$2`, x["account"], x["recovery"])
	sqlOK(t, db.Owner, `UPDATE cosmoformer.attempts SET state='terminal',outcome='success',closed_at=clock_timestamp()-interval '31 days' WHERE id=$1`, x["attempt"])
	if len(preservePlan) == 0 || !preservePlan[0] {
		sqlOK(t, db.Owner, `UPDATE cosmoformer.plans SET status='invalidated' WHERE id=$1`, x["plan"])
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.operations SET status='succeeded' WHERE id=$1`, x["operation"])
	// The production trigger makes terminal time immutable. Test historical age
	// under the owner, then restore the trigger before runtime transactions.
	sqlOK(t, db.Owner, `ALTER TABLE cosmoformer.operations DISABLE TRIGGER operation_terminal_time`)
	sqlOK(t, db.Owner, `UPDATE cosmoformer.operations SET terminal_at=clock_timestamp()-interval '31 days' WHERE id=$1`, x["operation"])
	sqlOK(t, db.Owner, `ALTER TABLE cosmoformer.operations ENABLE TRIGGER operation_terminal_time`)
}
func TestCleanupExactVersionFailureRestartAndMissing(t *testing.T) {
	for _, scenario := range []string{"success", "orphan", "storage-failure", "lost-response", "restart-after-claim", "lost-outcome", "missing", "wrong-version", "wrong-bucket"} {
		t.Run(scenario, func(t *testing.T) {
			db := testsupport.NewDatabase(t)
			x := evidenceFixture(t, db)
			artifactState := "finalized"
			if scenario == "orphan" {
				artifactState = "uploading"
			}
			id := cleanupArtifact(t, db, x, artifactState)
			if scenario == "orphan" {
				sqlOK(t, db.Owner, `UPDATE cosmoformer.artifacts SET version_id='exact' WHERE id=$1`, id)
			}
			closeCleanupFixture(t, db, x)
			ctx := context.Background()
			tx := cleanupTx(t, db, x["org"])
			settings := evidence.DefaultCleanupSettings()
			var claims []string
			if e := tx(func(q pgx.Tx) error { var e error; claims, e = evidence.ClaimCleanup(ctx, q, settings, 1); return e }); e != nil || len(claims) != 1 {
				t.Fatal(claims, e)
			}
			st := &cleanupStorage{missing: scenario == "missing", fail: scenario == "storage-failure", lost: scenario == "lost-response", wrong: scenario == "wrong-version"}
			st.outside = func() {
				t.Helper()
				sqlOK(t, db.Owner, `SET lock_timeout='100ms'`)
				sqlOK(t, db.Owner, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`)
				sqlOK(t, db.Owner, `SET lock_timeout=0`)
			}
			custody := &evidence.Custody{Storage: st, Bucket: "synthetic"}
			if scenario == "wrong-bucket" {
				custody.Bucket = "other"
			}
			deletionTx := tx
			if scenario == "lost-outcome" {
				calls := 0
				deletionTx = func(fn func(pgx.Tx) error) error {
					calls++
					if calls == 3 {
						return errors.New("outcome commit lost")
					}
					return tx(fn)
				}
			}
			e := custody.DeleteClaim(ctx, deletionTx, claims[0])
			wantError := scenario != "success" && scenario != "restart-after-claim" && scenario != "orphan"
			if (e != nil) != wantError {
				t.Fatal(e)
			}
			if wantError && scenario != "missing" && scenario != "wrong-version" && scenario != "wrong-bucket" {
				st.fail = false
				// New custody instance, same durable claim and exact identity after restart.
				custody = &evidence.Custody{Storage: st, Bucket: "synthetic"}
				if e = custody.DeleteClaim(ctx, tx, claims[0]); e != nil {
					t.Fatal(e)
				}
			}
			if scenario == "wrong-version" || scenario == "wrong-bucket" {
				if st.deletes != 0 {
					t.Fatal("mismatched identity deleted")
				}
				return
			}
			var state, status string
			if e = tx(func(q pgx.Tx) error {
				return q.QueryRow(ctx, `SELECT a.state,c.status FROM cosmoformer.artifacts a JOIN cosmoformer.cleanup_claims c ON c.artifact_id=a.id WHERE a.id=$1 AND a.tombstone IS NOT NULL`, id).Scan(&state, &status)
			}); e != nil {
				t.Fatal(e)
			}
			if scenario == "missing" {
				if state != "unavailable" || status != "failed" || st.deletes != 0 {
					t.Fatal(state, status, st.deletes)
				}
			} else {
				if state != "deleted" || status != "deleted" {
					t.Fatal(state, status)
				}
			}
			before := st.deletes
			if e = custody.DeleteClaim(ctx, tx, claims[0]); e != nil || st.deletes != before {
				t.Fatal("completed replay repeated I/O", e)
			}
			if scenario == "lost-response" && (st.gets != 1 || st.deletes != 2) {
				t.Fatal("lost response did not reuse intent", st.gets, st.deletes)
			}
		})
	}
}
func TestCleanupMatrixRepairClaimsAndIsolation(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	ctx := context.Background()
	tx := cleanupTx(t, db, x["org"])
	id := cleanupArtifact(t, db, x, "finalized")
	var eligible bool
	check := func(want bool) {
		t.Helper()
		if e := tx(func(q pgx.Tx) error {
			return q.QueryRow(ctx, `SELECT cosmoformer.cleanup_eligible($1,30)`, id).Scan(&eligible)
		}); e != nil || eligible != want {
			t.Fatal("eligibility", eligible, want, e)
		}
	}
	check(false)
	// Active attempt, live operation and recovery are protected even with NO holds.
	if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
		t.Fatal(e)
	}
	var holds int
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT count(DISTINCT owner_kind) FROM cosmoformer.artifact_holds WHERE artifact_id=$1 AND released_at IS NULL`, id).Scan(&holds)
	}); e != nil || holds != 4 {
		t.Fatal("live matrix repair", holds, e)
	}
	closeCleanupFixture(t, db, x)
	if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
		t.Fatal(e)
	}
	check(true)
	// An explicit backup hold remains protected and runtime cannot release it.
	backup := runs.ID()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.backup_sets(id,cutoff,set_digest,artifact_count) VALUES($1,clock_timestamp(),decode(repeat('ab',32),'hex'),1)`, backup)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'backup',$3,'test')`, x["org"], id, backup)
	check(false)
	if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
		t.Fatal(e)
	}
	check(false)
	sqlOK(t, db.Owner, `UPDATE cosmoformer.backup_sets SET released_at=clock_timestamp() WHERE id=$1`, backup)
	sqlOK(t, db.Owner, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_id=$1`, backup)
	check(true)
	incomplete := cleanupArtifact(t, db, x, "uploading")
	var page evidence.CleanupPage
	if e := tx(func(q pgx.Tx) error {
		var e error
		page, e = evidence.PreviewCleanup(ctx, q, 30, "", "unresolved", 1)
		return e
	}); e != nil || len(page.Items) != 1 || page.Items[0].Artifact != incomplete {
		t.Fatal(page, e)
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.artifacts SET version_id='orphan-version' WHERE id=$1`, incomplete)
	// A different tenant cannot preview or execute this tenant's claims.
	other := evidenceFixture(t, db)
	otherTx := cleanupTx(t, db, other["org"])
	if e := otherTx(func(q pgx.Tx) error {
		var e error
		page, e = evidence.PreviewCleanup(ctx, q, 30, "", "eligible", 100)
		return e
	}); e != nil || len(page.Items) != 0 {
		t.Fatal("tenant leak", page, e)
	}
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	var claims []string
	if e := tx(func(q pgx.Tx) error {
		var e error
		claims, e = evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
		return e
	}); e != nil || len(claims) != 1 {
		t.Fatal(claims, e)
	}
	// Claimed evidence cannot acquire a new use in the opposite commit order.
	var claimedArtifact string
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT artifact_id::text FROM cosmoformer.cleanup_claims WHERE claim_id=$1`, claims[0]).Scan(&claimedArtifact)
	}); e != nil {
		t.Fatal(e)
	}
	if e := tx(func(q pgx.Tx) error {
		_, e := q.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'operation',$3,'late_apply')`, x["org"], claimedArtifact, x["operation"])
		return e
	}); e == nil {
		t.Fatal("new use admitted after claim")
	}
	// Reintroduce a live reference under owner to exercise corrupt metadata scan.
	sqlOK(t, db.Owner, `UPDATE cosmoformer.operations SET status='needs_attention' WHERE id=$1`, x["operation"])
	if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
		t.Fatal(e)
	}
	var blocked bool
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT cosmoformer.artifact_start_blocked($1)`, x["stack"]).Scan(&blocked)
	}); e != nil || !blocked {
		t.Fatal("claimed reference did not block starts", blocked, e)
	}
}
func TestCleanupMaintenanceAndFilteredPagination(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	ctx := context.Background()
	tx := cleanupTx(t, db, x["org"])
	for range 3 {
		cleanupArtifact(t, db, x, "finalized")
	}
	closeCleanupFixture(t, db, x)
	if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
		t.Fatal(e)
	}
	var page evidence.CleanupPage
	for i := 0; i < 3; i++ {
		after := page.Next
		if e := tx(func(q pgx.Tx) error {
			var e error
			page, e = evidence.PreviewCleanup(ctx, q, 30, after, "eligible", 1)
			return e
		}); e != nil || len(page.Items) != 1 || (page.Next != "") != (i < 2) {
			t.Fatal(i, page, e)
		}
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET maintenance=true`)
	if e := tx(func(q pgx.Tx) error {
		_, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
		return e
	}); !errors.Is(e, evidence.ErrUnavailable) {
		t.Fatal("maintenance admitted cleanup", e)
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET maintenance=false`)
	if e := tx(func(q pgx.Tx) error {
		_, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 101)
		return e
	}); e == nil {
		t.Fatal("unbounded batch")
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET offline=true`)
	if e := tx(func(q pgx.Tx) error {
		_, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
		return e
	}); !errors.Is(e, evidence.ErrUnavailable) {
		t.Fatal("offline admitted cleanup", e)
	}
}

func TestCleanupDeclaredBackupMissingHoldNeedsOwnerAttention(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	ctx := context.Background()
	id := cleanupArtifact(t, db, x, "finalized")
	closeCleanupFixture(t, db, x)
	backup := runs.ID()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.backup_sets(id,cutoff,set_digest,artifact_count) VALUES($1,clock_timestamp(),decode(repeat('ab',32),'hex'),1)`, backup)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.backup_artifacts(backup_id,org_id,artifact_id,bucket,object_key,version_id,sha256,byte_count) SELECT $1,org_id,id,bucket,object_key,version_id,sha256,byte_count FROM cosmoformer.artifacts WHERE id=$2`, backup, id)
	tx := cleanupTx(t, db, x["org"])
	if e := tx(func(q pgx.Tx) error {
		claims, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 100)
		if len(claims) != 0 {
			t.Fatal("declared backup was claimed")
		}
		return e
	}); e != nil {
		t.Fatal(e)
	}
	var attention bool
	var repaired int
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT cleanup_attention IS NOT NULL,(SELECT count(*) FROM cosmoformer.artifact_holds WHERE artifact_id=$1 AND owner_kind='backup' AND released_at IS NULL) FROM cosmoformer.artifacts WHERE id=$1`, id).Scan(&attention, &repaired)
	}); e != nil || !attention || repaired != 0 {
		t.Fatal("runtime repaired owner-only backup protection", attention, repaired, e)
	}
}
func TestCleanupUseAndClaimCommitOrders(t *testing.T) {
	for _, first := range []string{"use", "cleanup"} {
		t.Run(first, func(t *testing.T) {
			db := testsupport.NewDatabase(t)
			x := evidenceFixture(t, db)
			ctx := context.Background()
			id := cleanupArtifact(t, db, x, "finalized")
			closeCleanupFixture(t, db, x)
			tx := cleanupTx(t, db, x["org"])
			apply := runs.ID()
			newUse := func(q pgx.Tx) error {
				if _, e := q.Exec(ctx, `INSERT INTO cosmoformer.operations(id,org_id,run_id,stack_id,kind,plan_id,status,accepted_at,ordering_at,requester_id) VALUES($1,$2,$3,$4,'apply',$5,'queued',clock_timestamp(),clock_timestamp(),$6)`, apply, x["org"], x["run"], x["stack"], x["plan"], x["account"]); e != nil {
					return e
				}
				_, e := q.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'operation',$3,'saved_plan_apply')`, x["org"], id, apply)
				return e
			}
			claim := func(q pgx.Tx) error {
				claims, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
				if e == nil && (len(claims) == 1) != (first == "cleanup") {
					t.Fatal("wrong claim count", claims)
				}
				return e
			}
			if first == "use" {
				if e := tx(newUse); e != nil {
					t.Fatal(e)
				}
				if e := tx(claim); e != nil {
					t.Fatal(e)
				}
			} else {
				if e := tx(claim); e != nil {
					t.Fatal(e)
				}
				if e := tx(newUse); e == nil {
					t.Fatal("claimed version admitted a new use")
				}
				var count int
				if e := tx(func(q pgx.Tx) error {
					return q.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.operations WHERE id=$1`, apply).Scan(&count)
				}); e != nil || count != 0 {
					t.Fatal("rejected use left an accepted operation", count, e)
				}
			}
		})
	}
}

func TestCleanupAllArtifactKindsAndExpiredPlanHolds(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "usable", true: "expired"}[expired], func(t *testing.T) {
			db := testsupport.NewDatabase(t)
			x := evidenceFixture(t, db)
			ctx := context.Background()
			ids := []string{}
			for _, kind := range []string{"pre_init_spec", "binary_plan", "raw_plan_json", "provider_lock", "manifest", "provider_schema", "normalized_changes", "findings", "log_segment", "recovery_plan", "evidence_attachment"} {
				id := runs.ID()
				ids = append(ids, id)
				sqlOK(t, db.Owner, `INSERT INTO cosmoformer.artifacts(id,org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,version_id,sha256,byte_count,state,finalized_at) VALUES($1,$2,$3,$4,$5,gen_random_uuid(),'synthetic',$1::uuid::text,'exact',decode(repeat('ab',32),'hex'),4,'finalized',clock_timestamp())`, id, x["org"], x["attempt"], x["plan"], kind)
			}
			// Age alone must never remove uncertain execution's independent protection.
			sqlOK(t, db.Owner, `UPDATE cosmoformer.attempts SET state='uncertain' WHERE id=$1`, x["attempt"])
			tx := cleanupTx(t, db, x["org"])
			if e := tx(func(q pgx.Tx) error {
				claims, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 100)
				if len(claims) != 0 {
					t.Fatal("uncertain attempt was claimed")
				}
				return e
			}); e != nil {
				t.Fatal(e)
			}
			closeCleanupFixture(t, db, x, true)
			expiry := "1 hour"
			if expired {
				expiry = "-1 hour"
			}
			sqlOK(t, db.Owner, `UPDATE cosmoformer.plans SET status='ready',ready_at=clock_timestamp()-interval '31 days',expires_at=clock_timestamp()+$2::interval WHERE id=$1`, x["plan"], expiry)
			if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
				t.Fatal(e)
			}
			var count int
			if e := tx(func(q pgx.Tx) error {
				return q.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.artifacts WHERE id=ANY($1::uuid[]) AND cosmoformer.cleanup_eligible(id,30)`, ids).Scan(&count)
			}); e != nil {
				t.Fatal(e)
			}
			want := 1 // closed logs are historical; usable plan evidence remains held.
			if expired {
				want = len(ids)
			}
			if count != want {
				t.Fatal("artifact matrix eligibility", count, want)
			}
		})
	}
}
func TestCleanupMaintenanceQuiescesAdmissionAndSettlesInflightOutcome(t *testing.T) {
	for _, phase := range []string{"before-intent", "inflight"} {
		t.Run(phase, func(t *testing.T) {
			db := testsupport.NewDatabase(t)
			x := evidenceFixture(t, db)
			ctx := context.Background()
			cleanupArtifact(t, db, x, "finalized")
			closeCleanupFixture(t, db, x)
			tx := cleanupTx(t, db, x["org"])
			var claims []string
			if e := tx(func(q pgx.Tx) error {
				var e error
				claims, e = evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
				return e
			}); e != nil || len(claims) != 1 {
				t.Fatal(claims, e)
			}
			st := &cleanupStorage{}
			calls := 0
			st.outside = func() {
				calls++
				if calls == 1 && phase == "before-intent" || calls == 2 && phase == "inflight" {
					sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET maintenance=true`)
				}
			}
			c := &evidence.Custody{Storage: st, Bucket: "synthetic"}
			e := c.DeleteClaim(ctx, tx, claims[0])
			if phase == "before-intent" {
				if e == nil || st.deletes != 0 {
					t.Fatal("new storage deletion admitted during maintenance", e, st.deletes)
				}
			} else {
				if e != nil || st.deletes != 1 {
					t.Fatal("inflight outcome was not settled", e, st.deletes)
				}
			}
		})
	}
}

func TestCleanupVersionFifteenPopulatedUpgrade(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	sqlOK(t, db.Owner, `DROP SCHEMA cosmoformer CASCADE`)
	names, e := store.MigrationNames()
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "migrations/016_") {
			break
		}
		raw, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		sqlOK(t, db.Owner, string(raw))
	}
	sqlOK(t, db.Owner, `CREATE TABLE cosmoformer.schema_version(version integer NOT NULL); INSERT INTO cosmoformer.schema_version VALUES(15); GRANT SELECT ON cosmoformer.schema_version TO cosmoformer_app`)
	fixtures := []map[string]string{}
	for index := range 2 {
		x := evidenceFixture(t, db)
		sqlOK(t, db.Owner, `UPDATE cosmoformer.source_subjects SET kind='pr',external_key='42',facts='{"state":"OPEN"}',facts_revision=facts_revision+1 WHERE id=$1`, x["subject"])
		id := cleanupArtifact(t, db, x, "finalized")
		x["artifact"] = id
		fixtures = append(fixtures, x)
		sqlOK(t, db.Owner, `UPDATE cosmoformer.recovery_cases SET status='cleared',cleared_at=clock_timestamp(),cleared_by=$1 WHERE id=$2`, x["account"], x["recovery"])
		sqlOK(t, db.Owner, `UPDATE cosmoformer.attempts SET state='terminal',outcome='success',closed_at=clock_timestamp()-interval '40 days' WHERE id=$1`, x["attempt"])
		sqlOK(t, db.Owner, `UPDATE cosmoformer.plans SET status='invalidated' WHERE id=$1`, x["plan"])
		sqlOK(t, db.Owner, `UPDATE cosmoformer.operations SET status='succeeded' WHERE id=$1`, x["operation"])
		sqlOK(t, db.Owner, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE artifact_id=$1`, id)
		sqlOK(t, db.Owner, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'exact',gen_random_uuid(),clock_timestamp())`, x["org"], id)
		x["state"] = "finalized"
		if index == 0 {
			sqlOK(t, db.Owner, `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, id)
			x["state"] = "deleting"
		}
		var outbox string
		if e := db.Owner.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY id),'[]')::text FROM cosmoformer.outbox o`).Scan(&outbox); e != nil {
			t.Fatal(e)
		}
		x["outbox"] = outbox
	}
	// The migration connection starts without a tenant; owner cannot bypass FORCE
	// RLS. Both tenants must retain their claims and receive conservative ages.
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id','',false)`)
	if e := store.Migrate(ctx, db.OwnerDSN); e != nil {
		t.Fatal(e)
	}
	for _, x := range fixtures {
		tx := cleanupTx(t, db, x["org"])
		var outbox string
		if e := tx(func(q pgx.Tx) error {
			return q.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(o) ORDER BY id),'[]')::text FROM cosmoformer.outbox o`).Scan(&outbox)
		}); e != nil || outbox != x["outbox"] {
			t.Fatal("upgrade changed existing delivery metadata", e)
		}
		var good bool
		if e := tx(func(q pgx.Tx) error {
			return q.QueryRow(ctx, `SELECT o.terminal_at>clock_timestamp()-interval '1 minute' AND a.state=$2 AND c.bucket=a.bucket AND c.object_key=a.object_key AND c.exact_version=a.version_id AND c.status='claimed' FROM cosmoformer.operations o JOIN cosmoformer.artifacts a ON a.operation_id=o.id JOIN cosmoformer.cleanup_claims c ON c.artifact_id=a.id WHERE a.id=$1`, x["artifact"], x["state"]).Scan(&good)
		}); e != nil || !good {
			t.Fatal("populated tenant upgrade lost custody/age metadata", good, e)
		}
		if x["state"] == "finalized" {
			storage := &cleanupStorage{}
			c := &evidence.Custody{Storage: storage, Bucket: "synthetic"}
			if _, file, e := c.Read(ctx, tx, x["artifact"]); e == nil || file != nil || storage.gets != 0 {
				t.Fatal("upgrade reopened claimed evidence", e)
			}
		}
	}
	if e := db.App.Ready(ctx); e != nil {
		t.Fatal(e)
	}
}

func TestCleanupArchivedVersionsDoNotInventLaterRecoveryUses(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	ctx := context.Background()
	old := cleanupArtifact(t, db, x, "finalized")
	closeCleanupFixture(t, db, x)
	tx := cleanupTx(t, db, x["org"])
	var claims []string
	if e := tx(func(q pgx.Tx) error {
		var e error
		claims, e = evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
		return e
	}); e != nil || len(claims) != 1 {
		t.Fatal(claims, e)
	}
	c := &evidence.Custody{Storage: &cleanupStorage{}, Bucket: "synthetic"}
	if e := c.DeleteClaim(ctx, tx, claims[0]); e != nil {
		t.Fatal(e)
	}
	recovery := runs.ID()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.recovery_cases(id,org_id,stack_id,target_id,origin_attempt_id,status,evidence,opened_at) VALUES($1,$2,$3,$4,$5,'open','{}',clock_timestamp())`, recovery, x["org"], x["stack"], x["target"], nil)
	current := cleanupArtifact(t, db, x, "finalized")
	if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
		t.Fatal(e)
	}
	var oldAttention, blocked, protected bool
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT
 (SELECT cleanup_attention IS NOT NULL FROM cosmoformer.artifacts WHERE id=$1),cosmoformer.artifact_start_blocked($2),
 EXISTS(SELECT FROM cosmoformer.artifact_holds WHERE artifact_id=$3 AND owner_kind='recovery' AND owner_id=$4 AND released_at IS NULL)`, old, x["stack"], current, recovery).Scan(&oldAttention, &blocked, &protected)
	}); e != nil || oldAttention || blocked || !protected {
		t.Fatal("later recovery invented a use of deleted history or failed to hold available evidence", oldAttention, blocked, protected, e)
	}
}
