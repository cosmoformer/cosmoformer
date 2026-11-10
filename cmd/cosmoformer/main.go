package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/config"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("use serve, migrate, bootstrap-admin or recover-admin")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	switch os.Args[1] {
	case "register-key":
		if len(os.Args) != 2 {
			return errors.New("register-key accepts no arguments")
		}
		key, e := config.ReadKey(os.Getenv("COSMOFORMER_ENCRYPTION_KEY_FILE"))
		if e != nil {
			return e
		}
		cipher, e := cryptostore.New(key)
		if e != nil {
			return e
		}
		fp := cipher.Fingerprint()
		dsn, e := config.ReadSecret(os.Getenv("COSMOFORMER_MIGRATION_DSN_FILE"))
		if e != nil {
			return e
		}
		conn, e := pgx.Connect(ctx, dsn)
		if e != nil {
			return errors.New("operator database connection unavailable")
		}
		defer conn.Close(ctx)
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(ctx)
		var owner bool
		if e = tx.QueryRow(ctx, `SELECT current_user='cosmoformer_owner'`).Scan(&owner); e != nil || !owner {
			return errors.New("key registration requires the migration owner")
		}
		if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
			return e
		}
		var accepted bool
		e = tx.QueryRow(ctx, `WITH u AS (UPDATE cosmoformer.installation SET active_key_fingerprint=$1 WHERE active_key_fingerprint IS NULL AND NOT EXISTS(SELECT FROM cosmoformer.crypto_keys) RETURNING id) SELECT EXISTS(SELECT FROM u)`, fp[:]).Scan(&accepted)
		if e != nil || !accepted {
			return errors.New("initial key already registered; key replacement requires offline rotation")
		}
		if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state) VALUES($1,'active')`, fp[:]); e != nil {
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return errors.New("key registration commit uncertain; inspect current metadata before retrying")
		}
		fmt.Println("Initial encryption key registered; key bytes remain outside the database.")
		return nil
	case "migrate":
		f := flag.NewFlagSet("migrate", flag.ContinueOnError)
		list := f.Bool("list", false, "list embedded forward migrations")
		if e := f.Parse(os.Args[2:]); e != nil {
			return errors.New("FORWARD_ONLY_MIGRATIONS: only --list or forward migration is supported")
		}
		if f.NArg() != 0 {
			return errors.New("FORWARD_ONLY_MIGRATIONS")
		}
		if *list {
			names, e := store.MigrationNames()
			if e != nil {
				return e
			}
			for _, n := range names {
				fmt.Println(n)
			}
			return nil
		}
		dsn, e := config.ReadSecret(os.Getenv("COSMOFORMER_MIGRATION_DSN_FILE"))
		if e != nil {
			return e
		}
		if e = store.Migrate(ctx, dsn); e != nil {
			return e
		}
		fmt.Printf("Database schema is current (version %d).\n", store.SchemaVersion)
		return nil
	case "bootstrap-admin", "recover-admin":
		return admin(ctx, os.Args[1], os.Args[2:])
	case "serve":
		c, e := config.Load()
		if e != nil {
			return e
		}
		dsn, e := config.ReadSecret(c.DSNFile)
		if e != nil {
			return e
		}
		s, e := store.Open(ctx, dsn)
		if e != nil {
			return e
		}
		defer s.Pool.Close()
		options := httpapi.Options{Background: ctx, PlanSeconds: c.PlanSeconds, PreflightSeconds: c.PreflightSeconds, PlanTTLHours: c.PlanTTLHours}
		if c.Artifact.Bucket != "" {
			storage, e := evidence.NewS3(ctx, c.Artifact)
			if e != nil {
				return e
			}
			options.Evidence = &evidence.Custody{Storage: storage, Bucket: c.Artifact.Bucket}
		}
		if keyPath := os.Getenv("COSMOFORMER_ENCRYPTION_KEY_FILE"); keyPath != "" {
			key, e := config.ReadKey(keyPath)
			if e != nil {
				return e
			}
			options.Cipher, e = cryptostore.New(key)
			if e != nil {
				return e
			}
			fp := options.Cipher.Fingerprint()
			var matches bool
			if e = s.Pool.QueryRow(ctx, `SELECT active_key_fingerprint=$1 FROM cosmoformer.installation WHERE id`, fp[:]).Scan(&matches); e != nil || !matches {
				return errors.New("encryption key does not match registered database identity")
			}
		}
		handler, _ := httpapi.NewWithOptions(s, c.Origin, options)
		server := &http.Server{Addr: c.Listen, Handler: handler, ConnContext: httpapi.ConnectionContext, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
		listener, e := listenAPI(c)
		if e != nil {
			return fmt.Errorf("cannot bind API listen address: %w", e)
		}
		defer listener.Close()
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		address := c.Listen
		if c.UnixSocket != "" {
			address = c.UnixSocket
		}
		fmt.Println("Cosmoformer API listening on", address)
		select {
		case e := <-done:
			if errors.Is(e, http.ErrServerClosed) {
				return nil
			}
			return e
		case <-ctx.Done():
			stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return server.Shutdown(stop)
		}
	default:
		return errors.New("unknown command")
	}
}
func listenAPI(c config.Config) (net.Listener, error) {
	if c.UnixSocket == "" {
		return net.Listen("tcp", c.Listen)
	}
	info, err := os.Stat(filepath.Dir(c.UnixSocket))
	if err != nil {
		return nil, errors.New("private API socket directory is unavailable")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0077 != 0 || owner.Uid != uint32(os.Getuid()) {
		return nil, errors.New("API socket directory must belong to this user and have mode 0700")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.UnixSocket, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(c.UnixSocket, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}
func admin(ctx context.Context, command string, args []string) error {
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	username := f.String("username", "", "local username")
	display := f.String("display-name", "Administrator", "display name")
	codeFile := f.String("setup-code-file", "", "protected output file for a one-use setup/reset code")
	if e := f.Parse(args); e != nil {
		return e
	}
	name, e := identity.Username(*username)
	if e != nil {
		return e
	}
	if *codeFile == "" {
		return errors.New("--setup-code-file is required; code is never printed")
	}
	// Reserve output before changing credentials; refuse accidental overwrites.
	out, e := os.OpenFile(*codeFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return errors.New("cannot create protected code output; choose a new file")
	}
	success := false
	defer func() {
		out.Close()
		if !success {
			os.Remove(*codeFile)
		}
	}()
	dsn, e := config.ReadSecret(os.Getenv("COSMOFORMER_MIGRATION_DSN_FILE"))
	if e != nil {
		return e
	}
	c, e := pgx.Connect(ctx, dsn)
	if e != nil {
		return errors.New("operator database connection unavailable")
	}
	defer c.Close(ctx)
	var owner bool
	if e = c.QueryRow(ctx, `SELECT current_user='cosmoformer_owner'`).Scan(&owner); e != nil || !owner {
		return errors.New("this host command requires the migration owner")
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Serialize host credential changes with authenticated requests, taking the
	// shared gate before the installation lock so recovery revokes their proofs.
	if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
		return e
	}
	var completed bool
	if e = tx.QueryRow(ctx, `SELECT bootstrap_completed FROM cosmoformer.installation WHERE id FOR UPDATE`).Scan(&completed); e != nil {
		return e
	}
	var id string
	var revision int64
	purpose := "setup"
	duration := "24 hours"
	if command == "bootstrap-admin" {
		if completed {
			return errors.New("bootstrap is already completed; it cannot be reopened")
		}
		e = tx.QueryRow(ctx, `INSERT INTO cosmoformer.accounts(username,display_name,installation_admin) VALUES($1,$2,true) RETURNING id::text,credential_revision`, name, *display).Scan(&id, &revision)
		if e == nil {
			_, e = tx.Exec(ctx, `UPDATE cosmoformer.installation SET bootstrap_completed=true WHERE id`)
		}
	} else {
		purpose = "reset"
		duration = "30 minutes"
		e = tx.QueryRow(ctx, `UPDATE cosmoformer.accounts SET credential_revision=credential_revision+1,revision=revision+1 WHERE username=$1 AND enabled AND installation_admin RETURNING id::text,credential_revision`, name).Scan(&id, &revision)
		if e == nil {
			_, e = tx.Exec(ctx, `DELETE FROM cosmoformer.sessions WHERE account_id=$1`, id)
		}
	}
	if e != nil {
		return errors.New("cannot bootstrap/recover the specified administrator")
	}
	code := identity.Token()
	if _, e = tx.Exec(ctx, `DELETE FROM cosmoformer.account_proofs WHERE account_id=$1`, id); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.account_proofs(account_id,purpose,verifier,credential_revision,expires_at) VALUES($1,$2,$3,$4,clock_timestamp()+$5::interval)`, id, purpose, identity.Digest(code), revision, duration); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.identity_audit_events(action,subject_id) VALUES($1,$2)`, command, id); e != nil {
		return e
	}
	if _, e = out.WriteString(code + "\n"); e != nil {
		return e
	}
	if e = out.Sync(); e != nil {
		return e
	}
	if e = tx.Commit(ctx); e != nil {
		return errors.New("administrator command commit is uncertain; check bootstrap state before retrying")
	}
	success = true
	fmt.Println("One-use", purpose, "code saved to", *codeFile, "— deliver it privately; no password has been set.")
	return nil
}
