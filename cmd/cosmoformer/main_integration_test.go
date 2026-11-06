//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

func protectedDSN(t *testing.T, dsn string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "database.dsn")
	if err := os.WriteFile(file, []byte(dsn), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}
func operator(t *testing.T, binary, dsnFile string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := testsupport.Command(t, binary, args...)
	cmd.Env = append(cmd.Env, "COSMOFORMER_MIGRATION_DSN_FILE="+dsnFile)
	return cmd
}

func TestMigrationCommand(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	owner := protectedDSN(t, db.OwnerDSN)
	ctx := context.Background()
	// This schema belongs only to the marker-verified, generated test database.
	if _, err := db.Owner.Exec(ctx, `DROP SCHEMA cosmoformer CASCADE`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		output, err := operator(t, binary, owner, "migrate").CombinedOutput()
		requireExit(t, output, err, 0, "Database schema is current (version 4).")
	}
	if err := db.App.Ready(ctx); err != nil {
		t.Fatal("CLI migrations did not provision a usable runtime schema", err)
	}
	runtime := protectedDSN(t, db.App.Pool.Config().ConnString())
	output, err := operator(t, binary, runtime, "migrate").CombinedOutput()
	requireExit(t, output, err, 1, "operator-provisioned owner/app roles are missing or unsafe")
	if _, err := db.Owner.Exec(ctx, `UPDATE cosmoformer.schema_version SET version=5`); err != nil {
		t.Fatal(err)
	}
	output, err = operator(t, binary, owner, "migrate").CombinedOutput()
	requireExit(t, output, err, 1, "FORWARD_ONLY_MIGRATIONS: database is newer than this binary")
	var version int
	if err := db.Owner.QueryRow(ctx, `SELECT version FROM cosmoformer.schema_version`).Scan(&version); err != nil || version != 5 {
		t.Fatal("newer schema was modified", err)
	}
}

func TestOperatorCommandRejectsWrongAuthorityAndAccounts(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	owner := protectedDSN(t, db.OwnerDSN)
	runtime := protectedDSN(t, db.App.Pool.Config().ConnString())
	if _, err := db.Owner.Exec(context.Background(), `INSERT INTO cosmoformer.accounts(username,display_name,enabled,installation_admin) VALUES('disabled','Disabled',false,true),('member','Member',true,false)`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, command, user, dsn, message string }{
		{"runtime cannot bootstrap", "bootstrap-admin", "admin", runtime, "requires the migration owner"},
		{"runtime cannot recover", "recover-admin", "admin", runtime, "requires the migration owner"},
		{"unknown account", "recover-admin", "unknown", owner, "cannot bootstrap/recover"},
		{"disabled administrator", "recover-admin", "disabled", owner, "cannot bootstrap/recover"},
		{"ordinary member", "recover-admin", "member", owner, "cannot bootstrap/recover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "proof")
			output, err := operator(t, binary, tc.dsn, tc.command, "--username", tc.user, "--setup-code-file", file).CombinedOutput()
			requireExit(t, output, err, 1, tc.message)
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("rejected command retained a proof output")
			}
		})
	}
	var proofs int
	if err := db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.account_proofs`).Scan(&proofs); err != nil || proofs != 0 {
		t.Fatal("rejected commands created a proof", err)
	}
}

func TestRecoveryRevokesProofFromInFlightRequest(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hasher := identity.NewHasher()
	phc, err := hasher.Hash(ctx, "Dummy administrator password")
	if err != nil {
		t.Fatal(err)
	}
	var account string
	if err := db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.accounts(username,display_name,installation_admin,password_phc) VALUES('admin','Administrator',true,$1) RETURNING id::text`, phc).Scan(&account); err != nil {
		t.Fatal(err)
	}
	session := identity.Token()
	sessionHash := identity.Digest(session)
	if accepted, err := db.App.CreateSession(ctx, account, 1, sessionHash, identity.Digest(identity.CSRF(session))); err != nil || !accepted {
		t.Fatal("could not create fixture session", err)
	}

	owner := protectedDSN(t, db.OwnerDSN+"&application_name=cosmoformer-recovery-race-test")
	proofFile := filepath.Join(t.TempDir(), "recovery-code")
	cmd := operator(t, binary, owner, "recover-admin", "--username", "admin", "--setup-code-file", proofFile)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	done := make(chan error, 1)
	waited := false
	requestCode := identity.Token()
	// Pause a real authenticated transaction after it has loaded the old actor.
	// Observe the recovery process waiting in PostgreSQL instead of relying on a sleep.
	err = db.App.ActorTx(ctx, sessionHash, "", func(tx pgx.Tx, actor store.Actor) error {
		var pid int
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			if !waited {
				cmd.Process.Kill()
				<-done
			}
		})
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var blocked bool
			if err := db.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE datname=current_database() AND application_name='cosmoformer-recovery-race-test' AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
				return err
			}
			if blocked {
				break
			}
			select {
			case err := <-done:
				waited = true
				t.Fatalf("recovery exited before waiting for the authenticated request: %v", err)
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
		var accepted bool
		if err := tx.QueryRow(ctx, `SELECT cosmoformer.issue_proof($1,$2,'reset',$3)`, actor.ID, account, identity.Digest(requestCode)).Scan(&accepted); err != nil {
			return err
		}
		if !accepted {
			t.Fatal("in-flight request could not issue its proof")
		}
		return nil
	})
	if err != nil {
		t.Fatal("authenticated request failed", err)
	}
	select {
	case err := <-done:
		waited = true
		requireExit(t, output.Bytes(), err, 0, "One-use reset code saved")
	case <-ctx.Done():
		t.Fatal("recovery did not finish after the request committed")
	}
	if _, err := db.App.Session(ctx, sessionHash); err != store.ErrUnauthenticated {
		t.Fatal("recovery retained the old session", err)
	}
	if accepted, err := db.App.ConsumeProof(ctx, identity.Digest(requestCode), "reset", phc); err != nil || accepted {
		t.Fatal("recovery retained the in-flight request's proof", err)
	}
	code, err := os.ReadFile(proofFile)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, err := db.App.ConsumeProof(ctx, identity.Digest(strings.TrimSpace(string(code))), "reset", phc); err != nil || !accepted {
		t.Fatal("operator recovery proof was not usable", err)
	}
}

