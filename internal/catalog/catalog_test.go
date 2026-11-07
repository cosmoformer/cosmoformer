package catalog

import (
	"bytes"
	"go.yaml.in/yaml/v3"
	"strings"
	"testing"
)

func fixture() Stack {
	return Stack{ID: "11111111-1111-4111-8111-111111111111", Name: "Fictional", ProjectID: "22222222-2222-4222-8222-222222222222", EnvironmentID: "33333333-3333-4333-8333-333333333333", Root: ".", TrackedBranch: "main", Engine: Engine{"opentofu", "1.13.8"}, Workspace: "default", BackendOwnerAccountID: "111122223333", Worker: Pool{ID: "44444444-4444-4444-8444-444444444444"}, Approvals: Approvals{1}}
}
func TestStrictManifest(t *testing.T) {
	b, _ := yaml.Marshal(Manifest{1, []Stack{fixture()}})
	if _, e := Parse(b); e != nil {
		t.Fatal(e)
	}
	for name, raw := range map[string]string{"unknown": string(b) + "unknown: true\n", "duplicate": "version: 1\n" + string(b), "multiple": string(b) + "---\nversion: 1\n", "alias": strings.Replace(string(b), "name: Fictional", "name: &name Fictional", 1), "tag": strings.Replace(string(b), "name: Fictional", "name: !!str Fictional", 1), "template": strings.Replace(string(b), "Fictional", "${env.NAME}", 1), "zero": strings.Replace(string(b), "minimum: 1", "minimum: 0", 1), "path": strings.Replace(string(b), "root: .", "root: ../escape", 1), "null": strings.Replace(string(b), "root: .", "root: null", 1)} {
		t.Run(name, func(t *testing.T) {
			if _, e := Parse([]byte(raw)); e == nil {
				t.Fatal("unsupported manifest accepted")
			}
		})
	}
}
func TestLimitsAndDestinations(t *testing.T) {
	s := fixture()
	l := Limits{ProjectID: s.ProjectID, Environments: []string{s.EnvironmentID}, Branch: "main", Pools: []string{s.Worker.ID}, Engines: []Engine{s.Engine}, Minimum: 2}
	resolved, e := l.Resolve(s)
	if e != nil || resolved.Approvals.Minimum != 2 {
		t.Fatal(e)
	}
	s.Worker.ID = s.ID
	if _, e = l.Resolve(s); e == nil {
		t.Fatal("Git expanded pool grant")
	}
	for _, s := range []string{"PATH", "AWS_TOKEN", "TF_CLI_ARGS", "TF_VAR_", "LD_PRELOAD", "GIT_CONFIG_COUNT", "NODE_OPTIONS"} {
		if SecretEnv(s) {
			t.Fatalf("accepted reserved destination %s", s)
		}
	}
	if !SecretEnv("TF_VAR_database_password") || !SecretEnv("SERVICE_TOKEN") {
		t.Fatal("ordinary declared destination refused")
	}
}

func TestManifestRequiresSeparateBackendOwnerMetadata(t *testing.T) {
	for _, owner := range []string{"", "123", "111122223333"} {
		s := fixture()
		s.BackendOwnerAccountID = owner
		raw, _ := yaml.Marshal(Manifest{1, []Stack{s}})
		_, e := Parse(raw)
		if (e == nil) != (owner == "111122223333") {
			t.Fatalf("owner metadata validation: %v", e)
		}
	}
}

func TestManifestRejectsNumericBackendOwner(t *testing.T) {
	raw, _ := yaml.Marshal(Manifest{1, []Stack{fixture()}})
	raw = bytes.Replace(raw, []byte(`backend_owner_account_id: "111122223333"`), []byte(`backend_owner_account_id: 111122223333`), 1)
	if _, e := Parse(raw); e == nil {
		t.Fatal("numeric owner metadata accepted")
	}
}
