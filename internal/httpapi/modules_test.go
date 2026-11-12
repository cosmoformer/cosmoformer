//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"go.yaml.in/yaml/v3"
)

func moduleBody(f *sourceFixture, project string) map[string]any {
	b := map[string]any{"repository_id": f.Repo, "commit": f.Remote.Commit, "subdirectory": "modules/instance", "name": "Synthetic instance", "instructions": "Peer-reviewed usage for this exact commit. No real cloud resources.", "example_paths": []string{"examples/basic"}, "archived": false}
	if project != "" {
		b["project_id"] = project
	}
	return b
}
func moduleID(t *testing.T, f *sourceFixture, b map[string]any) string {
	t.Helper()
	w := f.browser(t, "POST", "/api/v1/organizations/"+f.Org+"/modules", b, 200)
	var r struct {
		ID string `json:"id"`
	}
	if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil || r.ID == "" {
		t.Fatal(e, w.Body.String())
	}
	return r.ID
}
func readModule(t *testing.T, f *sourceFixture, id string, status int) ModulePublic {
	t.Helper()
	w := f.browser(t, "GET", "/api/v1/organizations/"+f.Org+"/modules/"+id, nil, status)
	var m ModulePublic
	if status == 200 {
		if e := json.Unmarshal(w.Body.Bytes(), &m); e != nil {
			t.Fatal(e)
		}
	}
	return m
}
func TestModuleCatalogScopeRevisionAndPinnedConsumption(t *testing.T) {
	f := newSourceFixture(t)
	s, _ := f.tracked(t)
	base := "/api/v1/organizations/" + f.Org + "/modules"
	b := moduleBody(f, s.ProjectID)
	id := moduleID(t, f, b)
	m := readModule(t, f, id, 200)
	parsed, e := source.ParseModule(m.Source)
	if e != nil || parsed.Commit != f.Remote.Commit || parsed.Repository != m.RepositoryURL || parsed.Subdirectory != "modules/instance" || len(m.Examples) != 1 || !strings.Contains(m.Examples[0].URL, "/src/"+m.Commit+"/examples/basic") {
		t.Fatal("unpinned instructions", m, e)
	}
	// Consume the returned source in real recursive HCL inspection. The resolver
	// supplies only this fictional pinned checkout; no catalog credential exists.
	root, mod := t.TempDir(), t.TempDir()
	main := "terraform {\n backend \"s3\" {\n bucket=\"fictional-bucket\"\n key=\"state.tfstate\"\n region=\"eu-west-1\"\n encrypt=true\n use_lockfile=true\n }\n}\n" + m.Snippet
	if e = os.WriteFile(filepath.Join(root, "main.tf"), []byte(main), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(filepath.Join(mod, "modules", "instance"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(mod, "modules", "instance", "main.tf"), []byte("output \"fixture\" { value = \"synthetic\" }\n"), 0600); e != nil {
		t.Fatal(e)
	}
	s.Root = "."
	s.BackendFiles = []string{}
	s.VarFiles = []string{}
	inspected, e := source.InspectResolved(root, s, "", func(ref source.ModuleSource) (string, error) {
		if ref != parsed {
			t.Fatal("catalog source retargeted", ref)
		}
		return mod, nil
	})
	if e != nil || len(inspected.Modules) == 0 {
		t.Fatal("catalog source could not be consumed", e)
	}
	// A failed read-only grant resolver still blocks consumption.
	if _, e = source.InspectResolved(root, s, "", func(source.ModuleSource) (string, error) { return "", source.ErrSource }); e == nil {
		t.Fatal("catalog bypassed source grants")
	}
	patch := moduleBody(f, "")
	patch["expected_revision"] = 1
	patch["commit"] = strings.Repeat("b", 40)
	f.browser(t, "PATCH", base+"/"+id, patch, 200)
	f.browser(t, "GET", base+"/"+id+"/usage-instructions?expected_revision=1", nil, 409)
	f.browser(t, "PATCH", base+"/"+id, patch, 409)
	changed := readModule(t, f, id, 200)
	if changed.Commit != strings.Repeat("b", 40) || changed.Revision != 2 {
		t.Fatal(changed)
	}
	// Discovery is not permission to register, mutate, or retrieve a token.
	f.exec(t, `UPDATE cosmoformer.memberships SET org_admin=false WHERE org_id=$1 AND account_id=$2`, f.Org, f.Account)
	f.exec(t, `UPDATE cosmoformer.accounts SET installation_admin=false WHERE id=$1`, f.Account)
	if !readModule(t, f, id, 200).Available {
		t.Fatal("environment-scoped viewer lost project catalog")
	}
	patch["expected_revision"] = 2
	f.browser(t, "PATCH", base+"/"+id, patch, 404)
	f.browser(t, "POST", base, moduleBody(f, s.ProjectID), 404)
	// Project-wide configuration administration can manage this catalog without
	// granting shared registration or execution/evidence access.
	f.exec(t, `UPDATE cosmoformer.grants SET role='administrator',environment_id=NULL WHERE project_id=$1`, s.ProjectID)
	if !readModule(t, f, id, 200).CanManage {
		t.Fatal("project administrator cannot manage catalog metadata")
	}
	f.browser(t, "PATCH", base+"/"+id, patch, 200)
	moduleID(t, f, moduleBody(f, s.ProjectID))
	f.browser(t, "POST", base, moduleBody(f, ""), 404)
	// No project grants means no shared catalog discovery either.
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp()`)
	f.browser(t, "GET", base+"/"+id, nil, 404)
	var empty ModulesOutput
	w := f.browser(t, "GET", base, nil, 200)
	json.Unmarshal(w.Body.Bytes(), &empty.Body)
	if len(empty.Body.Modules) != 0 || len(empty.Body.Repositories) != 0 || empty.Body.CanRegister {
		t.Fatal("empty access leaked catalog", empty)
	}
}
func TestModuleCatalogFilteringExamplesAndReplay(t *testing.T) {
	f := newSourceFixture(t)
	s, _ := f.tracked(t)
	base := "/api/v1/organizations/" + f.Org + "/modules"
	for _, change := range []string{"commit", "directory", "example", "token URL"} {
		b := moduleBody(f, s.ProjectID)
		switch change {
		case "commit":
			b["commit"] = "main"
		case "directory":
			b["subdirectory"] = "../escape"
		case "example":
			b["example_paths"] = []string{"https://attacker.example/a"}
		case "token URL":
			b["repository_id"] = runs.ID()
		}
		status := 422
		if change == "token URL" {
			status = 404
		}
		f.browser(t, "POST", base, b, status)
	}
	b := moduleBody(f, s.ProjectID)
	key := runs.ID()
	first := f.browserKey(t, "POST", base, b, key, 200)
	again := f.browserKey(t, "POST", base, b, key, 200)
	if first.Body.String() != again.Body.String() {
		t.Fatal("registration replay duplicated identity")
	}
	for _, name := range []string{"Alpha", "Zeta"} {
		b := moduleBody(f, s.ProjectID)
		b["name"] = name
		moduleID(t, f, b)
	}
	shared := moduleID(t, f, moduleBody(f, ""))
	hiddenProject, hidden := runs.ID(), runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.projects(id,org_id,name) VALUES($1,$2,'Hidden project')`, hiddenProject, f.Org)
	f.exec(t, `INSERT INTO cosmoformer.module_entries(id,org_id,project_id,repository_id,commit,subdirectory,name,instructions,example_links) VALUES($1,$2,$3,$4,$5,'.','A hidden module','Hidden instructions','[]')`, hidden, f.Org, hiddenProject, f.Repo, f.Remote.Commit)
	f.exec(t, `UPDATE cosmoformer.memberships SET org_admin=false WHERE org_id=$1 AND account_id=$2`, f.Org, f.Account)
	f.exec(t, `UPDATE cosmoformer.accounts SET installation_admin=false WHERE id=$1`, f.Account)
	read := func(q string) ModulesOutput {
		var out ModulesOutput
		w := f.browser(t, "GET", base+q, nil, 200)
		if e := json.Unmarshal(w.Body.Bytes(), &out.Body); e != nil {
			t.Fatal(e)
		}
		return out
	}
	page := read("?limit=1")
	if readModule(t, f, shared, 200).Project != "" {
		t.Fatal("shared catalog scope changed")
	}
	if len(page.Body.Modules) != 1 || page.Body.Modules[0].Name != "Alpha" || page.Body.Next == "" {
		t.Fatal(page)
	}
	next := read("?limit=1&before=" + page.Body.Next)
	if len(next.Body.Modules) != 1 || next.Body.Modules[0].Name == "Alpha" {
		t.Fatal(next)
	}
	if len(read("?before="+hidden).Body.Modules) != 0 {
		t.Fatal("hidden cursor accepted")
	}
	if len(read("?query=Zeta").Body.Modules) != 1 {
		t.Fatal("catalog search failed")
	}
	f.browserKey(t, "POST", base, b, key, 404)
	// Archived and disabled entries remain historical metadata, not a usable source.
	f.exec(t, `UPDATE cosmoformer.module_entries SET archived=true WHERE name='Zeta'`)
	z := read("?query=Zeta")
	if len(z.Body.Modules) != 0 {
		t.Fatal(z)
	}
	id := page.Body.Modules[0].ID
	f.exec(t, `UPDATE cosmoformer.repositories SET enabled=false WHERE id=$1`, f.Repo)
	if readModule(t, f, id, 200).Available {
		t.Fatal("disabled source is advertised available")
	}
	f.browser(t, "GET", base+"/"+id+"/usage-instructions", nil, 409)
	f.exec(t, `UPDATE cosmoformer.repositories SET enabled=true WHERE id=$1`, f.Repo)
	f.exec(t, `UPDATE cosmoformer.integrations SET reconnect_required=true WHERE id=$1`, f.Integration)
	if readModule(t, f, id, 200).Available {
		t.Fatal("disconnected integration is advertised as usable")
	}
	f.browser(t, "GET", base+"/"+id+"/usage-instructions", nil, 409)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp()`)
	f.browser(t, "GET", base+"/"+shared, nil, 404)
}
func TestModuleCatalogMetadataCanBeArchivedAfterDisconnect(t *testing.T) {
	for _, state := range []string{"repository disabled", "integration disabled", "reconnect required"} {
		t.Run(state, func(t *testing.T) {
			f := newSourceFixture(t)
			s, _ := f.tracked(t)
			id := moduleID(t, f, moduleBody(f, s.ProjectID))
			switch state {
			case "repository disabled":
				f.exec(t, `UPDATE cosmoformer.repositories SET enabled=false WHERE id=$1`, f.Repo)
			case "integration disabled":
				f.exec(t, `UPDATE cosmoformer.integrations SET enabled=false WHERE id=$1`, f.Integration)
			case "reconnect required":
				f.exec(t, `UPDATE cosmoformer.integrations SET reconnect_required=true WHERE id=$1`, f.Integration)
			}
			base := "/api/v1/organizations/" + f.Org + "/modules/" + id
			patch := moduleBody(f, "")
			patch["expected_revision"], patch["archived"] = 1, true
			patch["instructions"] = "Retired source. Keep the original pin."
			f.browser(t, "PATCH", base, patch, 200)
			m := readModule(t, f, id, 200)
			if !m.Archived || m.Available || m.Revision != 2 || m.Commit != f.Remote.Commit {
				t.Fatal("disconnected catalog metadata was not archived", m)
			}
			f.browser(t, "GET", base+"/usage-instructions", nil, 409)
			// A metadata edit must not authorize a new binding through a disabled
			// connection or an inaccessible repository.
			patch["expected_revision"], patch["commit"] = 2, strings.Repeat("c", 40)
			f.browser(t, "PATCH", base, patch, 404)
			patch["commit"], patch["repository_id"] = f.Remote.Commit, runs.ID()
			f.browser(t, "PATCH", base, patch, 404)
		})
	}
}

func TestTrackedDependenciesAreScopedManualMetadata(t *testing.T) {
	f := newSourceFixture(t)
	_, owner := f.tracked(t)
	m, _ := catalog.Parse(f.Remote.Manifest)
	target := m.Stacks[0]
	target.ID = runs.ID()
	target.Name = "Dependency"
	m.Stacks[0].DependsOn = []string{target.ID}
	m.Stacks = append(m.Stacks, target)
	f.Remote.Manifest, _ = yaml.Marshal(m)
	f.Remote.Commit = strings.Repeat("b", 40)
	f.reconcile(t, "branch", 0)
	path := "/api/v1/organizations/" + f.Org + "/stacks/" + owner + "/dependencies"
	var out DependenciesOutput
	w := f.browser(t, "GET", path, nil, 200)
	json.Unmarshal(w.Body.Bytes(), &out.Body)
	if len(out.Body.Dependencies) != 1 || out.Body.Revision == "" {
		t.Fatal(out)
	}
	dependency := out.Body.Dependencies[0]
	// Edges may be cyclic because they have no scheduler semantics.
	m.Stacks[1].DependsOn = []string{m.Stacks[0].ID}
	f.Remote.Manifest, _ = yaml.Marshal(m)
	f.Remote.Commit = strings.Repeat("c", 40)
	f.reconcile(t, "branch", 0)
	// Hide the target environment; do not leak its ID/name or a hidden-edge count.
	hiddenEnv := runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.environments(id,org_id,project_id,name) VALUES($1,$2,$3,'Hidden')`, hiddenEnv, f.Org, dependency.Project)
	f.exec(t, `UPDATE cosmoformer.stacks SET environment_id=$2 WHERE id=$1`, dependency.ID, hiddenEnv)
	w = f.browser(t, "GET", path, nil, 200)
	json.Unmarshal(w.Body.Bytes(), &out.Body)
	if len(out.Body.Dependencies) != 0 {
		t.Fatal("hidden dependency leaked", out)
	}
	f.exec(t, `UPDATE cosmoformer.stacks SET environment_id=$2 WHERE id=$1`, dependency.ID, dependency.Environment)
	// Unknown declarations reject the import atomically and preserve prior edges.
	m.Stacks[0].DependsOn = []string{runs.ID()}
	f.Remote.Manifest, _ = yaml.Marshal(m)
	f.Remote.Commit = strings.Repeat("d", 40)
	if e := f.Server.reconcileSource(context.Background(), f.Org, f.Repo, "branch", 0); e == nil {
		t.Fatal("unknown dependency accepted")
	}
	var n int
	f.DB.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.stack_dependencies`).Scan(&n)
	if n != 2 {
		t.Fatal("failed import partly rewrote dependency graph", n)
	}
}
