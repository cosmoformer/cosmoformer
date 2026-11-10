package runs

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// Pinned root, local module, remote module and symlinks are exercised together:
// validation must distinguish the complete authorized closure from extra files.
func reviewedFixture(t *testing.T) (ExecutionSpec, ReviewedInputs) {
	t.Helper()
	s := executionSpecFixture(t)
	s.VarFiles = []string{"vars.tfvars"}
	s.BackendFiles = []string{"backend.hcl"}
	remote := ID()
	s.Grants = append(s.Grants, ResourceRef{ID: ID(), Kind: "repository", ResourceID: remote, Revision: 1})
	b := s.Backend
	r := ReviewedInputs{Version: 1, Stack: ID(), StackRevision: s.StackRevisionID, Subject: s.Source.SubjectID, Facts: s.Source.FactsRevision, Commit: s.Source.Commit,
		Engine:  ManifestEngine{s.Engine.Name, s.Engine.Version, s.Engine.Arch, s.Engine.ImageDigest, "1:2.47.3-0+deb13u1"},
		Backend: ManifestBackend{Partition: b.Target.Partition, Bucket: b.Target.Bucket, Key: b.Target.Key, Region: b.Region, Owner: b.ExpectedOwner, Workspace: s.Workspace, Profile: b.Profile, Table: b.TableARN, AttributesDigest: Digest(b.Attributes)},
		AWS:     ManifestAWS{s.AWS.ID, s.AWS.Revision, s.AWS.RoleARN}, Pool: s.Pool, Secrets: s.Secrets, Grants: s.Grants, Credentials: s.SourceCredentials,
		Approval: ApprovalPolicy{Floor: 1, Requested: 1, Minimum: 1, Revision: s.Review.Revision}, Rules: s.RuleVersion, Profile: s.ProfileID, Provenance: Provenance{Kind: "normal"}, Epoch: 3}
	for _, p := range []string{"main.tf", "vars.tfvars", "backend.hcl", "modules/local/main.tf"} {
		r.Files = append(r.Files, ManifestFile{Repository: s.Source.RepositoryID, Commit: s.Source.Commit, Path: p, Kind: "regular", Size: 12, Hash: Digest(p)})
	}
	r.Files = append(r.Files, ManifestFile{Repository: remote, Commit: strings.Repeat("b", 40), Path: "main.tf", Kind: "regular", Hash: Digest("remote")})
	for _, p := range []string{"alias.tf", "nested-alias.tf"} {
		target := "main.tf"
		if p == "nested-alias.tf" {
			target = "alias.tf"
		}
		r.Files = append(r.Files, ManifestFile{Repository: s.Source.RepositoryID, Commit: s.Source.Commit, Path: p, Kind: "symlink", Symlink: target, Hash: Digest(target)})
	}
	sortManifestFiles(r.Files)
	r.Modules = []ManifestModule{{s.Source.RepositoryID, s.Source.Commit, "main.tf", "local", s.Source.RepositoryID, s.Source.Commit, "modules/local", Digest("./modules/local")}, {s.Source.RepositoryID, s.Source.Commit, "remote.tf", "git", remote, strings.Repeat("b", 40), ".", Digest("remote literal")}}
	r.VarFiles = []ManifestInputFile{{"vars.tfvars", 12, Digest("vars.tfvars")}}
	r.BackendFiles = []ManifestInputFile{{"backend.hcl", 12, Digest("backend.hcl")}}
	if e := r.Validate(s, r.Stack, 3); e != nil {
		t.Fatal(e)
	}
	return s, r
}
func sortManifestFiles(files []ManifestFile) {
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		return a.Repository+"/"+a.Commit+"/"+a.Path < b.Repository+"/"+b.Commit+"/"+b.Path
	})
}
func copyReviewed(t *testing.T, r ReviewedInputs) ReviewedInputs {
	t.Helper()
	b, e := json.Marshal(r)
	if e != nil {
		t.Fatal(e)
	}
	var out ReviewedInputs
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func requireRunCode(t *testing.T, e error, code string) {
	t.Helper()
	if e == nil || e.Error() != code {
		t.Fatalf("got %v, want %s", e, code)
	}
}

func TestReviewedInputsRepositoryRelativePaths(t *testing.T) {
	s, r := reviewedFixture(t)
	s.Source.Root = "infra"
	for _, p := range []string{"backend.hcl", "vars.tfvars"} {
		r.Files = append(r.Files, ManifestFile{Repository: s.Source.RepositoryID, Commit: s.Source.Commit, Path: "infra/" + p, Kind: "regular", Size: 21, Hash: Digest("different nested " + p)})
	}
	sortManifestFiles(r.Files)
	if e := r.Validate(s, r.Stack, r.Epoch); e != nil {
		t.Fatal("repository-relative inputs rejected for nested stack", e)
	}
	for _, backend := range []bool{true, false} {
		bad := copyReviewed(t, r)
		input := &bad.VarFiles[0]
		if backend {
			input = &bad.BackendFiles[0]
		}
		input.Size, input.Hash = 21, Digest("different nested "+input.Path)
		if bad.Validate(s, r.Stack, r.Epoch) == nil {
			t.Fatal("nested shadow input accepted as declared repository-relative input")
		}
	}
}

func TestReviewedClosureRejectsUnpinnedOrAmbiguousFiles(t *testing.T) {
	s, r := reviewedFixture(t)
	cases := map[string]func(*ReviewedInputs){
		"missing files":           func(r *ReviewedInputs) { r.Files = nil },
		"missing modules":         func(r *ReviewedInputs) { r.Modules = nil },
		"missing declared inputs": func(r *ReviewedInputs) { r.VarFiles = nil },
		"unordered files":         func(r *ReviewedInputs) { r.Files[0], r.Files[1] = r.Files[1], r.Files[0] },
		"duplicate file":          func(r *ReviewedInputs) { r.Files = append(r.Files, r.Files[0]); sortManifestFiles(r.Files) },
		"unassigned repository":   func(r *ReviewedInputs) { r.Files[0].Repository = ID(); sortManifestFiles(r.Files) },
		"unassigned commit":       func(r *ReviewedInputs) { r.Files[0].Commit = strings.Repeat("c", 40); sortManifestFiles(r.Files) },
		"absolute path":           func(r *ReviewedInputs) { r.Files[0].Path = "/etc/passwd" },
		"negative size":           func(r *ReviewedInputs) { r.Files[0].Size = -1 },
		"oversized file":          func(r *ReviewedInputs) { r.Files[0].Size = 67108865 },
		"invalid hash":            func(r *ReviewedInputs) { r.Files[0].Hash = "short" },
		"generated plan":          func(r *ReviewedInputs) { r.Files[0].Path = "plan.bin"; sortManifestFiles(r.Files) },
		"nested generated plan":   func(r *ReviewedInputs) { r.Files[0].Path = "sub/plan.bin"; sortManifestFiles(r.Files) },
		"regular file with symlink target": func(r *ReviewedInputs) {
			for i := range r.Files {
				if r.Files[i].Kind == "regular" {
					r.Files[i].Symlink = "main.tf"
					break
				}
			}
		},
		"symlink escapes checkout": func(r *ReviewedInputs) {
			for i := range r.Files {
				if r.Files[i].Kind == "symlink" {
					r.Files[i].Symlink = "../secret"
					break
				}
			}
		},
		"absolute symlink": func(r *ReviewedInputs) {
			for i := range r.Files {
				if r.Files[i].Kind == "symlink" {
					r.Files[i].Symlink = "/secret"
					break
				}
			}
		},
		"dangling symlink": func(r *ReviewedInputs) {
			for i := range r.Files {
				if r.Files[i].Kind == "symlink" {
					r.Files[i].Symlink = "missing.tf"
					break
				}
			}
		},
		"symlink cycle": func(r *ReviewedInputs) {
			for i := range r.Files {
				if r.Files[i].Path == "alias.tf" {
					r.Files[i].Symlink = "nested-alias.tf"
				}
			}
		},
		"module lacks grant":              func(r *ReviewedInputs) { r.Modules[1].CallerRepo = ID() },
		"local module changes repository": func(r *ReviewedInputs) { r.Modules[0].TargetRepo = r.Modules[1].TargetRepo },
		"unpinned module":                 func(r *ReviewedInputs) { r.Modules[1].TargetCommit = "main" },
		"registry module":                 func(r *ReviewedInputs) { r.Modules[0].SourceKind = "registry" },
		"duplicate module":                func(r *ReviewedInputs) { r.Modules = append(r.Modules, r.Modules[1]) },
		"reordered inputs":                func(r *ReviewedInputs) { r.VarFiles[0].Path = "main.tf" },
		"changed input size":              func(r *ReviewedInputs) { r.BackendFiles[0].Size++ },
		"changed input bytes":             func(r *ReviewedInputs) { r.VarFiles[0].Hash = Digest("different") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			v := copyReviewed(t, r)
			change(&v)
			requireRunCode(t, v.Validate(s, r.Stack, 3), "INVALID_MANIFEST")
		})
	}
	for name, change := range map[string]func(*ReviewedInputs){"state epoch": func(r *ReviewedInputs) { r.Epoch++ }, "source facts": func(r *ReviewedInputs) { r.Facts++ }, "approval floor": func(r *ReviewedInputs) { r.Approval.Floor = 2 }, "recovery provenance": func(r *ReviewedInputs) { r.Provenance.Case = ID() }, "backend": func(r *ReviewedInputs) { r.Backend.Key = "other" }} {
		t.Run(name, func(t *testing.T) {
			v := copyReviewed(t, r)
			change(&v)
			requireRunCode(t, v.Validate(s, r.Stack, 3), "REPREPARE_REQUIRED")
		})
	}
}

