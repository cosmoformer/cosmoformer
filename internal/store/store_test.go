//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

func TestRuntimeRoleAndTenantIsolation(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	if _, e := db.App.Pool.Exec(ctx, `SELECT * FROM cosmoformer.accounts`); e == nil {
		t.Fatal("runtime can select global account verifiers")
	}
	if _, e := db.App.Pool.Exec(ctx, `SET ROLE cosmoformer_owner`); e == nil {
		t.Fatal("runtime can become owner")
	}
	org := "11111111-1111-4111-8111-111111111111"
	if _, e := db.Owner.Exec(ctx, `INSERT INTO cosmoformer.organizations(id,name) VALUES($1,'Fictional')`, org); e != nil {
		t.Fatal(e)
	}
	tx, e := db.App.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, org); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.projects(org_id,name) VALUES($1,'Private')`, org); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = db.App.Pool.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.projects`).Scan(&count); e != nil || count != 0 {
		t.Fatal("unscoped pooled read", count, e)
	}
	tx, e = db.App.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id','22222222-2222-4222-8222-222222222222',true)`); e != nil {
		t.Fatal(e)
	}
	if e = tx.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.projects`).Scan(&count); e != nil || count != 0 {
		t.Fatal("cross-org read", count, e)
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.projects(org_id,name) VALUES($1,'Escape')`, org); e == nil {
		t.Fatal("cross-org write accepted")
	}
}

func TestGlobalStateAndLockClaims(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	var first, second string
	if e := db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.backend_targets(partition,bucket,effective_key) VALUES('aws','fictional-test-bucket','state.tfstate') RETURNING id::text`).Scan(&first); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Owner.Exec(ctx, `INSERT INTO cosmoformer.backend_targets(partition,bucket,effective_key) VALUES('aws','fictional-test-bucket','state.tfstate')`); e == nil {
		t.Fatal("duplicate canonical state target accepted")
	}
	if e := db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.backend_targets(partition,bucket,effective_key) VALUES('aws','fictional-test-bucket','state.tfstate.tflock') RETURNING id::text`).Scan(&second); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Owner.Exec(ctx, `INSERT INTO cosmoformer.backend_resource_claims(target_id,kind,namespace,resource_key,purpose) VALUES($1,'s3_object','aws/fictional-test-bucket','state.tfstate.tflock','lock')`, first); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Owner.Exec(ctx, `INSERT INTO cosmoformer.backend_resource_claims(target_id,kind,namespace,resource_key,purpose) VALUES($1,'s3_object','aws/fictional-test-bucket','state.tfstate.tflock','state')`, second); e == nil {
		t.Fatal("state object collided with another target's lock claim")
	}
	if _, e := db.App.Pool.Exec(ctx, `SELECT * FROM cosmoformer.backend_targets`); e == nil {
		t.Fatal("runtime can enumerate global target custody")
	}
}
