//go:build integration

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

func TestKeyRegistrationRequiresOwnerAndCannotReplaceIdentity(t *testing.T) {
	db := testsupport.NewDatabase(t)
	binary := testsupport.BuildCommand(t, "cosmoformer")
	key := bytes.Repeat([]byte{42}, 32)
	file := filepath.Join(t.TempDir(), "synthetic-key")
	if e := os.WriteFile(file, key, 0600); e != nil {
		t.Fatal(e)
	}
	owner := protectedDSN(t, db.OwnerDSN)
	app := protectedDSN(t, db.App.Pool.Config().ConnString())
	for _, tc := range []struct {
		name, dsn string
		code      int
		message   string
	}{
		{"runtime role", app, 1, "key registration requires the migration owner"},
		{"initial registration", owner, 0, "Initial encryption key registered"},
		{"repeated registration", owner, 1, "initial key already registered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := operator(t, binary, tc.dsn, "register-key")
			cmd.Env = append(cmd.Env, "COSMOFORMER_ENCRYPTION_KEY_FILE="+file)
			output, e := cmd.CombinedOutput()
			requireExit(t, output, e, tc.code, tc.message)
			if bytes.Contains(output, key) {
				t.Fatal("key bytes exposed by command")
			}
		})
	}
	cipher, e := cryptostore.New(key)
	if e != nil {
		t.Fatal(e)
	}
	fp := cipher.Fingerprint()
	var stored []byte
	var count int
	if e := db.Owner.QueryRow(context.Background(), `SELECT active_key_fingerprint,(SELECT count(*) FROM cosmoformer.crypto_keys) FROM cosmoformer.installation`).Scan(&stored, &count); e != nil || !bytes.Equal(stored, fp[:]) || count != 1 {
		t.Fatal("registration identity was replaced or duplicated", e)
	}
	replacement := filepath.Join(t.TempDir(), "different-synthetic-key")
	if e := os.WriteFile(replacement, bytes.Repeat([]byte{43}, 32), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := serving(t, binary, app, "127.0.0.1:0")
	cmd.Env = append(cmd.Env, "COSMOFORMER_ENCRYPTION_KEY_FILE="+replacement)
	output, e := cmd.CombinedOutput()
	requireExit(t, output, e, 1, "encryption key does not match registered database identity")
}
