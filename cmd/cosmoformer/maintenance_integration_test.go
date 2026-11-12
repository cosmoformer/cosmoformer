//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
