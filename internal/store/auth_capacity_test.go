//go:build integration

package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

func TestAuthCapacityReservesLastSlot(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.auth_throttles(subject,failures,window_at) SELECT sha256(convert_to('synthetic-capacity/'||n::text,'UTF8')),0,clock_timestamp() FROM generate_series(1,9999) n`)
	type result struct {
		subject []byte
		wait    int
		err     error
	}
	results := make(chan result, 8)
	for i := range 8 {
		go func() {
			subject := identity.Digest(fmt.Sprintf("synthetic-contender/%d", i))
			wait, err := db.App.Rate(ctx, subject, "check")
			results <- result{subject, wait, err}
		}()
	}
	var admitted []byte
	accepted := 0
	for range 8 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.wait == 0 {
			accepted++
			admitted = r.subject
		} else if r.wait != 60 {
			t.Fatal("unexpected capacity wait", r.wait)
		}
	}
	var count, failures int
	if err := db.Owner.QueryRow(ctx, `SELECT count(*),(SELECT failures FROM cosmoformer.auth_throttles WHERE subject=$1) FROM cosmoformer.auth_throttles`, admitted).Scan(&count, &failures); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 || count != 10000 || failures != 0 {
		t.Fatal("last slot was not reserved atomically", accepted, count, failures)
	}
	if wait, err := db.App.Rate(ctx, admitted, "check"); err != nil || wait != 0 {
		t.Fatal("existing reservation was rejected", wait, err)
	}
	// The failure window starts at the first failed verification, not the
	// earlier reservation; check itself must not count as a failed password.
	sqlOK(t, db.Owner, `UPDATE cosmoformer.auth_throttles SET window_at=clock_timestamp()-interval '10 minutes' WHERE subject=$1`, admitted)
	if wait, err := db.App.Rate(ctx, admitted, "failure"); err != nil || wait != 0 {
		t.Fatal(wait, err)
	}
	var fresh bool
	if err := db.Owner.QueryRow(ctx, `SELECT failures=1 AND window_at>clock_timestamp()-interval '10 seconds' FROM cosmoformer.auth_throttles WHERE subject=$1`, admitted).Scan(&fresh); err != nil || !fresh {
		t.Fatal("first failure inherited reservation age", fresh, err)
	}
	if wait, err := db.App.Rate(ctx, admitted, "success"); err != nil || wait != 0 {
		t.Fatal(wait, err)
	}
	if wait, err := db.App.Rate(ctx, identity.Digest("synthetic-next-subject"), "check"); err != nil || wait != 0 {
		t.Fatal("successful login did not free capacity", wait, err)
	}
}

func TestAuthCapacityVersionSixteenUpgrade(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	raw, err := os.ReadFile("migrations/004_security.sql")
	if err != nil {
		t.Fatal(err)
	}
	legacy := string(raw)
	start := strings.Index(legacy, "CREATE FUNCTION auth_rate(")
	if start < 0 {
		t.Fatal("missing historical function")
	}
	end := strings.Index(legacy[start:], "\nEND $$;")
	if end < 0 {
		t.Fatal("missing historical function end")
	}
	legacy = strings.Replace(legacy[start:start+end+len("\nEND $$;")], "CREATE FUNCTION auth_rate(", "CREATE OR REPLACE FUNCTION cosmoformer.auth_rate(", 1)
	sqlOK(t, db.Owner, legacy)
	sqlOK(t, db.Owner, `ALTER FUNCTION cosmoformer.auth_rate(bytea,text) SET search_path=pg_catalog,cosmoformer,pg_temp; UPDATE cosmoformer.schema_version SET version=16`)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.auth_throttles(subject,failures,window_at) SELECT sha256(convert_to('synthetic-upgrade/'||n::text,'UTF8')),0,clock_timestamp() FROM generate_series(1,10000) n`)
	subject := identity.Digest("synthetic-upgrade-target")
	if wait, err := db.App.Rate(ctx, subject, "check"); err != nil || wait != 0 {
		t.Fatal("historical capacity condition not reproduced", wait, err)
	}
	if db.App.Ready(ctx) == nil {
		t.Fatal("new binary accepted the old schema")
	}
	if err := store.Migrate(ctx, db.OwnerDSN); err != nil {
		t.Fatal(err)
	}
	if err := db.App.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if wait, err := db.App.Rate(ctx, subject, "check"); err != nil || wait != 60 {
		t.Fatal("upgrade left cached admission unsafe", wait, err)
	}
}
