package source

import (
	"context"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func attributes() map[string]any {
	return map[string]any{"bucket": "fictional-state-bucket", "key": "prod/state.tfstate", "region": "eu-west-1", "encrypt": true, "use_lockfile": true}
}
func TestBackendIdentityAndMetadataClaims(t *testing.T) {
	a := attributes()
	first, e := CanonicalBackend(a, backendContext("default"), "")
	if e != nil {
		t.Fatal(e)
	}
	if first.Claims[1].Key != "prod/state.tfstate.tflock" {
		t.Fatal("missing permanent lock claim")
	}
	a["key"] = "state.tfstate"
	a["workspace_key_prefix"] = "env:"
	workspace, e := CanonicalBackend(a, backendContext("production"), "")
	if e != nil || workspace.Target.Key != "env:/production/state.tfstate" {
		t.Fatal(workspace, e)
	}
	a["key"] = "env:/production/state.tfstate"
	plain, e := CanonicalBackend(a, backendContext("default"), "")
	if e != nil || plain.Target != workspace.Target {
		t.Fatal("workspace alias changed ownership domain")
	}
	a["endpoints"] = true
	if _, e = CanonicalBackend(a, backendContext("default"), ""); e == nil {
		t.Fatal("accepted custom endpoint")
	}
}
func TestDynamoClaimsUseExecutionTableAccount(t *testing.T) {
	a := attributes()
	a["dynamodb_table"] = "fictional-locks"
	table := "arn:aws:dynamodb:eu-west-1:999900001111:table/fictional-locks"
	b, e := CanonicalBackend(a, backendContext("default"), table)
	if e != nil || len(b.Claims) != 4 || b.Profile != "dual" || b.Claims[3].Key != "fictional-state-bucket/prod/state.tfstate-md5" || b.Claims[2].Namespace != table {
		t.Fatal(b, e)
	}
}
func TestSourceInspectionAndNoEvaluation(t *testing.T) {
	root := t.TempDir()
	s := catalog.Stack{ID: "11111111-1111-4111-8111-111111111111", Name: "Fictional", ProjectID: "22222222-2222-4222-8222-222222222222", EnvironmentID: "33333333-3333-4333-8333-333333333333", Root: ".", TrackedBranch: "main", Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", BackendOwnerAccountID: "111122223333", Worker: catalog.Pool{ID: "44444444-4444-4444-8444-444444444444"}, Approvals: catalog.Approvals{Minimum: 1}}
	good := `terraform {
backend "s3" {
 bucket = "fictional-state-bucket"
 key = "prod/state.tfstate"
 region = "eu-west-1"
 encrypt = true
 use_lockfile = true
}
}
output "example" { value = "synthetic only" }
`
	file := filepath.Join(root, "main.tf")
	if e := os.WriteFile(file, []byte(good), 0600); e != nil {
		t.Fatal(e)
	}
	v, e := Inspect(root, s, "")
	if e != nil || len(v.Files) != 1 || len(v.Digest) != 64 {
		t.Fatal(v, e)
	}
	for name, body := range map[string]string{"expression": strings.Replace(good, `"prod/state.tfstate"`, `var.state_key`, 1), "credential": strings.Replace(good, "encrypt = true", "access_key = \"fictional\"\nencrypt = true", 1), "ephemeral": good + "variable \"secret\" { ephemeral = true }", "dynamic module": good + "module \"bad\" { source = var.source }"} {
		t.Run(name, func(t *testing.T) {
			os.WriteFile(file, []byte(body), 0600)
			if _, e := Inspect(root, s, ""); e == nil {
				t.Fatal("unsupported source accepted")
			}
		})
	}
	os.WriteFile(file, []byte(good), 0600)
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("private fixture"), 0600)
	os.Symlink(outside, filepath.Join(root, "escape"))
	if _, e := Inspect(root, s, ""); e == nil {
		t.Fatal("escape symlink accepted")
	}
}
func TestGitUsesExactCommitAndFixedConfiguration(t *testing.T) {
	calls := [][]string{}
	commit := strings.Repeat("a", 40)
	g := Git{Run: func(ctx context.Context, dir string, args, env []string) ([]byte, error) {
		calls = append(calls, args)
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "rev-parse") {
			return []byte(commit + "\n"), nil
		}
		return nil, nil
	}}
	if e := g.Checkout(context.Background(), filepath.Join(t.TempDir(), "checkout"), "https://bitbucket.org/fictional/repo.git", commit, ""); e != nil {
		t.Fatal(e)
	}
	all := ""
	for _, c := range calls {
		for _, required := range []string{"maintenance.auto=false", "gc.auto=0"} {
			if !strings.Contains(strings.Join(c, " "), "-c "+required) {
				t.Fatal("Git command permits background maintenance", c)
			}
		}
		all += strings.Join(c, " ") + "\n"
	}
	for _, required := range []string{"protocol.allow=never", "core.hooksPath=/dev/null", "http.followRedirects=false", "credential.useHttpPath=true", "--no-tags", "--detach " + commit} {
		if !strings.Contains(all, required) {
			t.Fatal("missing Git boundary", required)
		}
	}
	for _, url := range []string{"file:///tmp/repo", "https://token@bitbucket.org/fictional/repo.git", "ssh://bitbucket.org/fictional/repo.git", "https://attacker.example/repo.git", "https://bitbucket.org/fictional/repo.git?ref=main"} {
		if Repository(url) {
			t.Fatal("accepted unsupported URL")
		}
	}
	if _, e := ParseModule("git::https://bitbucket.org/fictional/modules.git//ec2?ref=" + commit); e != nil {
		t.Fatal(e)
	}
}

func backendContext(workspace string) BackendContext {
	return BackendContext{Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: workspace, ExpectedOwner: "111122223333"}
}