func TestPreparationBindsExactTrackedAndRetainedLocks(t *testing.T) {
	s, r := reviewedFixture(t)
	p := PreInitSpec{ReviewedInputs: r, Kind: "plan", SpecDigest: Digest(s), LockOrigin: "absent"}
	if e := p.Validate(s, "plan", r.Stack, 3); e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*PreInitSpec){func(p *PreInitSpec) { p.SpecDigest = Digest("other") }, func(p *PreInitSpec) { p.LockOrigin = "retained"; p.LockHash = Digest("lock") }, func(p *PreInitSpec) { p.RetainedDigest = Digest("plan") }, func(p *PreInitSpec) { p.LockHash = Digest("unexpected") }, func(p *PreInitSpec) { p.LockOrigin = "tracked"; p.LockHash = Digest("missing") }} {
		v := p
		change(&v)
		requireRunCode(t, v.Validate(s, "plan", r.Stack, 3), "INVALID_MANIFEST")
	}
	lock := ManifestFile{Repository: s.Source.RepositoryID, Commit: s.Source.Commit, Path: ".terraform.lock.hcl", Kind: "regular", Hash: Digest("lock"), Size: 15}
	p.Files = append(append([]ManifestFile{}, r.Files...), lock)
	sortManifestFiles(p.Files)
	requireRunCode(t, p.Validate(s, "plan", r.Stack, 3), "INVALID_MANIFEST")
	p.LockOrigin = "tracked"
	p.LockHash = lock.Hash
	if e := p.Validate(s, "plan", r.Stack, 3); e != nil {
		t.Fatal(e)
	}
	p.LockHash = Digest("changed")
	requireRunCode(t, p.Validate(s, "plan", r.Stack, 3), "INVALID_MANIFEST")
	p.Files = r.Files
	p.Kind = "apply"
	p.LockOrigin = "retained"
	p.LockHash = lock.Hash
	requireRunCode(t, p.Validate(s, "apply", r.Stack, 3), "REPREPARE_REQUIRED")
	s.RetainedPlan = &RetainedPlan{ManifestDigest: Digest("retained")}
	p.SpecDigest = Digest(s)
	p.RetainedDigest = s.RetainedPlan.ManifestDigest
	if e := p.Validate(s, "apply", r.Stack, 3); e != nil {
		t.Fatal(e)
	}
	p.LockOrigin = "tracked"
	requireRunCode(t, p.Validate(s, "apply", r.Stack, 3), "REPREPARE_REQUIRED")
}

