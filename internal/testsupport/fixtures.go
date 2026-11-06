// Package testsupport supplies only isolated, explicitly marked test databases.
package testsupport

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

type Database struct {
	App      *store.Store
	Owner    *pgx.Conn
	OwnerDSN string
}

func NewDatabase(t *testing.T) *Database {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("COSMOFORMER_TEST_OWNER_DSN")
	markerFile := os.Getenv("COSMOFORMER_TEST_MARKER_FILE")
	if dsn == "" || markerFile == "" {
		t.Fatal("selected integration suite requires COSMOFORMER_TEST_OWNER_DSN and COSMOFORMER_TEST_MARKER_FILE; no application DSN fallback")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal("invalid test DSN")
	}
	if cfg.Database != "cosmoformer_test_control" || (cfg.Host != "127.0.0.1" && cfg.Host != "localhost" && !(os.Getenv("CI") == "true" && cfg.Host == "postgres")) {
		t.Fatal("test database must be the marked loopback/CI control database")
	}
	marker, e := os.ReadFile(markerFile)
	if e != nil || len(marker) != 64 {
		t.Fatal("missing test marker")
	}
	control, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		t.Fatal("cannot connect to isolated test control database")
	}
	defer control.Close(ctx)
	var stored string
	if e = control.QueryRow(ctx, `SELECT value FROM public.test_marker`).Scan(&stored); e != nil || stored != string(marker) {
		t.Fatal("test database marker mismatch")
	}
	var b [12]byte
	if _, e = rand.Read(b[:]); e != nil {
		t.Fatal(e)
	}
	name := "cf_test_" + hex.EncodeToString(b[:])
	if _, e = control.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER cosmoformer_owner"); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		c, e := pgx.ConnectConfig(context.Background(), cfg)
		if e != nil {
			t.Error("test DB cleanup connection failed")
			return
		}
		defer c.Close(context.Background())
		_, e = c.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if e != nil {
			t.Errorf("cleanup generated test DB: %v", e)
		}
	})
	ownerCfg := cfg.Copy()
	ownerCfg.Database = name
	ownerCfg.User = "cosmoformer_owner"
	ownerDSN := fmt.Sprintf("postgres://cosmoformer_owner@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, name)
	if e = store.Migrate(ctx, ownerDSN); e != nil {
		t.Fatal(e)
	}
	owner, e := pgx.ConnectConfig(ctx, ownerCfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { owner.Close(context.Background()) })
	appDSN := fmt.Sprintf("postgres://cosmoformer_app@%s:%d/%s?sslmode=disable", cfg.Host, cfg.Port, name)
	app, e := store.Open(ctx, appDSN)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Pool.Close)
	return &Database{app, owner, ownerDSN}
}

// BuildCommand builds the real entry point. Coverage runs instrument child
// binaries too, so operator commands do not disappear from the final report.
func BuildCommand(t *testing.T, name string) string {
	t.Helper()
	if name != "cosmoformer" && name != "contract" {
		t.Fatal("unsupported test command")
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test sources")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(source)))
	binary := filepath.Join(t.TempDir(), name)
	args := []string{"build", "-race", "-o", binary}
	if os.Getenv("COSMOFORMER_TEST_COVER_DIR") != "" {
		args = append(args, "-covermode=atomic")
	}
	args = append(args, "./cmd/"+name)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), args...)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test command: %v\n%s", err, output)
	}
	return binary
}

// Command never inherits application/operator configuration from the developer.
// Each test must supply its own isolated database files and listen address.
func Command(t *testing.T, binary string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, args...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "COSMOFORMER_") && !strings.HasPrefix(entry, "GOCOVERDIR=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	if dir := os.Getenv("COSMOFORMER_TEST_COVER_DIR"); dir != "" {
		cmd.Env = append(cmd.Env, "GOCOVERDIR="+dir)
	}
	return cmd
}
