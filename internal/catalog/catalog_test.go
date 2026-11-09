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

func TestOmittedOptionalCollectionsUseExplicitEmptyDefaults(t *testing.T) {
	raw, _ := yaml.Marshal(Manifest{1, []Stack{fixture()}})
	var doc map[string]any
	if e := yaml.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	stack := doc["stacks"].([]any)[0].(map[string]any)
	for _, name := range []string{"backend_config_files", "var_files", "secrets", "change_paths", "depends_on", "approvals"} {
		delete(stack, name)
	}
	delete(stack["worker"].(map[string]any), "tags")
	raw, _ = yaml.Marshal(doc)
	parsed, e := Parse(raw)
	if e != nil {
		t.Fatal(e)
	}
	got := parsed.Stacks[0]
	if got.BackendFiles == nil || got.VarFiles == nil || got.Secrets == nil || got.ChangePaths == nil || got.DependsOn == nil || got.Worker.Tags == nil || got.Approvals.Minimum != 1 {
		t.Fatal("omitted collections or approval default remained ambiguous", got)
	}
}
func TestManifestRejectsExcessiveAndAmbiguousYAMLSyntax(t *testing.T) {
	raw, _ := yaml.Marshal(Manifest{1, []Stack{fixture()}})
	for name, value := range map[string][]byte{
		"oversized": bytes.Repeat([]byte("x"), 1048577), "invalid UTF8": {0xff}, "empty": {}, "broken sequence": []byte("version: 1\nstacks: ["),
		"non-string mapping key": []byte("version: 1\nstacks: []\ntrue: value\n"),
		"implicit float":         bytes.Replace(raw, []byte("name: Fictional"), []byte("name: 1.25"), 1),
		"nested depth":           []byte("version: 1\nstacks: " + strings.Repeat("[", 34) + strings.Repeat("]", 34)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := Parse(value); e == nil {
				t.Fatal("unsupported syntax accepted")
			}
		})
	}
	for _, stacks := range [][]Stack{{fixture(), fixture()}, make([]Stack, 1001)} {
		raw, _ := yaml.Marshal(Manifest{1, stacks})
		if _, e := Parse(raw); e == nil {
			t.Fatal("duplicate or excessive stack declarations accepted")
		}
	}
}
func TestStackCollectionsCannotExpandScopeOrOverwriteSecretDestinations(t *testing.T) {
	for name, change := range map[string]func(*Stack){
		"too many input files": func(s *Stack) { s.VarFiles = make([]string, 33) },
		"duplicate input file": func(s *Stack) { s.BackendFiles = []string{"backend.hcl", "backend.hcl"} },
		"invalid tag":          func(s *Stack) { s.Worker.Tags = []string{"INVALID TAG"} },
		"self dependency":      func(s *Stack) { s.DependsOn = []string{s.ID} },
		"secret scope": func(s *Stack) {
			s.Secrets = []Secret{{Source: SecretSource{Scope: "account", ID: s.ID}, Env: "SERVICE_TOKEN"}}
		},
		"duplicate destination": func(s *Stack) {
			v := Secret{Source: SecretSource{Scope: "org", ID: s.ID}, Env: "SERVICE_TOKEN"}
			s.Secrets = []Secret{v, v}
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := fixture()
			change(&s)
			if s.Validate() == nil {
				t.Fatal("invalid collection accepted")
			}
		})
	}
	for _, branch := range []string{"main\x01", "feature/.hidden", "feature/end.lock", "feature//empty"} {
		if Branch(branch) {
			t.Fatal("ambiguous branch accepted", branch)
		}
	}
	if SecretEnv("invalid-name") {
		t.Fatal("invalid environment destination accepted")
	}
	if _, e := Canonical(make(chan int)); e == nil {
		t.Fatal("uncanonical value accepted")
	}
}
