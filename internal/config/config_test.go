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
