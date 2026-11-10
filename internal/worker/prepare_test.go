package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func TestPlanningRepositoryRelativeInputs(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		t.Run(map[bool]string{false: "root files only", true: "different nested files"}[shadow], func(t *testing.T) {
			a := inputAssignment(t, 0)
			a.StackID, a.ApprovalFloor = runs.ID(), 1
			s := a.Execution
			s.Source.Root = "infra"
			s.BackendFiles = []string{"config/state.hcl"}
			s.VarFiles = []string{"config/first.tfvars", "config/last.tfvars"}
			root, attempt := t.TempDir(), t.TempDir()
			write := func(p, value string) {
				t.Helper()
				p = filepath.Join(root, p)
				if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(p, []byte(value), 0600); e != nil {
					t.Fatal(e)
				}
			}
			backend := "bucket = \"synthetic-bucket\"\nkey = \"state\"\nregion = \"eu-west-1\"\nencrypt = true\nuse_lockfile = true\n"
			write("infra/main.tf", "terraform {\n backend \"s3\" {}\n}\n")
			write(s.BackendFiles[0], backend)
			for _, p := range s.VarFiles {
				write(p, "label = \""+p+"\"\n")
			}
			if shadow {
				write("infra/"+s.BackendFiles[0], "key = \"unreserved-state\"\nuse_lockfile = false\n")
				for _, p := range s.VarFiles {
					write("infra/"+p, "label = \"wrong nested input\"\n")
				}
			}
			declaration := catalog.Stack{ID: a.StackID, Name: "Synthetic nested stack", ProjectID: runs.ID(), EnvironmentID: runs.ID(), Root: s.Source.Root, TrackedBranch: "main", Engine: catalog.Engine{Name: s.Engine.Name, Version: s.Engine.Version}, Workspace: s.Workspace, BackendOwnerAccountID: s.Backend.ExpectedOwner, BackendFiles: s.BackendFiles, VarFiles: s.VarFiles, Worker: catalog.Pool{ID: s.Pool.ID}, Approvals: catalog.Approvals{Minimum: 1}}
			inspection, e := source.InspectResolved(root, declaration, "", nil)
			if e != nil || runs.Digest(inspection.Backend) != runs.Digest(s.Backend) {
				t.Fatal("preflight did not inspect the repository-relative backend", e)
			}
			for _, command := range []string{"init", "plan"} {
				_, cwd, args, e := nativeArgs(*s, attempt, root, command, false)
				if e != nil || cwd != filepath.Join(root, "infra") {
					t.Fatal("repository-relative inputs rejected or native cwd changed", command, e)
				}
				want := append([]string{}, runs.FixedFlags(command)...)
				if command == "init" {
					want = append(want, "-backend-config="+filepath.Join(root, s.BackendFiles[0]))
				} else {
					want = append(want, "-out="+filepath.Join(attempt, "plan.bin"))
					for _, p := range s.VarFiles {
						want = append(want, "-var-file="+filepath.Join(root, p))
					}
				}
				if runs.Digest(args) != runs.Digest(want) {
					t.Fatalf("native %s consumed different inputs: %v", command, args)
				}
			}
			trees := map[string]checkoutTree{"root": {ID: s.Source.RepositoryID, Commit: s.Source.Commit, Dir: root}}
			reviewed, e := reviewedInputs(a, trees, inspection)
			if e != nil {
				t.Fatal("repository-relative manifest rejected", e)
			}
			if reviewed.BackendFiles[0].Hash != hash([]byte(backend)) {
				t.Fatal("manifest bound a different backend input")
			}
			for i, p := range s.VarFiles {
				if reviewed.VarFiles[i].Path != p || reviewed.VarFiles[i].Hash != hash([]byte("label = \""+p+"\"\n")) {
					t.Fatal("manifest changed variable-file identity or precedence")
				}
			}
			for _, pair := range []struct{ command, file string }{{"init", s.BackendFiles[0]}, {"plan", s.VarFiles[0]}} {
				if e := os.Remove(filepath.Join(root, pair.file)); e != nil {
					t.Fatal(e)
				}
				if _, _, _, e := nativeArgs(*s, attempt, root, pair.command, false); e == nil {
					t.Fatal("missing root input fell back to nested file", pair.command)
				}
			}
		})
	}
}

