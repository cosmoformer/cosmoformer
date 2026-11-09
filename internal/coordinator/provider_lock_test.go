package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"strings"
	"testing"
)

func TestExactProviderLockAndEmptyException(t *testing.T) {
	absent := runs.Manifest{Lock: runs.ProviderLock{}, Providers: []runs.Provider{}}
	if e := absent.ValidateLock(); e != nil {
		t.Fatal(e)
	}
	if e := verifyProviderLock(&absent, nil); e != nil {
		t.Fatal(e)
	}
	if verifyProviderLock(&absent, []byte{}) == nil {
		t.Fatal("absent lock silently replaced by empty file")
	}
	lock := []byte("# exact native bytes\nprovider \"registry.opentofu.org/hashicorp/null\" {\n version=\"3.2.4\"\n hashes=[\"zh:" + strings.Repeat("b", 64) + "\"]\n}\n")
	h := sha256.Sum256(lock)
	manifest := runs.Manifest{Lock: runs.ProviderLock{Present: true, Hash: hex.EncodeToString(h[:]), Size: int64(len(lock))}, Providers: []runs.Provider{{Address: "registry.opentofu.org/hashicorp/null", Version: "3.2.4", Platform: "linux_amd64", Hashes: []string{"zh:" + strings.Repeat("b", 64)}}}}
	if e := manifest.ValidateLock(); e != nil {
		t.Fatal(e)
	}
	if e := verifyProviderLock(&manifest, lock); e != nil {
		t.Fatal(e)
	}
	if verifyProviderLock(&manifest, append(append([]byte{}, lock...), '\n')) == nil {
		t.Fatal("lock bytes were normalized")
	}
	manifest.Providers[0].Version = "3.2.5"
	if verifyProviderLock(&manifest, lock) == nil {
		t.Fatal("unselected provider version accepted")
	}
	manifest.Lock.Present = false
	manifest.Lock.Hash = ""
	manifest.Lock.Size = 0
	if manifest.ValidateLock() == nil {
		t.Fatal("external providers without native lock accepted")
	}
}

func TestProviderLockRejectsExpressionsExtensionsAndInvalidSelections(t *testing.T) {
	wrap := func(body string) string {
		return "provider \"registry.opentofu.org/hashicorp/null\" {\n" + body + "\n}\n"
	}
	goodBody := "version = \"3.2.4\"\nhashes = [\"zh:fixture\"]"
	for name, raw := range map[string]string{
		"malformed": "provider {", "root attribute": "value = true\n", "wrong block": "resource \"x\" {}", "duplicate block": wrap(goodBody) + wrap(goodBody),
		"nested block": wrap("nested {}"), "expression": wrap("version = var.version"), "null": wrap("version = null"),
		"numeric version": wrap("version = 3"), "numeric constraint": wrap("constraints = 3"), "scalar hashes": wrap("hashes = 3"),
		"numeric checksum": wrap("hashes = [3]"), "null checksum": wrap("hashes = [null]"), "unsupported checksum": wrap("hashes = [\"sha256:fixture\"]"),
		"unknown attribute": wrap("command = \"sh\""),
	} {
		t.Run(name, func(t *testing.T) {
			bytes := []byte(raw)
			hash := sha256.Sum256(bytes)
			manifest := runs.Manifest{Lock: runs.ProviderLock{Present: true, Hash: hex.EncodeToString(hash[:]), Size: int64(len(bytes))}, Providers: []runs.Provider{}}
			if e := verifyProviderLock(&manifest, bytes); runs.Code(e) != "PROVIDER_LOCK_INVALID" {
				t.Fatal("untrusted lock shape accepted", e)
			}
		})
	}
	raw := []byte(wrap(goodBody+"\nconstraints = \">= 3.0.0\"") + "provider \"registry.opentofu.org/hashicorp/aws\" {\nversion = \"6.41.0\"\nhashes = [\"zh:fixture\",\"h1:fixture\"]\n}\n")
	hash := sha256.Sum256(raw)
	manifest := runs.Manifest{Lock: runs.ProviderLock{Present: true, Hash: hex.EncodeToString(hash[:]), Size: int64(len(raw))}, Providers: []runs.Provider{{Address: "registry.opentofu.org/hashicorp/aws", Version: "6.41.0", Platform: "linux_amd64", Hashes: []string{"h1:fixture", "zh:fixture"}}, {Address: "registry.opentofu.org/hashicorp/null", Version: "3.2.4", Platform: "linux_amd64", Hashes: []string{"zh:fixture"}}}}
	if e := verifyProviderLock(&manifest, raw); e != nil {
		t.Fatal("valid native constraints or provider ordering rejected", e)
	}
}
