package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cosmoformer/cosmoformer/internal/config"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	hostops "github.com/cosmoformer/cosmoformer/internal/operator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5/pgconn"
)

func protectedReceipt(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("--receipt-file is required; retain it through recovery and backup retention")
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("receipt directory must be private (0700)")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("cannot create protected receipt; choose a new file")
	}
	// Persist the directory entry before any database mutation can be attempted.
	d, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = d.Sync()
		d.Close()
	}
	if err != nil {
		f.Close()
		return nil, errors.New("cannot persist receipt directory")
	}
	return f, nil
}
func receipt(f *os.File, value any) error {
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return errors.New("cannot write protected recovery receipt")
	}
	if err := f.Sync(); err != nil {
		return errors.New("cannot persist protected recovery receipt")
	}
	return nil
}
func maintenanceCommand(ctx context.Context, command string, args []string) (result error) {
	defer func() {
		var p *pgconn.PgError
		if errors.As(result, &p) {
			result = fmt.Errorf("operator database operation rejected (SQLSTATE %s); retain maintenance and inspect protected evidence", p.Code)
		}
	}()

	action := ""
	if command == "maintenance" || command == "backup-hold" {
		if len(args) == 0 {
			return errors.New("maintenance requires enter/status/exit; backup-hold requires create/release")
		}
		action, args = args[0], args[1:]
		if command == "maintenance" && action != "enter" && action != "status" && action != "exit" || command == "backup-hold" && action != "create" && action != "release" && action != "inspect" {
			return errors.New("unknown operator action")
		}
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	evidencePath := f.String("evidence-file", "", "protected operator evidence JSON")
	receiptPath := f.String("receipt-file", "", "new protected recovery receipt file")
	backupID := f.String("backup-id", "", "backup UUID")
	restoreID := f.String("restore-id", "", "restore UUID")
	oldPath := f.String("old-key-file", "", "protected current raw key")
	newPath := f.String("new-key-file", "", "protected new raw key")
	verified := f.Bool("verified", false, "independent UTC clock validation completed")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected operator arguments")
	}
	var ev hostops.Evidence
	needsEvidence := !(command == "maintenance" && (action == "enter" || action == "status")) && command != "restore-begin" && !(command == "backup-hold" && action == "inspect")
	if needsEvidence {
		var err error
		ev, err = hostops.ReadEvidenceFile(*evidencePath)
		if err != nil {
			return err
		}
	}
	var old, next *cryptostore.Cipher
	if command == "rotate-key" {
		b, err := config.ReadKey(*oldPath)
		if err != nil {
			return err
		}
		old, err = cryptostore.New(b)
		clear(b)
		if err != nil {
			return err
		}
		b, err = config.ReadKey(*newPath)
		if err != nil {
			return err
		}
		next, err = cryptostore.New(b)
		clear(b)
		if err != nil {
			return err
		}
	}
	var output *os.File
	if command == "rotate-key" || command == "backup-hold" && (action == "create" || action == "inspect") {
		var err error
		output, err = protectedReceipt(*receiptPath)
		if err != nil {
			return err
		}
		defer output.Close()
	}
	id := runs.ID()
	if output != nil {
		intent := map[string]any{"operation": command, "operation_id": id, "maintenance_id": ev.MaintenanceID, "backup_id": *backupID, "state": "intent"}
		if old != nil {
			a, b := old.Fingerprint(), next.Fingerprint()
			intent["old_key"] = hex.EncodeToString(a[:])
			intent["new_key"] = hex.EncodeToString(b[:])
		}
		if err := receipt(output, intent); err != nil {
			return err
		}
	}
	dsn, err := config.ReadSecret(os.Getenv("COSMOFORMER_MIGRATION_DSN_FILE"))
	if err != nil {
		return err
	}
	op, err := hostops.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer op.Close()
	switch command {
	case "maintenance":
		switch action {
		case "enter":
			err = op.Enter(ctx)
		case "exit":
			b, e := config.ReadKey(os.Getenv("COSMOFORMER_ENCRYPTION_KEY_FILE"))
			if e != nil {
				return e
			}
			c, e := cryptostore.New(b)
			clear(b)
			if e != nil {
				return e
			}
			fp := c.Fingerprint()
			err = op.Exit(ctx, ev, fp[:])
		}
	case "rotate-key":
		err = op.Rotate(ctx, ev, id, old, next)
	case "clock-reset":
		err = op.ClockReset(ctx, ev, *verified)
	case "backup-hold":
		if action == "create" {
			var capture hostops.BackupReceipt
			capture, err = op.BackupCreate(ctx, ev, *backupID)
			if err == nil {
				err = receipt(output, capture)
			}
		} else if action == "inspect" {
			var capture hostops.BackupReceipt
			capture, err = op.BackupInspect(ctx, *backupID)
			if err == nil {
				err = receipt(output, capture)
			}
		} else {
			err = op.BackupRelease(ctx, ev, *backupID)
		}
	case "restore-begin":
		err = op.RestoreBegin(ctx, *restoreID)
	case "restore-reconcile":
		err = op.RestoreReconcile(ctx, ev)
	default:
		return errors.New("unknown host command")
	}
	if err != nil {
		return err
	}
	if output != nil {
		if err = receipt(output, map[string]string{"operation_id": id, "state": "committed"}); err != nil {
			return err
		}
		fmt.Println("Protected recovery receipt saved; retain it and inspect maintenance status before resuming.")
	}
	status, err := op.Status(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
