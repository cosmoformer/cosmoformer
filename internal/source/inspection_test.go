package source

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
)

func inspectionStack() catalog.Stack {
	return catalog.Stack{ID: "11111111-1111-4111-8111-111111111111", Name: "Synthetic", ProjectID: "11111111-1111-4111-8111-111111111111", EnvironmentID: "11111111-1111-4111-8111-111111111111", Root: ".", TrackedBranch: "main", Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", BackendOwnerAccountID: "111122223333", Worker: catalog.Pool{ID: "11111111-1111-4111-8111-111111111111"}, Approvals: catalog.Approvals{Minimum: 1}}
}

const backendFixture = `terraform {
 backend "s3" {
 bucket = "fictional-state-bucket"
 key = "prod/state.tfstate"
 region = "eu-west-1"
 encrypt = true
 use_lockfile = true
 }
}
`

func writeFixture(t *testing.T, root, path, body string) {
	t.Helper()
	p := filepath.Join(root, path)
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestBackendRejectsUntrustedIdentityAndLocking(t *testing.T) {
	for name, update := range map[string]map[string]any{
		"bucket IP": {"bucket": "127.0.0.1"}, "bucket alias": {"bucket": "fictional-s3alias"}, "bucket dots": {"bucket": "fictional..bucket"},
		"missing lock": {"use_lockfile": false}, "encryption disabled": {"encrypt": false}, "boolean string": {"encrypt": "true"}, "numeric region": {"region": 1},
		"unknown region": {"region": "unknown"}, "owner": {"expected_bucket_owner": "123"}, "null key": {"key": "a\x00b"}, "long key": {"key": strings.Repeat("a", 1020)},
		"foreign KMS": {"kms_key_id": "arn:aws-cn:kms:eu-west-1:111122223333:key/fictional"}, "table missing ARN": {"dynamodb_table": "locks"},
	} {
		t.Run(name, func(t *testing.T) {
			a := attributes()
			for k, v := range update {
				a[k] = v
			}
			if _, e := CanonicalBackend(a, backendContext("default"), ""); e == nil {
				t.Fatal("unsafe backend accepted")
			}
		})
	}
	for _, region := range []struct{ region, partition string }{{"cn-north-1", "aws-cn"}, {"us-gov-west-1", "aws-us-gov"}} {
		a := attributes()
		a["region"] = region.region
		a["dynamodb_table"] = "locks"
		a["use_lockfile"] = false
		arn := "arn:" + region.partition + ":dynamodb:" + region.region + ":999900001111:table/locks"
		b, e := CanonicalBackend(a, backendContext("review"), arn)
		if e != nil || b.Target.Partition != region.partition || b.Target.Key != "env:/review/prod/state.tfstate" || b.Profile != "dynamodb" || len(b.Claims) != 4 {
			t.Fatal(b, e)
		}
	}
	if _, e := CanonicalBackend(attributes(), backendContext("../escape"), ""); e == nil {
		t.Fatal("unsafe workspace accepted")
	}
}
func TestInspectionRetainsModuleClosureAndSensitiveInputs(t *testing.T) {
	root, remote := t.TempDir(), t.TempDir()
	s := inspectionStack()
	s.BackendFiles = []string{"state.hcl"}
	s.VarFiles = []string{"values.tfvars"}
	s.Secrets = []catalog.Secret{{Env: "TF_VAR_password", Source: catalog.SecretSource{ID: "11111111-1111-4111-8111-111111111111", Scope: "project"}}}
	url := "git::https://bitbucket.org/fictional/modules.git//service?ref=" + strings.Repeat("a", 40)
	writeFixture(t, root, "main.tf", backendFixture+`variable "password" { sensitive = true }
module "local" { source = "./local" }
module "remote" { source = "`+url+`" }
`)
	writeFixture(t, root, "local/main.tf", `module "again" { source = "../local" }`)
	writeFixture(t, remote, "service/main.tf", `output "fixture" { value = "local only" }`)
	writeFixture(t, root, "state.hcl", `key = "overridden.tfstate"`)
	writeFixture(t, root, "values.tfvars", `label = "fictional"`)
	writeFixture(t, root, "script", "synthetic executable")
	if e := os.Chmod(filepath.Join(root, "script"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("values.tfvars", filepath.Join(root, "values-link")); e != nil {
		t.Fatal(e)
	}
	writeFixture(t, root, ".git/ignored", "not source")
	calls := 0
	inspect := func() Inspection {
		t.Helper()
		v, e := InspectResolved(root, s, "", func(m ModuleSource) (string, error) {
			calls++
			if m.Repository != "https://bitbucket.org/fictional/modules.git" || m.Subdirectory != "service" {
				t.Fatal(m)
			}
			return remote, nil
		})
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	first := inspect()
	second := inspect()
	if first.Digest != second.Digest || first.Backend.Target.Key != "overridden.tfstate" || calls != 2 || len(first.Modules) != 3 {
		t.Fatal("module closure or stable digest lost", first)
	}
	s.BackendOwnerAccountID = "999900001111"
	changedOwner := inspect()
	if changedOwner.Digest == first.Digest || changedOwner.Backend.ExpectedOwner != s.BackendOwnerAccountID || changedOwner.Backend.Target != first.Backend.Target {
		t.Fatal("owner intent must change reviewed identity without changing canonical ownership")
	}
	if _, exists := changedOwner.Backend.Attributes["expected_bucket_owner"]; exists {
		t.Fatal("Cosmoformer owner metadata leaked into native backend attributes")
	}
	s.BackendOwnerAccountID = "111122223333"
	modes := map[string]uint32{}
	for _, f := range first.Files {
		modes[f.Path] = f.Mode
		if strings.HasPrefix(f.Path, ".git/") {
			t.Fatal("Git metadata retained")
		}
	}
	if modes["script"] != 0100755 || modes["values-link"] != 0120000 {
		t.Fatal("source modes lost", modes)
	}
	writeFixture(t, remote, "service/main.tf", `output "fixture" { value = "changed" }`)
	if inspect().Digest == first.Digest {
		t.Fatal("remote change not fenced by digest")
	}
	sentinel := errors.New("synthetic resolver refusal")
	if _, e := InspectResolved(root, s, "", func(ModuleSource) (string, error) { return "", sentinel }); e == nil {
		t.Fatal("resolver error ignored")
	}
}
func TestInspectionRejectsUnsupportedTrees(t *testing.T) {
	for name, body := range map[string]string{
		"missing backend":       `output "x" { value = "fixture" }`,
		"duplicate backend":     backendFixture + backendFixture,
		"foreign backend":       strings.Replace(backendFixture, `backend "s3"`, `backend "local"`, 1),
		"module escape":         backendFixture + `module "x" { source = "../outside" }`,
		"module missing source": backendFixture + `module "x" { count = 1 }`,
		"registry module":       backendFixture + `module "x" { source = "fictional/fixture/aws" }`,
		"literal object":        strings.Replace(backendFixture, `encrypt = true`, `encrypt = { value = true }`, 1),
		"arithmetic":            strings.Replace(backendFixture, `encrypt = true`, `encrypt = 1 + 1`, 1),
		"number":                strings.Replace(backendFixture, `encrypt = true`, `encrypt = 1`, 1),
		"null":                  strings.Replace(backendFixture, `encrypt = true`, `encrypt = null`, 1),
		"malformed":             `terraform {`,
		"variable expression":   backendFixture + `variable "password" { sensitive = var.flag }`,
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "main.tf", body)
			if _, e := Inspect(root, inspectionStack(), ""); e == nil {
				t.Fatal("unsupported tree accepted")
			}
		})
	}
	for _, name := range []string{".gitmodules", "pointer", "huge", "directory"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "main.tf", backendFixture)
			switch name {
			case ".gitmodules":
				writeFixture(t, root, name, "fixture")
			case "pointer":
				writeFixture(t, root, name, "version https://git-lfs.github.com/spec/v1\n")
			case "huge":
				writeFixture(t, root, name, strings.Repeat("x", 1048577))
			case "directory":
				if e := os.Mkdir(filepath.Join(root, "not-a-file"), 0700); e != nil {
					t.Fatal(e)
				}
				if _, e := Read(root, "not-a-file"); e == nil {
					t.Fatal("directory read accepted")
				}
				return
			}
			if _, e := Inspect(root, inspectionStack(), ""); e == nil {
				t.Fatal("unsupported closure accepted")
			}
		})
	}
}