func serving(t *testing.T, binary, dsn, listen string) *exec.Cmd {
	t.Helper()
	cmd := testsupport.Command(t, binary, "serve")
	cmd.Env = append(cmd.Env, "COSMOFORMER_ORIGIN=https://localhost:5173", "COSMOFORMER_LISTEN="+listen, "COSMOFORMER_DATABASE_DSN_FILE="+dsn)
	return cmd
}

func TestServeCommandRefusesUnsafeStartup(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	owner := protectedDSN(t, db.OwnerDSN)
	app := protectedDSN(t, db.App.Pool.Config().ConnString())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, tc := range []struct{ name, dsn, listen, message string }{
		{"invalid address", app, "invalid", "invalid listen address"},
		{"missing protected input", filepath.Join(t.TempDir(), "missing"), "127.0.0.1:0", "cannot open protected input file"},
		{"privileged runtime", owner, "127.0.0.1:0", "incompatible privileges"},
		{"occupied port", app, listener.Addr().String(), "cannot bind API listen address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := serving(t, binary, tc.dsn, tc.listen).CombinedOutput()
			requireExit(t, output, err, 1, tc.message)
		})
	}
	if _, err := db.Owner.Exec(context.Background(), `UPDATE cosmoformer.schema_version SET version=3`); err != nil {
		t.Fatal(err)
	}
	output, err := serving(t, binary, app, "127.0.0.1:0").CombinedOutput()
	requireExit(t, output, err, 1, "unsupported database schema; run explicit migrations")
	var version int
	if err := db.Owner.QueryRow(context.Background(), `SELECT version FROM cosmoformer.schema_version`).Scan(&version); err != nil || version != 3 {
		t.Fatal("serve migrated implicitly", err)
	}
}

func TestServeCommandStartsAndShutsDown(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	app := protectedDSN(t, db.App.Pool.Config().ConnString())
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
		t.Run(sig.String(), func(t *testing.T) {
			// Reserve a loopback address, then release it for the real child process.
			reserve, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := reserve.Addr().String()
			if err := reserve.Close(); err != nil {
				t.Fatal(err)
			}
			cmd := serving(t, binary, app, address)
			var output bytes.Buffer
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			t.Cleanup(func() {
				if !waited {
					cmd.Process.Kill()
					<-done
				}
			})
			client := &http.Client{Timeout: 250 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
			deadline := time.Now().Add(8 * time.Second)
			for {
				response, err := client.Get("http://" + address + "/health/ready")
				if err == nil {
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if response.StatusCode == 200 {
						break
					}
				}
				select {
				case err := <-done:
					waited = true
					t.Fatalf("API exited before readiness: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("API did not become ready")
				}
				time.Sleep(20 * time.Millisecond)
			}
			response, err := client.Get("http://" + address + "/api/v1/session")
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 401 || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal("unsafe anonymous API response")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				waited = true
				if err != nil {
					t.Fatalf("graceful shutdown returned an error: %v", err)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("API did not exit within its shutdown deadline")
			}
			if !strings.Contains(output.String(), "Cosmoformer API listening on "+address) {
				t.Fatal("missing startup confirmation")
			}
			if !strings.Contains(output.String(), "listening") || strings.Contains(output.String(), "postgres://") {
				t.Fatal("unsafe startup output")
			}
			if err := db.App.Ready(context.Background()); err != nil {
				t.Fatal("shutdown changed the database", err)
			}
		})
	}
}