func TestPlanningMeasuresCompleteModuleTreesAndNativeCopies(t *testing.T) {
	a := inputAssignment(t, 0)
	a.StackID = runs.ID()
	a.ApprovalFloor = 1
	s := a.Execution
	root, remote, attempt := t.TempDir(), t.TempDir(), t.TempDir()
	moduleID := runs.ID()
	moduleURL := "https://bitbucket.org/fictional/module.git"
	commit := strings.Repeat("b", 40)
	s.Grants = append(s.Grants, runs.ResourceRef{ID: runs.ID(), Kind: "repository", ResourceID: moduleID, Revision: 1})
	literal := "git::" + moduleURL + "?ref=" + commit
	write := func(p, v string) {
		t.Helper()
		if e := os.WriteFile(p, []byte(v), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(filepath.Join(root, "main.tf"), `module "first" { source = "`+literal+`" }`)
	write(filepath.Join(remote, "main.tf"), `output "fixture" { value = "synthetic" }`)
	write(filepath.Join(remote, "script.sh"), "tracked non-HCL input\n")
	trees := map[string]checkoutTree{s.Source.URL + "@" + s.Source.Commit: {ID: s.Source.RepositoryID, Commit: s.Source.Commit, Dir: root, URL: s.Source.URL}, moduleURL + "@" + commit: {ID: moduleID, Commit: commit, Dir: remote, URL: moduleURL}}
	inspected := source.Inspection{Modules: []source.Module{{Path: "first", Source: literal, Checkout: root, CallerDirectory: "."}}}
	reviewed, e := reviewedInputs(a, trees, inspected)
	if e != nil {
		t.Fatal(e)
	}
	if len(reviewed.Files) != 3 || len(reviewed.Modules) != 1 || reviewed.Modules[0].TargetRepo != moduleID {
		t.Fatal("incomplete pinned module manifest")
	}
	copyDir := filepath.Join(attempt, "data", "modules", "first")
	if e = os.MkdirAll(copyDir, 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"main.tf", "script.sh"} {
		b, _ := os.ReadFile(filepath.Join(remote, name))
		write(filepath.Join(copyDir, name), string(b))
	}
	raw, _ := json.Marshal(map[string]any{"Modules": []map[string]string{{"Key": "", "Source": "", "Dir": root}, {"Key": "first", "Source": literal, "Dir": copyDir}}})
	write(filepath.Join(attempt, "data", "modules", "modules.json"), string(raw))
	if e = verifyInstalledModules(attempt, root, trees, inspected); e != nil {
		t.Fatal("exact native closure rejected", e)
	}
	write(filepath.Join(copyDir, "script.sh"), "altered installed script")
	if verifyInstalledModules(attempt, root, trees, inspected) == nil {
		t.Fatal("native module non-HCL mutation accepted")
	}
	write(filepath.Join(remote, "script.sh"), "altered reviewed input")
	if verifyTrees(reviewed, trees, ".terraform.lock.hcl", s.Source.RepositoryID) == nil {
		t.Fatal("post-init input mutation accepted")
	}
}
func TestPlanningRejectsGeneratedPathAndEscapingSymlinkInputs(t *testing.T) {
	for _, scenario := range []string{"reserved", "escaping symlink", "lock mutation"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			tree := checkoutTree{ID: runs.ID(), Commit: strings.Repeat("a", 40), Dir: root}
			if e := os.WriteFile(filepath.Join(root, "main.tf"), []byte("# synthetic"), 0600); e != nil {
				t.Fatal(e)
			}
			if scenario == "reserved" {
				os.Mkdir(filepath.Join(root, ".terraform"), 0700)
			}
			if scenario == "escaping symlink" {
				if e := os.Symlink("../outside", filepath.Join(root, "escape")); e != nil {
					t.Fatal(e)
				}
			}
			files, e := snapshotTree(tree, "")
			if scenario != "lock mutation" {
				if e == nil {
					t.Fatal("unsafe tracked tree accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			trees := map[string]checkoutTree{"root": tree}
			before := runs.ReviewedInputs{Commit: tree.Commit, Files: files}
			if e = os.WriteFile(filepath.Join(root, ".terraform.lock.hcl"), []byte("# native generated"), 0600); e != nil {
				t.Fatal(e)
			}
			if verifyTrees(before, trees, ".terraform.lock.hcl", tree.ID) != nil {
				t.Fatal("documented generated lock exception rejected")
			}
			if verifyNativeLock(filepath.Join(root, ".terraform.lock.hcl"), runs.Manifest{}) == nil {
				t.Fatal("unexpected lock accepted for retained absence")
			}
		})
	}
}
