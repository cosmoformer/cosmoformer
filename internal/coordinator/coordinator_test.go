//go:build integration

package coordinator_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

func seed(t *testing.T, db *testsupport.Database) (string, string, string) {
	t.Helper()
	ctx := context.Background()
	org, project, env, integration, repo, stack, rev := runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID(), runs.ID()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO cosmoformer.organizations(id,name) VALUES($1,'Synthetic')`, []any{org}},
		{`SELECT set_config('app.org_id',$1,false)`, []any{org}},
		{`INSERT INTO cosmoformer.projects(id,org_id,name) VALUES($1,$2,'Synthetic')`, []any{project, org}},
		{`INSERT INTO cosmoformer.environments(id,org_id,project_id,name) VALUES($1,$2,$3,'Synthetic')`, []any{env, org, project}},
		{`INSERT INTO cosmoformer.integrations(id,org_id,name,provider) VALUES($1,$2,'Synthetic','bitbucket_cloud')`, []any{integration, org}},
		{`INSERT INTO cosmoformer.repositories(id,org_id,project_id,integration_id,external_id,url,tracked_branch) VALUES($1,$2,$3,$4,'fixture','https://bitbucket.org/fictional/repo.git','main')`, []any{repo, org, project, integration}},
		{`INSERT INTO cosmoformer.stacks(id,org_id,project_id,environment_id,repository_id,declaration_id,display_name) VALUES($1,$2,$3,$4,$5,$1,'Synthetic')`, []any{stack, org, project, env, repo}},
		{`INSERT INTO cosmoformer.stack_revisions(id,org_id,stack_id,commit,parsed_config,effective_config,semantic_digest,inventory_revision) VALUES($1,$2,$3,$4,'{}','{}',$5,1)`, []any{rev, org, stack, strings.Repeat("a", 40), []byte(strings.Repeat("a", 32))}},
		{`UPDATE cosmoformer.stacks SET effective_revision_id=$2 WHERE id=$1`, []any{stack, rev}},
	}
	for _, s := range statements {
		if _, e := db.Owner.Exec(ctx, s.sql, s.args...); e != nil {
			t.Fatal(e)
		}
	}
	return org, stack, repo
}
func tenantTx(t *testing.T, db *testsupport.Database, org string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, e := db.App.Pool.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, org); e != nil {
		t.Fatal(e)
	}
	return tx
}
func TestPermanentBindingCollisionAndRollback(t *testing.T) {
	db := testsupport.NewDatabase(t)
	org, first, _ := seed(t, db)
	other, second, _ := seed(t, db)
	ctx := context.Background()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, s := range []string{first, second} {
		tenant := []string{org, other}[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tx, e := db.App.Pool.Begin(ctx)
			if e != nil {
				results <- e
				return
			}
			defer tx.Rollback(ctx)
			_, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, tenant)
			if e == nil {
				_, e = tx.Exec(ctx, `SELECT cosmoformer.bind_backend($1,$2,'aws','fictional-bucket','state.tfstate','s3',NULL)`, tenant, s)
			}
			if e == nil {
				e = tx.Commit(ctx)
			}
			results <- e
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatal("concurrent duplicate owners", success)
	}
	tx := tenantTx(t, db, org)
	defer tx.Rollback(ctx)
	var hidden bool
	if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.stack_bindings WHERE org_id=$1)`, other).Scan(&hidden); e != nil || hidden {
		t.Fatal("cross-tenant binding exposed", e)
	}
	tx.Rollback(ctx)
	var owner string
	if e := db.Owner.QueryRow(ctx, `SELECT target_id::text FROM cosmoformer.backend_resource_claims WHERE namespace='aws/fictional-bucket' AND resource_key='state.tfstate'`).Scan(&owner); e != nil {
		t.Fatal(e)
	}
	var winningOrg, losingOrg, loser string
	if e := db.Owner.QueryRow(ctx, `SELECT org_id::text FROM cosmoformer.stack_bindings WHERE target_id=$1`, owner).Scan(&winningOrg); e != nil { // FORCE RLS: try each known tenant, never disable RLS.
		for _, tenant := range []string{org, other} {
			db.Owner.Exec(ctx, `SELECT set_config('app.org_id',$1,false)`, tenant)
			db.Owner.QueryRow(ctx, `SELECT org_id::text FROM cosmoformer.stack_bindings WHERE target_id=$1`, owner).Scan(&winningOrg)
		}
	}
	if winningOrg == org {
		losingOrg = other
		loser = second
	} else {
		losingOrg = org
		loser = first
	}
	fail := tenantTx(t, db, losingOrg)
	_, e := fail.Exec(ctx, `SELECT cosmoformer.bind_backend($1,$2,'aws','fictional-bucket','state.tfstate.tflock','s3',NULL)`, losingOrg, loser)
	if e == nil {
		t.Fatal("state/lock collision accepted")
	}
	fail.Rollback(ctx)
	var count int
	if e = db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.backend_targets`).Scan(&count); e != nil || count != 1 {
		t.Fatal("failed multi-claim left target", count, e)
	}
}

type elapsed struct{ now time.Duration }

func (c *elapsed) Now() (time.Duration, error) { return c.now, nil }
func TestObservationReverseArrivalAndNoABARevival(t *testing.T) {
	db := testsupport.NewDatabase(t)
	org, _, repo := seed(t, db)
	ctx := context.Background()
	clock := &elapsed{time.Second}
	c := coordinator.New(db.App, clock)
	begin := func() coordinator.Observation {
		tx := tenantTx(t, db, org)
		defer tx.Rollback(ctx)
		tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`)
		o, e := c.BeginObservation(ctx, tx, org, repo, "branch", "main")
		if e != nil {
			t.Fatal(e)
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		return o
	}
	accept := func(o coordinator.Observation, commit string) (int64, error) {
		tx := tenantTx(t, db, org)
		defer tx.Rollback(ctx)
		tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`)
		rev, e := c.AcceptObservation(ctx, tx, org, o, bitbucket.Facts{RepositoryID: "fixture", SourceRepositoryID: "fixture", Commit: commit, Branch: "main", Destination: "main", State: "OPEN"})
		if e == nil {
			e = tx.Commit(ctx)
		}
		return rev, e
	}
	old, newer := begin(), begin()
	revision, e := accept(newer, strings.Repeat("b", 40))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = accept(old, strings.Repeat("a", 40)); e == nil {
		t.Fatal("reverse arrival overwrote facts")
	}
	next, e := accept(begin(), strings.Repeat("a", 40))
	if e != nil || next <= revision {
		t.Fatal("ABA restored old revision", next, e)
	}
}
func TestEncryptionQuotaIsDurableAndBounded(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	fingerprint := []byte(strings.Repeat("k", 32))
	if _, e := db.Owner.Exec(ctx, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state,reserved_encryptions) VALUES($1,'active',4294967295)`, fingerprint); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Owner.Exec(ctx, `UPDATE cosmoformer.installation SET active_key_fingerprint=$1`, fingerprint); e != nil {
		t.Fatal(e)
	}
	if _, e := db.App.Pool.Exec(ctx, `SELECT cosmoformer.reserve_encryptions($1,1)`, fingerprint); e != nil {
		t.Fatal(e)
	}
	if _, e := db.App.Pool.Exec(ctx, `SELECT cosmoformer.reserve_encryptions($1,1)`, fingerprint); e == nil {
		t.Fatal("encryption quota exceeded")
	}
	var count int64
	if e := db.Owner.QueryRow(ctx, `SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, fingerprint).Scan(&count); e != nil || count != 4294967296 {
		t.Fatal("quota reservation not retained", count, e)
	}
}
