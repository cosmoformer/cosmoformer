//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	hostops "github.com/cosmoformer/cosmoformer/internal/operator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

func TestOperatorCLIProtectedRotationReceiptAndExplicitExit(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	owner := protectedDSN(t, db.OwnerDSN)
	app := protectedDSN(t, db.App.Pool.Config().ConnString())
	output, err := operator(t, binary, app, "maintenance", "enter").CombinedOutput()
	requireExit(t, output, err, 1, "verified migration-owner")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	oldFile, newFile := filepath.Join(dir, "old-key"), filepath.Join(dir, "new-key")
	for i, p := range []string{oldFile, newFile} {
		if err := os.WriteFile(p, bytes.Repeat([]byte{byte(91 + i)}, 32), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := operator(t, binary, owner, "register-key")
	cmd.Env = append(cmd.Env, "COSMOFORMER_ENCRYPTION_KEY_FILE="+oldFile)
	output, err = cmd.CombinedOutput()
	requireExit(t, output, err, 0, "Initial encryption key registered")
	output, err = operator(t, binary, owner, "maintenance", "enter").CombinedOutput()
	requireExit(t, output, err, 0, "maintenance_id")
	var status struct {
		ID string `json:"maintenance_id"`
	}
	if err = json.Unmarshal(output, &status); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(dir, "evidence.json")
	if err = os.WriteFile(ev, []byte(`{"maintenance_id":"`+status.ID+`","reference":"synthetic-host-check","writers_stopped":true,"cleanup_stopped":true,"workers":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	db.App.Pool.Close()
	out := filepath.Join(dir, "rotation.jsonl")
	cmd = operator(t, binary, owner, "rotate-key", "--old-key-file", oldFile, "--new-key-file", newFile, "--evidence-file", ev, "--receipt-file", out)
	output, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("rotation CLI failed: %s", output)
	}
	requireExit(t, output, err, 0, "Protected recovery receipt saved")
	receipt, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(receipt, []byte(`"state":"intent"`)) || !bytes.Contains(receipt, []byte(`"state":"committed"`)) {
		t.Fatal("protected handoff receipt missing")
	}
	info, err := os.Stat(out)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt not protected")
	}
	failReceipt := filepath.Join(dir, "failed.jsonl")
	cmd = operator(t, binary, owner, "rotate-key", "--old-key-file", oldFile, "--new-key-file", newFile, "--evidence-file", ev, "--receipt-file", failReceipt)
	output, err = cmd.CombinedOutput()
	requireExit(t, output, err, 1, "old key does not match")
	if _, err = os.Stat(failReceipt); err != nil {
		t.Fatal("failed rotation discarded recovery intent")
	}
	for i, key := range []string{oldFile, newFile} {
		cmd = operator(t, binary, owner, "maintenance", "exit", "--evidence-file", ev)
		cmd.Env = append(cmd.Env, "COSMOFORMER_ENCRYPTION_KEY_FILE="+key)
		output, err = cmd.CombinedOutput()
		if i == 0 {
			requireExit(t, output, err, 1, "resumption blocked")
		} else {
			requireExit(t, output, err, 0, `"maintenance":false`)
		}
	}
}

func TestOperatorCLIBackupRestoreAndClockFailureAtomicity(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := context.Background()
	binary := testsupport.BuildCommand(t, "cosmoformer")
	dsn := protectedDSN(t, db.OwnerDSN)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	oldKey, newKey := filepath.Join(dir, "old.key"), filepath.Join(dir, "new.key")
	for i, p := range []string{oldKey, newKey} {
		if err := os.WriteFile(p, bytes.Repeat([]byte{byte(111 + i)}, 32), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Invoke the real command surface, with no inherited application settings.
	invoke := func(key string, code int, message string, args ...string) []byte {
		t.Helper()
		cmd := operator(t, binary, dsn, args...)
		if key != "" {
			cmd.Env = append(cmd.Env, "COSMOFORMER_ENCRYPTION_KEY_FILE="+key)
		}
		out, err := cmd.CombinedOutput()
		requireExit(t, out, err, code, message)
		return out
	}
	status := func() hostops.Status {
		t.Helper()
		var out hostops.Status
		if err := json.Unmarshal(invoke("", 0, "maintenance_id", "maintenance", "status"), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	sql := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Owner.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	evidenceFile := filepath.Join(dir, "evidence.json")
	writeEvidence := func(ev hostops.Evidence) {
		t.Helper()
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(evidenceFile, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	invoke(oldKey, 0, "Initial encryption key registered", "register-key")
	invoke("", 0, "maintenance_id", "maintenance", "enter")
	db.App.Pool.Close()
	ev := hostops.Evidence{MaintenanceID: status().MaintenanceID, Reference: "synthetic-cli-host-proof", WritersStopped: true, CleanupStopped: true, Workers: []hostops.WorkerStop{}}
	writeEvidence(ev)
	backupA, backupB := runs.ID(), runs.ID()
	// Receipt failures must precede any backup capture and never overwrite an
	// existing custody record, even when the operator supplies valid stop facts.
	invoke("", 1, "--receipt-file is required", "backup-hold", "create", "--backup-id", backupA, "--evidence-file", evidenceFile)
	publicDir := filepath.Join(dir, "public")
	if err := os.Mkdir(publicDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(publicDir, 0755); err != nil {
		t.Fatal(err)
	}
	invoke("", 1, "receipt directory must be private", "backup-hold", "create", "--backup-id", backupA, "--evidence-file", evidenceFile, "--receipt-file", filepath.Join(publicDir, "receipt"))
	existing := filepath.Join(dir, "existing.jsonl")
	if err := os.WriteFile(existing, []byte("retained-custody-record"), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("", 1, "cannot create protected receipt", "backup-hold", "create", "--backup-id", backupA, "--evidence-file", evidenceFile, "--receipt-file", existing)
	if b, err := os.ReadFile(existing); err != nil || string(b) != "retained-custody-record" {
		t.Fatal("receipt overwritten")
	}
	var captures int
	if err := db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.backup_sets`).Scan(&captures); err != nil || captures != 0 {
		t.Fatal("failed receipt operation captured backup", captures, err)
	}
	captureFile := filepath.Join(dir, "capture.jsonl")
	invoke("", 0, "Protected recovery receipt saved", "backup-hold", "create", "--backup-id", backupA, "--evidence-file", evidenceFile, "--receipt-file", captureFile)
	recoveredFile := filepath.Join(dir, "recovered.jsonl")
	invoke("", 0, "Protected recovery receipt saved", "backup-hold", "inspect", "--backup-id", backupA, "--receipt-file", recoveredFile)
	readCapture := func(path string) hostops.BackupReceipt {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		d := json.NewDecoder(f)
		var intent map[string]any
		var result hostops.BackupReceipt
		if err = d.Decode(&intent); err != nil {
			t.Fatal(err)
		}
		if err = d.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if intent["state"] != "intent" || intent["backup_id"] != backupA {
			t.Fatal("capture intent lost")
		}
		return result
	}
	captured, recovered := readCapture(captureFile), readCapture(recoveredFile)
	if captured.ID != backupA || captured.Count != 0 || captured.Digest == "" || captured.Digest != recovered.Digest || !captured.Cutoff.Equal(recovered.Cutoff) {
		t.Fatal("CLI inspect changed the committed backup set")
	}
	invoke("", 0, "Protected recovery receipt saved", "backup-hold", "create", "--backup-id", backupB, "--evidence-file", evidenceFile, "--receipt-file", filepath.Join(dir, "second.jsonl"))
	invoke("", 1, "explicit retention/custody checks required", "backup-hold", "release", "--backup-id", backupA, "--evidence-file", evidenceFile)
	ev.RetentionChecked = true
	writeEvidence(ev)
	invoke("", 0, "backup_sets", "backup-hold", "release", "--backup-id", backupA, "--evidence-file", evidenceFile)
	var selectedOnly bool
	if err := db.Owner.QueryRow(ctx, `SELECT (SELECT released_at IS NOT NULL FROM cosmoformer.backup_sets WHERE id=$1) AND (SELECT released_at IS NULL FROM cosmoformer.backup_sets WHERE id=$2)`, backupA, backupB).Scan(&selectedOnly); err != nil || !selectedOnly {
		t.Fatal("CLI release changed another backup", err)
	}
	// Seed authenticatable restored bearer state, then establish revocation via
	// the real restore command rather than calling the operator library directly.
	account := runs.ID()
	token := identity.Token()
	sql(`INSERT INTO cosmoformer.accounts(id,username,display_name,password_phc,installation_admin) VALUES($1,'restore.cli','Synthetic CLI','synthetic-verifier',true)`, account)
	sql(`INSERT INTO cosmoformer.sessions(account_id,credential_revision,verifier,csrf_verifier) VALUES($1,1,$2,$2)`, account, identity.Digest(token))
	restoreID := runs.ID()
	invoke("", 0, "restore_key_required", "restore-begin", "--restore-id", restoreID)
	var revoked bool
	if err := db.Owner.QueryRow(ctx, `SELECT cosmoformer.session_identity($1) IS NULL AND (SELECT password_phc IS NULL FROM cosmoformer.accounts WHERE id=$2)`, identity.Digest(token), account).Scan(&revoked); err != nil || !revoked {
		t.Fatal("restored credential replay remains possible", err)
	}
	invoke(oldKey, 1, "maintenance identity and independent operator evidence required", "maintenance", "exit", "--evidence-file", evidenceFile)
	ev.MaintenanceID = status().MaintenanceID
	writeEvidence(ev)
	invoke("", 1, "complete external worker/writer/backend/Git inventory", "restore-reconcile", "--evidence-file", evidenceFile)
	ev.InventoryComplete = true
	ev.ExternalWritersStopped = true
	ev.SourceReconciled = true
	ev.InventoryReference = "synthetic-current-inventory"
	ev.ClockValidation = "synthetic-utc-validation"
	ev.FreshKey = true
	ev.KeyHistoryReference = "synthetic-independent-key-history"
	writeEvidence(ev)
	invoke("", 0, `"restore_reconciled":true`, "restore-reconcile", "--evidence-file", evidenceFile)
	invoke(oldKey, 1, "resumption blocked", "maintenance", "exit", "--evidence-file", evidenceFile)
	// A database failure during clock-reset must neither clear the clock latch
	// nor leak its diagnostic detail; retry still needs --verified and evidence.
	sql(`INSERT INTO cosmoformer.sessions(account_id,credential_revision,verifier,csrf_verifier) SELECT id,credential_revision,$2,$2 FROM cosmoformer.accounts WHERE id=$1`, account, identity.Digest(token))
	sql(`INSERT INTO cosmoformer.account_proofs(account_id,purpose,credential_revision,verifier,expires_at) SELECT id,'reset',credential_revision,$2,clock_timestamp()+interval '1 hour' FROM cosmoformer.accounts WHERE id=$1`, account, identity.Digest("synthetic-proof"))
	sql(`CREATE FUNCTION cosmoformer.fixture_reject_clock_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action='clock.reset' THEN RAISE EXCEPTION 'synthetic-sensitive-diagnostic' USING DETAIL='synthetic-private-detail'; END IF; RETURN NEW; END $$; CREATE TRIGGER fixture_reject_clock_reset BEFORE INSERT ON cosmoformer.operator_events FOR EACH ROW EXECUTE FUNCTION cosmoformer.fixture_reject_clock_reset()`)
	output := invoke("", 1, "operator database operation rejected (SQLSTATE P0001)", "clock-reset", "--verified", "--evidence-file", evidenceFile)
	if bytes.Contains(output, []byte("synthetic-sensitive-diagnostic")) || bytes.Contains(output, []byte("synthetic-private-detail")) {
		t.Fatal("database diagnostic leaked")
	}
	if !status().ClockUnreliable {
		t.Fatal("failed clock reset cleared the latch")
	}
	sql(`DROP TRIGGER fixture_reject_clock_reset ON cosmoformer.operator_events; DROP FUNCTION cosmoformer.fixture_reject_clock_reset()`)
	invoke("", 1, "--verified and recorded independent UTC clock validation required", "clock-reset", "--evidence-file", evidenceFile)
	invoke("", 0, `"clock_unreliable":false`, "clock-reset", "--verified", "--evidence-file", evidenceFile)
	if err := db.Owner.QueryRow(ctx, `SELECT cosmoformer.session_identity($1) IS NULL AND NOT EXISTS(SELECT FROM cosmoformer.account_proofs)`, identity.Digest(token)).Scan(&revoked); err != nil || !revoked {
		t.Fatal("clock reset kept session/proof replay authority", err)
	}
	invoke("", 0, "Protected recovery receipt saved", "rotate-key", "--old-key-file", oldKey, "--new-key-file", newKey, "--evidence-file", evidenceFile, "--receipt-file", filepath.Join(dir, "rotation.jsonl"))
	invoke(newKey, 0, `"maintenance":false`, "maintenance", "exit", "--evidence-file", evidenceFile)
	final := status()
	if final.Maintenance || final.Offline || !final.RestoreReconciled || final.RestoreKeyRequired || final.RestoreID != restoreID {
		t.Fatal("CLI restore did not require explicit complete resumption")
	}
}