func TestBackendOwnerMetadataAndNativeKeyProfiles(t *testing.T) {
	for _, engine := range []catalog.Engine{{Name: "opentofu", Version: "1.13.8"}, {Name: "terraform", Version: "1.17.1"}} {
		for _, workspace := range []string{"default", "review"} {
			t.Run(engine.Name+"/"+workspace, func(t *testing.T) {
				ctx := backendContext(workspace)
				ctx.Engine = engine
				for _, key := range []string{"state.tfstate", "a//state", "a/../state", "/state", "state/"} {
					a := attributes()
					a["key"] = key
					a["workspace_key_prefix"] = "workspaces//fixtures/.."
					b, e := CanonicalBackend(a, ctx, "")
					reject := strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || (engine.Name == "terraform" && strings.Contains(key, "//"))
					if (e != nil) != reject {
						t.Fatalf("key %q: error %v", key, e)
					}
					if !reject {
						want := key
						if workspace != "default" {
							want = path.Join("workspaces//fixtures/..", workspace, key)
						}
						if b.Target.Key != want || b.ExpectedOwner != ctx.ExpectedOwner {
							t.Fatal("key normalized or owner metadata lost", b)
						}
					}
				}
				for _, prefix := range []string{"/prefix", "prefix/"} {
					a := attributes()
					a["workspace_key_prefix"] = prefix
					if _, e := CanonicalBackend(a, ctx, ""); e == nil {
						t.Fatal("native-invalid prefix accepted")
					}
				}
				for _, owner := range []string{"", "123", "not-an-account"} {
					bad := ctx
					bad.ExpectedOwner = owner
					if _, e := CanonicalBackend(attributes(), bad, ""); e == nil {
						t.Fatal("invalid owner metadata accepted")
					}
				}
				a := attributes()
				a["expected_bucket_owner"] = "111122223333"
				if _, e := CanonicalBackend(a, ctx, ""); e == nil {
					t.Fatal("unsupported native owner argument accepted")
				}
			})
		}
	}
	ctx := backendContext("default")
	ctx.Engine.Version = "0.0.0"
	if _, e := CanonicalBackend(attributes(), ctx, ""); e == nil {
		t.Fatal("unassessed native key profile accepted")
	}
}

