//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRuntimeCannotMaskAPrivilegedLoginWithTheApplicationRole(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(db.OwnerDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = "postgres" // The disposable, marker-verified test administrator.
	cfg.ConnConfig.RuntimeParams["role"] = "cosmoformer_app"
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var login, role string
	if err = pool.QueryRow(ctx, `SELECT session_user::text,current_user::text`).Scan(&login, &role); err != nil {
		t.Fatal(err)
	}
	if login != "postgres" || role != "cosmoformer_app" {
		t.Fatal("masked-role fixture not established")
	}
	if err = (&store.Store{Pool: pool}).Ready(ctx); err == nil {
		t.Fatal("runtime accepted a masked privileged login")
	}
}
