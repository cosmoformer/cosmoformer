package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/testsupport"
)

func requireExit(t *testing.T, output []byte, err error, code int, message string) {
	t.Helper()
	actual := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("command did not exit normally: %v", err)
		}
		actual = exit.ExitCode()
	}
	if actual != code || !strings.Contains(string(output), message) {
		t.Fatalf("exit=%d, expected=%d; missing expected message %q", actual, code, message)
	}
}

func TestCommandArguments(t *testing.T) {
	binary := testsupport.BuildCommand(t, "cosmoformer")
	for _, tc := range []struct {
		name    string
		args    []string
		message string
	}{
		{"missing command", nil, "use serve, migrate, bootstrap-admin or recover-admin"},
		{"unknown command", []string{"unknown"}, "unknown command"},
		{"backward migration", []string{"migrate", "--down"}, "FORWARD_ONLY_MIGRATIONS"},
		{"migration positional argument", []string{"migrate", "rollback"}, "FORWARD_ONLY_MIGRATIONS"},
		{"missing migration credential", []string{"migrate"}, "cannot open protected input file"},
		{"missing serve settings", []string{"serve"}, "COSMOFORMER_ORIGIN"},
		{"invalid bootstrap flag", []string{"bootstrap-admin", "--unknown"}, "flag provided but not defined"},
		{"missing bootstrap username", []string{"bootstrap-admin"}, "username must"},
		{"missing recovery username", []string{"recover-admin"}, "username must"},
		{"missing output", []string{"bootstrap-admin", "--username", "admin"}, "--setup-code-file is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := testsupport.Command(t, binary, tc.args...).CombinedOutput()
			requireExit(t, output, err, 1, tc.message)
		})
	}
	t.Run("migration inventory without database", func(t *testing.T) {
		output, err := testsupport.Command(t, binary, "migrate", "--list").CombinedOutput()
		requireExit(t, output, err, 0, "migrations/006_evidence_alignment.sql")
		if strings.Count(string(output), "migrations/") != 6 {
			t.Fatal("unexpected embedded migration inventory")
		}
	})
}

func TestProtectedOperatorInputs(t *testing.T) {
	binary := testsupport.BuildCommand(t, "cosmoformer")
	t.Run("refuses to overwrite a proof file", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "proof")
		if err := os.WriteFile(file, []byte("existing fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		output, err := testsupport.Command(t, binary, "bootstrap-admin", "--username", "admin", "--setup-code-file", file).CombinedOutput()
		requireExit(t, output, err, 1, "cannot create protected code output")
		content, err := os.ReadFile(file)
		if err != nil || string(content) != "existing fixture" {
			t.Fatal("existing proof output was modified")
		}
	})
	for _, command := range []string{"bootstrap-admin", "recover-admin"} {
		t.Run(command+" cleans failed output", func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "proof")
			output, err := testsupport.Command(t, binary, command, "--username", "admin", "--setup-code-file", file).CombinedOutput()
			requireExit(t, output, err, 1, "cannot open protected input file")
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatal("failed command retained proof output")
			}
		})
	}
	t.Run("sanitizes database connection failures", func(t *testing.T) {
		dir := t.TempDir()
		dsn := filepath.Join(dir, "owner.dsn")
		proof := filepath.Join(dir, "proof")
		if err := os.WriteFile(dsn, []byte("postgres://fixture:private-fixture-value@127.0.0.1:1/missing?connect_timeout=1"), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := testsupport.Command(t, binary, "recover-admin", "--username", "admin", "--setup-code-file", proof)
		cmd.Env = append(cmd.Env, "COSMOFORMER_MIGRATION_DSN_FILE="+dsn)
		output, err := cmd.CombinedOutput()
		requireExit(t, output, err, 1, "operator database connection unavailable")
		if strings.Contains(string(output), "private-fixture-value") {
			t.Fatal("connection error exposed credential")
		}
		if _, err := os.Stat(proof); !os.IsNotExist(err) {
			t.Fatal("connection failure retained proof output")
		}
	})
}
