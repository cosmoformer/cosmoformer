//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

func TestVersionTenApprovalUpgrade(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	sqlOK(t, db.Owner, `DROP SCHEMA cosmoformer CASCADE`)
	names, e := store.MigrationNames()
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range names[:10] {
		raw, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		sqlOK(t, db.Owner, string(raw))
	}
	sqlOK(t, db.Owner, `CREATE TABLE cosmoformer.schema_version(version integer NOT NULL); INSERT INTO cosmoformer.schema_version VALUES(10); GRANT SELECT ON cosmoformer.schema_version TO cosmoformer_app`)
	org, project := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.organizations(id,name) VALUES($1,'Synthetic upgrade')`, org)
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, org)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.projects(id,org_id,name,minimum_approvals,revision) VALUES($1,$2,'Existing approval policy',2,7)`, project, org)
	if e = store.Migrate(ctx, db.OwnerDSN); e != nil {
		t.Fatal(e)
	}
	if e = db.App.Ready(ctx); e != nil {
		t.Fatal(e)
	}
	var floor int
	var rev, policy int64
	if e = db.Owner.QueryRow(ctx, `SELECT minimum_approvals,revision,approval_policy_revision FROM cosmoformer.projects WHERE id=$1`, project).Scan(&floor, &rev, &policy); e != nil || floor != 2 || rev != 7 || policy != 7 {
		t.Fatal("existing policy changed", floor, rev, policy, e)
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.projects SET name='Cosmetic rename',revision=revision+1 WHERE id=$1`, project)
	if e = db.Owner.QueryRow(ctx, `SELECT approval_policy_revision FROM cosmoformer.projects WHERE id=$1`, project).Scan(&policy); e != nil || policy != 7 {
		t.Fatal("label changed approval policy", policy, e)
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.projects SET minimum_approvals=3,revision=revision+1 WHERE id=$1`, project)
	if e = db.Owner.QueryRow(ctx, `SELECT approval_policy_revision FROM cosmoformer.projects WHERE id=$1`, project).Scan(&policy); e != nil || policy != 8 {
		t.Fatal("floor did not advance approval policy", policy, e)
	}
	if e = store.Migrate(ctx, db.OwnerDSN); e != nil {
		t.Fatal("migration replay", e)
	}
	sqlOK(t, db.Owner, `ALTER TABLE cosmoformer.approvals DISABLE TRIGGER approval_event_integrity`)
	if e = db.App.Ready(ctx); e == nil {
		t.Fatal("startup accepted mutable approval history")
	}
}