func TestOwnerSessionPolicyPreservesPermissionsAndDeniesBackendOwnerMismatch(t *testing.T) {
	for _, partition := range []string{"aws", "aws-cn", "aws-us-gov"} {
		t.Run(partition, func(t *testing.T) {
			b := Backend{Target: Target{Partition: partition, Bucket: "fictional-state-bucket"}, ExpectedOwner: "111122223333"}
			raw, e := b.OwnerSessionPolicy()
			if e != nil {
				t.Fatal(e)
			}
			var policy struct {
				Version    string
				Statements []struct {
					Effect    string
					Action    string
					Resource  json.RawMessage
					Condition map[string]map[string]string
				} `json:"Statement"`
			}
			if e = json.Unmarshal(raw, &policy); e != nil || policy.Version != "2012-10-17" || len(policy.Statements) != 2 {
				t.Fatal("invalid owner policy", e)
			}
			allow, deny := policy.Statements[0], policy.Statements[1]
			var all string
			json.Unmarshal(allow.Resource, &all)
			if allow.Effect != "Allow" || allow.Action != "*" || all != "*" {
				t.Fatal("role permission intersection changed")
			}
			var resources []string
			json.Unmarshal(deny.Resource, &resources)
			arn := "arn:" + partition + ":s3:::fictional-state-bucket"
			if deny.Effect != "Deny" || deny.Action != "s3:*" || !reflect.DeepEqual(resources, []string{arn, arn + "/*"}) || deny.Condition["StringNotEquals"]["s3:ResourceAccount"] != "111122223333" {
				t.Fatal("owner deny missing or scoped to wrong resource/account")
			}
			if len(raw) > 2048 {
				t.Fatal("owner policy exceeds STS policy bound")
			}
		})
	}
	for _, b := range []Backend{{Target: Target{"aws", "fictional-state-bucket", ""}}, {Target: Target{"unknown", "fictional-state-bucket", ""}, ExpectedOwner: "111122223333"}, {Target: Target{"aws", "bucket*", ""}, ExpectedOwner: "111122223333"}} {
		if _, e := b.OwnerSessionPolicy(); e == nil {
			t.Fatal("invalid owner policy input accepted")
		}
	}
}

func TestModuleResolverPreservesUnprovenStop(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "main.tf", backendFixture+`module "remote" { source = "git::https://bitbucket.org/fictional/modules.git?ref=`+strings.Repeat("a", 40)+`" }`)
	for _, sentinel := range []error{ErrProcess, errors.New("synthetic resolver failure")} {
		_, e := InspectResolved(root, inspectionStack(), "", func(ModuleSource) (string, error) { return "", sentinel })
		if !errors.Is(e, sentinel) {
			t.Fatal("module resolver error identity lost", e)
		}
	}
}
