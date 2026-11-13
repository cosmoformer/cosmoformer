package config

import (
	"fmt"
	"os"
	"testing"
)

func TestOrigin(t *testing.T) {
	for _, origin := range []string{"http://localhost:5173", "https://localhost:5173/", "https://user@localhost", "https://localhost?x=1"} {
		if (Config{Listen: "127.0.0.1:8080", Origin: origin, DSNFile: "local"}).Validate() == nil {
			t.Fatal(origin)
		}
	}
	if err := (Config{Listen: "127.0.0.1:8080", Origin: "https://localhost:5173", DSNFile: "local"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedInput(t *testing.T) {
	path := t.TempDir() + "/input"
	if e := os.WriteFile(path, []byte("fictional\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if value, e := ReadSecret(path); e != nil || value != "fictional" {
		t.Fatal("protected file rejected", e)
	}
	if e := os.Chmod(path, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := ReadSecret(path); e == nil {
		t.Fatal("world-readable input accepted")
	}
}

func TestLoadAndRequiredSettings(t *testing.T) {
	t.Setenv("COSMOFORMER_UNIX_SOCKET", "")
	t.Setenv("COSMOFORMER_LISTEN", "")
	t.Setenv("COSMOFORMER_ORIGIN", "https://localhost:5173")
	t.Setenv("COSMOFORMER_DATABASE_DSN_FILE", "protected.dsn")
	c, err := Load()
	if err != nil || c.Listen != "127.0.0.1:18080" {
		t.Fatalf("default listen: %v, %v", c, err)
	}
	t.Setenv("COSMOFORMER_LISTEN", "missing-port")
	if _, err := Load(); err == nil {
		t.Fatal("invalid listen accepted")
	}
	t.Setenv("COSMOFORMER_LISTEN", "127.0.0.1:8080")
	t.Setenv("COSMOFORMER_DATABASE_DSN_FILE", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing protected DSN accepted")
	}
}
func TestUnixSocketConfiguration(t *testing.T) {
	t.Setenv("COSMOFORMER_LISTEN", "")
	t.Setenv("COSMOFORMER_UNIX_SOCKET", "/private/api.sock")
	t.Setenv("COSMOFORMER_ORIGIN", "https://localhost:5173")
	t.Setenv("COSMOFORMER_DATABASE_DSN_FILE", "protected.dsn")
	c, err := Load()
	if err != nil || c.Listen != "" || c.UnixSocket != "/private/api.sock" {
		t.Fatal(c, err)
	}
	t.Setenv("COSMOFORMER_LISTEN", "127.0.0.1:18080")
	if _, err = Load(); err == nil {
		t.Fatal("simultaneous TCP and Unix endpoints accepted")
	}
	t.Setenv("COSMOFORMER_LISTEN", "")
	t.Setenv("COSMOFORMER_UNIX_SOCKET", "relative.sock")
	if _, err = Load(); err == nil {
		t.Fatal("relative Unix endpoint accepted")
	}
}

func TestProtectedInputRejectsMissingEmptyAndOversized(t *testing.T) {
	dir := t.TempDir()
	for _, size := range []int{0, 16385} {
		p := fmt.Sprintf("%s/input-%d", dir, size)
		if err := os.WriteFile(p, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadSecret(p); err == nil {
			t.Fatalf("accepted %d bytes", size)
		}
	}
	for _, p := range []string{dir, dir + "/missing"} {
		if _, err := ReadSecret(p); err == nil {
			t.Fatal("accepted directory/missing input")
		}
	}
}
func TestRawEncryptionKeyDoesNotTrimBinaryBytes(t *testing.T) {
	file := t.TempDir() + "/key"
	key := make([]byte, 32)
	key[31] = '\n'
	if e := os.WriteFile(file, key, 0600); e != nil {
		t.Fatal(e)
	}
	read, e := ReadKey(file)
	if e != nil || len(read) != 32 || read[31] != '\n' {
		t.Fatal("binary key was text-trimmed", e)
	}
	os.Chmod(file, 0644)
	if _, e = ReadKey(file); e == nil {
		t.Fatal("public key file accepted")
	}
}

func TestApplyBudget(t *testing.T) {
	t.Setenv("COSMOFORMER_ORIGIN", "https://localhost:5173")
	t.Setenv("COSMOFORMER_DATABASE_DSN_FILE", "protected.dsn")
	t.Setenv("COSMOFORMER_APPLY_SECONDS", "")
	c, e := Load()
	if e != nil || c.ApplySeconds != 2400 {
		t.Fatal(c.ApplySeconds, e)
	}
	for _, v := range []string{"29", "3301", "bad"} {
		t.Setenv("COSMOFORMER_APPLY_SECONDS", v)
		if _, e = Load(); e == nil {
			t.Fatal("invalid Apply cap accepted", v)
		}
	}
}

func TestCleanupDefaultsAndBounds(t *testing.T) {
	t.Setenv("COSMOFORMER_ORIGIN", "https://localhost:5173")
	t.Setenv("COSMOFORMER_DATABASE_DSN_FILE", "protected.dsn")
	for _, key := range []string{"CLEANUP_ENABLED", "CLEANUP_INTERVAL_SECONDS", "TERMINAL_RETENTION_DAYS", "CLEANUP_BATCH"} {
		t.Setenv("COSMOFORMER_"+key, "")
	}
	c, e := Load()
	if e != nil || c.Cleanup.Enabled || c.Cleanup.IntervalSeconds != 86400 || c.Cleanup.RetentionDays != 30 || c.Cleanup.Batch != 100 {
		t.Fatal(c.Cleanup, e)
	}
	for key, value := range map[string]string{"CLEANUP_ENABLED": "yes", "CLEANUP_INTERVAL_SECONDS": "3599", "TERMINAL_RETENTION_DAYS": "29", "CLEANUP_BATCH": "0"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("COSMOFORMER_"+key, value)
			if _, e := Load(); e == nil {
				t.Fatal("invalid cleanup setting accepted")
			}
		})
	}
	t.Setenv("COSMOFORMER_CLEANUP_BATCH", "101")
	if _, e := Load(); e == nil {
		t.Fatal("oversized cleanup batch")
	}
	t.Setenv("COSMOFORMER_CLEANUP_BATCH", "1")
	t.Setenv("COSMOFORMER_CLEANUP_ENABLED", "true")
	if _, e := Load(); e == nil {
		t.Fatal("cleanup enabled without storage")
	}
	t.Setenv("COSMOFORMER_ARTIFACT_BUCKET", "synthetic-artifacts")
	t.Setenv("COSMOFORMER_ARTIFACT_REGION", "eu-west-1")
	t.Setenv("COSMOFORMER_ARTIFACT_EXPECTED_OWNER", "123456789012")
	if c, e = Load(); e != nil || !c.Cleanup.Enabled || c.Cleanup.Batch != 1 {
		t.Fatal(c.Cleanup, e)
	}
}
