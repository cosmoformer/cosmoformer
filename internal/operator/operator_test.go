package operator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceRejectsUnknownAndTrailingData(t *testing.T) {
	for _, b := range []string{`{"writers_stopped":true,"invented_stop_proof":true}`, `{} {}`, `{}x`, `null []`} {
		if _, err := ParseEvidence([]byte(b)); err == nil {
			t.Fatal("invalid evidence accepted")
		}
	}
	if _, err := ParseEvidence([]byte(`{"reference":"synthetic","workers":[]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestProtectedEvidenceAllowsCompleteInventoriesButRejectsPublicFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "evidence.json")
	// A large inventory must not inherit the small credential-file bound.
	value := `{"reference":"` + strings.Repeat("synthetic", 4096) + `","workers":[]}`
	if err := os.WriteFile(p, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEvidenceFile(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEvidenceFile(p); err == nil {
		t.Fatal("public evidence accepted")
	}
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, (16<<20)+1); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadEvidenceFile(p); err == nil {
		t.Fatal("oversized evidence accepted")
	}
}
