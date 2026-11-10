//go:build integration

package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

func quotaConnection(t *testing.T) (*testsupport.Database, *pgx.Conn, []byte) {
	t.Helper()
	db := testsupport.NewDatabase(t)
	fingerprint := bytes.Repeat([]byte{0x31}, 32)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state) VALUES($1,'active')`, fingerprint)
	sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET active_key_fingerprint=$1`, fingerprint)
	// Authenticate as the actual runtime role, without SET ROLE from an owner.
	c, err := pgx.ConnectConfig(context.Background(), db.App.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return db, c, fingerprint
}

func temporaryQuota(t *testing.T, c *pgx.Conn, fingerprint []byte) {
	t.Helper()
	sqlOK(t, c, `CREATE TEMP TABLE crypto_keys(fingerprint bytea PRIMARY KEY,reserved_encryptions bigint,state text)`)
	sqlOK(t, c, `INSERT INTO pg_temp.crypto_keys VALUES($1,0,'active')`, fingerprint)
	sqlOK(t, c, `GRANT SELECT,UPDATE ON pg_temp.crypto_keys TO cosmoformer_owner`)
}

func assertQuota(t *testing.T, db *testsupport.Database, c *pgx.Conn, want int64) {
	t.Helper()
	var permanent, temporary int64
	if err := db.Owner.QueryRow(context.Background(), `SELECT reserved_encryptions FROM cosmoformer.crypto_keys`).Scan(&permanent); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(context.Background(), `SELECT reserved_encryptions FROM pg_temp.crypto_keys`).Scan(&temporary); err != nil {
		t.Fatal(err)
	}
	if permanent != want || temporary != 0 {
		t.Fatalf("reservation used wrong relation: permanent=%d, temporary=%d, want permanent=%d", permanent, temporary, want)
	}
}

func TestDefinerQuotaUsesPermanentRelations(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "cached"}[cached], func(t *testing.T) {
			db, c, fingerprint := quotaConnection(t)
			var want int64 = 1
			if cached {
				sqlOK(t, c, `SELECT cosmoformer.reserve_encryptions($1,1)`, fingerprint)
				want++
			}
			// Ordinary colliding data only: no injected functions or triggers.
			temporaryQuota(t, c, fingerprint)
			sqlReject(t, c, "42501", `UPDATE cosmoformer.crypto_keys SET reserved_encryptions=0`)
			sqlOK(t, c, `SET search_path=pg_temp,public,cosmoformer,pg_catalog`)
			sqlOK(t, c, `SELECT cosmoformer.reserve_encryptions($1,1)`, fingerprint)
			assertQuota(t, db, c, want)
		})
	}
}

func TestDefinerNamespaceReadiness(t *testing.T) {
	db := testsupport.NewDatabase(t)
	for _, setting := range []string{
		`RESET search_path`,
		`SET search_path=pg_catalog,cosmoformer`,
		`SET search_path=pg_temp,pg_catalog,cosmoformer`,
		`SET search_path=pg_catalog,public,cosmoformer,pg_temp`,
	} {
		sqlOK(t, db.Owner, `ALTER FUNCTION cosmoformer.reserve_encryptions(bytea,bigint) `+setting)
		if err := db.App.Ready(context.Background()); err == nil {
			t.Fatalf("readiness accepted unsafe definer setting: %s", setting)
		}
	}
	sqlOK(t, db.Owner, `ALTER FUNCTION cosmoformer.reserve_encryptions(bytea,bigint) SET search_path=pg_catalog,cosmoformer,pg_temp`)
	if err := db.App.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVersionEightDefinerUpgrade(t *testing.T) {
	db, c, fingerprint := quotaConnection(t)
	// Restore the exact version-8 function setting without rewriting migrations.
	sqlOK(t, db.Owner, `
 DROP FUNCTION cosmoformer.approval_policy_revision() CASCADE;
 ALTER TABLE cosmoformer.projects DROP COLUMN approval_policy_revision;
 DROP FUNCTION cosmoformer.approval_event_integrity() CASCADE;
 ALTER TABLE cosmoformer.approvals DROP COLUMN manifest_digest, DROP COLUMN source_commit;
 DROP FUNCTION cosmoformer.plan_vote_integrity() CASCADE;
 DROP FUNCTION cosmoformer.invalidate_reviewed_input() CASCADE;
 DROP FUNCTION cosmoformer.invalidate_runtime_plans() CASCADE;
 DROP FUNCTION cosmoformer.release_invalid_plan_holds() CASCADE;
 DROP FUNCTION cosmoformer.invalidate_epoch_plans() CASCADE;
 DO $$ DECLARE f regprocedure; BEGIN
 FOR f IN SELECT p.oid::regprocedure FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='cosmoformer' AND p.prosecdef LOOP
 EXECUTE format('ALTER FUNCTION %s SET search_path=pg_catalog,cosmoformer',f);
 END LOOP; END $$;
 DROP TRIGGER ready_plan_identity ON cosmoformer.plans;
 DROP FUNCTION cosmoformer.ready_plan_identity();
 DROP TRIGGER planning_intent_identity ON cosmoformer.operations;
 DROP FUNCTION cosmoformer.planning_intent_identity();
 ALTER TABLE cosmoformer.operations DROP COLUMN native_planning;
 UPDATE cosmoformer.schema_version SET version=8`)
	snapshot := func() string {
		t.Helper()
		var value string
		if err := db.Owner.QueryRow(context.Background(), `SELECT jsonb_agg(jsonb_build_array(p.oid,p.proowner,p.proacl,p.prosrc) ORDER BY p.oid)::text FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='cosmoformer' AND p.prosecdef AND p.proname NOT IN ('invalidate_runtime_plans','invalidate_epoch_plans')`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := snapshot()
	sqlOK(t, c, `SELECT cosmoformer.reserve_encryptions($1,1)`, fingerprint)
	temporaryQuota(t, c, fingerprint)
	if err := db.App.Ready(context.Background()); err == nil {
		t.Fatal("runtime accepted schema version 8")
	}
	if err := store.Migrate(context.Background(), db.OwnerDSN); err != nil {
		t.Fatal(err)
	}
	if snapshot() != before {
		t.Fatal("upgrade changed function identity, owner, body or grants")
	}
	if err := db.App.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The old connection has a warmed function plan and a colliding temp table.
	sqlOK(t, c, `SELECT cosmoformer.reserve_encryptions($1,1)`, fingerprint)
	assertQuota(t, db, c, 2)
	if err := store.Migrate(context.Background(), db.OwnerDSN); err != nil {
		t.Fatal("migration replay", err)
	}
}