func TestProviderManifestRejectsMissingLocksAndUnstableSelections(t *testing.T) {
	good := Manifest{Lock: ProviderLock{Present: true, Hash: Digest("lock"), Size: 50}, Providers: []Provider{{Address: "registry.opentofu.org/hashicorp/null", Version: "3.2.4", Platform: "linux_amd64", Hashes: []string{"h1:fixture", "zh:fixture"}}}}
	if e := good.ValidateLock(); e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(*Manifest){"missing collection": func(m *Manifest) { m.Providers = nil }, "lock missing": func(m *Manifest) { m.Lock.Present = false }, "bad hash": func(m *Manifest) { m.Lock.Hash = "invalid" }, "negative size": func(m *Manifest) { m.Lock.Size = -1 }, "oversized": func(m *Manifest) { m.Lock.Size = 67108865 }} {
		t.Run(name, func(t *testing.T) {
			m := good
			change(&m)
			requireRunCode(t, m.ValidateLock(), "PROVIDER_LOCK_REQUIRED")
		})
	}
	for name, change := range map[string]func(*Provider){"untrusted registry": func(p *Provider) { p.Address = "private.example/hashicorp/null" }, "wrong platform": func(p *Provider) { p.Platform = "darwin_arm64" }, "missing version": func(p *Provider) { p.Version = "" }, "missing checksums": func(p *Provider) { p.Hashes = nil }, "unordered checksums": func(p *Provider) { p.Hashes = []string{"zh:fixture", "h1:fixture"} }} {
		t.Run(name, func(t *testing.T) {
			m := good
			m.Providers = append([]Provider{}, good.Providers...)
			change(&m.Providers[0])
			requireRunCode(t, m.ValidateLock(), "PROVIDER_LOCK_INVALID")
		})
	}
	good.Providers = append(good.Providers, good.Providers[0])
	requireRunCode(t, good.ValidateLock(), "PROVIDER_LOCK_INVALID")
}
