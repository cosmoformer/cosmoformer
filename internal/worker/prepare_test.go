package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

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
