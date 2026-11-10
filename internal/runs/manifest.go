package runs

import (
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"path"
	"sort"
	"strings"
)

type ManifestEngine struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Image   string `json:"image_digest"`
	Git     string `json:"git_package_version"`
}
type ManifestBackend struct {
	Partition        string `json:"partition"`
	Bucket           string `json:"bucket"`
	Key              string `json:"effective_key"`
	Region           string `json:"region"`
	Owner            string `json:"expected_owner"`
	Workspace        string `json:"workspace"`
	Profile          string `json:"lock_profile"`
	Table            string `json:"table_arn,omitempty"`
	KMS              string `json:"kms_key_arn,omitempty"`
	AttributesDigest string `json:"attributes_digest"`
}
type ManifestAWS struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	Role     string `json:"role_arn"`
}
type ManifestFile struct {
	Repository string `json:"repository_id"`
	Commit     string `json:"commit"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Executable bool   `json:"executable"`
	Size       int64  `json:"size"`
	Hash       string `json:"sha256"`
	Symlink    string `json:"symlink_target,omitempty"`
}
type ManifestModule struct {
	CallerRepo    string `json:"caller_repo"`
	CallerCommit  string `json:"caller_commit"`
	CallerPath    string `json:"caller_path"`
	SourceKind    string `json:"source_kind"`
	TargetRepo    string `json:"target_repo"`
	TargetCommit  string `json:"target_commit"`
	TargetDir     string `json:"target_subdir"`
	LiteralDigest string `json:"source_literal_digest"`
}
type ManifestInputFile struct {
	Path string `json:"repo_path"`
	Size int64  `json:"size"`
	Hash string `json:"sha256"`
}
type ApprovalPolicy struct {
	Floor     int   `json:"admin_floor"`
	Requested int   `json:"requested_minimum"`
	Minimum   int   `json:"effective_minimum"`
	Revision  int64 `json:"revision"`
}
type Provenance struct {
	Kind     string `json:"kind"`
	Case     string `json:"case_id,omitempty"`
	Revision int64  `json:"evidence_revision,omitempty"`
}
type ReviewedInputs struct {
	Version       int                 `json:"version"`
	Stack         string              `json:"stack_id"`
	StackRevision string              `json:"stack_revision_id"`
	Subject       string              `json:"source_subject_id"`
	Facts         int64               `json:"source_facts_revision"`
	Commit        string              `json:"source_commit"`
	Engine        ManifestEngine      `json:"engine"`
	Backend       ManifestBackend     `json:"backend"`
	AWS           ManifestAWS         `json:"aws_connection"`
	Pool          Pool                `json:"pool"`
	Files         []ManifestFile      `json:"files"`
	Modules       []ManifestModule    `json:"modules"`
	VarFiles      []ManifestInputFile `json:"var_files"`
	BackendFiles  []ManifestInputFile `json:"backend_config_files"`
	Secrets       []SecretRef         `json:"secret_refs"`
	Grants        []ResourceRef       `json:"resource_grant_refs"`
	Approval      ApprovalPolicy      `json:"approval_policy"`
	AllowPR       bool                `json:"allow_pr_apply"`
	Credentials   []SourceCredential  `json:"source_credential_refs"`
	Rules         string              `json:"rule_version"`
	Profile       string              `json:"profile_id"`
	Epoch         int64               `json:"state_epoch"`
	Provenance    Provenance          `json:"provenance"`
}
type PreInitSpec struct {
	ReviewedInputs
	Kind           string `json:"operation_kind"`
	SpecDigest     string `json:"execution_spec_digest"`
	LockOrigin     string `json:"input_lock_origin"`
	LockHash       string `json:"input_lock_sha256,omitempty"`
	RetainedDigest string `json:"retained_manifest_digest,omitempty"`
}
type ProviderLock struct {
	Present bool   `json:"present"`
	Hash    string `json:"sha256,omitempty"`
	Size    int64  `json:"size"`
}
type Provider struct {
	Address  string   `json:"source_address"`
	Version  string   `json:"version"`
	Platform string   `json:"platform"`
	Hashes   []string `json:"package_hashes"`
}
type Manifest struct {
	ReviewedInputs
	Lock      ProviderLock `json:"provider_lock"`
	Providers []Provider   `json:"providers"`
}

func (r ReviewedInputs) Validate(s ExecutionSpec, stack string, epoch int64) error {
	b := s.Backend
	kms, _ := b.Attributes["kms_key_id"].(string)
	expectedBackend := ManifestBackend{b.Target.Partition, b.Target.Bucket, b.Target.Key, b.Region, b.ExpectedOwner, s.Workspace, b.Profile, b.TableARN, kms, Digest(b.Attributes)}
	if r.Version != 1 || r.Stack != stack || r.StackRevision != s.StackRevisionID || r.Subject != s.Source.SubjectID || r.Facts != s.Source.FactsRevision || r.Commit != s.Source.Commit || r.Engine != (ManifestEngine{s.Engine.Name, s.Engine.Version, s.Engine.Arch, s.Engine.ImageDigest, "1:2.47.3-0+deb13u1"}) || r.Backend != expectedBackend || r.AWS != (ManifestAWS{s.AWS.ID, s.AWS.Revision, s.AWS.RoleARN}) || Digest(r.Pool) != Digest(s.Pool) || Digest(r.Secrets) != Digest(s.Secrets) || Digest(r.Grants) != Digest(s.Grants) || Digest(r.Credentials) != Digest(s.SourceCredentials) || r.Approval.Minimum != s.Review.Minimum || r.Approval.Revision != s.Review.Revision || r.Approval.Floor < 1 || r.Approval.Requested < 1 || r.Approval.Minimum != max(r.Approval.Floor, r.Approval.Requested) || r.AllowPR != s.Review.AllowPRApply || r.Rules != s.RuleVersion || r.Profile != s.ProfileID || r.Epoch != epoch || Digest(r.Provenance) != Digest(s.Provenance()) {
		return Fail("REPREPARE_REQUIRED")
	}
	if r.Files == nil || len(r.Files) == 0 || len(r.Files) > 10000 || r.Modules == nil || len(r.Modules) > 1024 || r.VarFiles == nil || r.BackendFiles == nil {
		return Fail("INVALID_MANIFEST")
	}
	previous := ""
	files := map[string]ManifestFile{}
	for _, f := range r.Files {
		key := f.Repository + "/" + f.Commit + "/" + f.Path
		if !catalog.UUID(f.Repository) || !commit40.MatchString(f.Commit) || !catalog.RepoPath(f.Path) || f.Path == "." || !ValidDigest(f.Hash) || f.Size < 0 || f.Size > 67108864 || key <= previous || f.Kind != "regular" && f.Kind != "symlink" {
			return Fail("INVALID_MANIFEST")
		}
		// All files belong to the assigned root or an explicitly pinned module.
		authorized := f.Repository == s.Source.RepositoryID && f.Commit == s.Source.Commit
		for _, m := range r.Modules {
			authorized = authorized || f.Repository == m.TargetRepo && f.Commit == m.TargetCommit
		}
		if !authorized {
			return Fail("INVALID_MANIFEST")
		}
		if f.Kind == "symlink" {
			target := path.Clean(path.Join(path.Dir(f.Path), f.Symlink))
			if f.Symlink == "" || strings.HasPrefix(f.Symlink, "/") || target == ".." || strings.HasPrefix(target, "../") {
				return Fail("INVALID_MANIFEST")
			}
		} else if f.Symlink != "" {
			return Fail("INVALID_MANIFEST")
		}
		if strings.HasSuffix(f.Path, "/plan.bin") || f.Path == "plan.bin" {
			return Fail("INVALID_MANIFEST")
		}
		files[key] = f
		previous = key
	}
	for _, f := range r.Files {
		if f.Kind == "symlink" {
			seen := map[string]bool{}
			for f.Kind == "symlink" {
				key := f.Repository + "/" + f.Commit + "/" + path.Clean(path.Join(path.Dir(f.Path), f.Symlink))
				if seen[key] {
					return Fail("INVALID_MANIFEST")
				}
				seen[key] = true
				var ok bool
				f, ok = files[key]
				if !ok {
					return Fail("INVALID_MANIFEST")
				}
			}
		}
	}
	previous = ""
	repositoryAllowed := func(id string) bool {
		if id == s.Source.RepositoryID {
			return true
		}
		for _, g := range s.Grants {
			if g.Kind == "repository" && g.ResourceID == id {
				return true
			}
		}
		return false
	}
	for _, m := range r.Modules {
		if !repositoryAllowed(m.CallerRepo) || !repositoryAllowed(m.TargetRepo) || m.SourceKind == "local" && (m.CallerRepo != m.TargetRepo || m.CallerCommit != m.TargetCommit) {
			return Fail("INVALID_MANIFEST")
		}
		if !catalog.UUID(m.CallerRepo) || !catalog.UUID(m.TargetRepo) || !commit40.MatchString(m.CallerCommit) || !commit40.MatchString(m.TargetCommit) || !catalog.RepoPath(m.CallerPath) || !catalog.RepoPath(m.TargetDir) || !ValidDigest(m.LiteralDigest) || (m.SourceKind != "local" && m.SourceKind != "git") || m.CallerPath <= previous {
			return Fail("INVALID_MANIFEST")
		}
		previous = m.CallerPath
	}
	for _, pair := range []struct {
		declared []string
		actual   []ManifestInputFile
	}{{s.VarFiles, r.VarFiles}, {s.BackendFiles, r.BackendFiles}} {
		if len(pair.declared) != len(pair.actual) {
			return Fail("INVALID_MANIFEST")
		}
		for i, p := range pair.actual {
			f, ok := files[s.Source.RepositoryID+"/"+s.Source.Commit+"/"+p.Path]
			if p.Path != pair.declared[i] || !ok || f.Size != p.Size || f.Hash != p.Hash {
				return Fail("INVALID_MANIFEST")
			}
		}
	}
	return nil
}
func (p PreInitSpec) Validate(s ExecutionSpec, kind, stack string, epoch int64) error {
	if e := p.ReviewedInputs.Validate(s, stack, epoch); e != nil {
		return e
	}
	if p.Kind != kind || p.SpecDigest != Digest(s) || (p.LockOrigin != "tracked" && p.LockOrigin != "retained" && p.LockOrigin != "absent") || p.LockOrigin == "absent" && p.LockHash != "" || p.LockOrigin != "absent" && !ValidDigest(p.LockHash) {
		return Fail("INVALID_MANIFEST")
	}
	var tracked *ManifestFile
	for i, f := range p.Files {
		if f.Repository == s.Source.RepositoryID && f.Commit == s.Source.Commit && f.Path == path.Join(s.Source.Root, ".terraform.lock.hcl") {
			tracked = &p.Files[i]
		}
	}
	if kind == "plan" {
		if p.RetainedDigest != "" || p.LockOrigin == "retained" || p.LockOrigin == "tracked" && (tracked == nil || tracked.Hash != p.LockHash) || p.LockOrigin == "absent" && tracked != nil {
			return Fail("INVALID_MANIFEST")
		}
	} else if s.RetainedPlan == nil || p.RetainedDigest != s.RetainedPlan.ManifestDigest || p.LockOrigin == "tracked" {
		return Fail("REPREPARE_REQUIRED")
	}
	return nil
}
func (m Manifest) ValidateLock() error {
	if m.Providers == nil || len(m.Providers) > 256 || m.Lock.Size < 0 || m.Lock.Size > 67108864 || m.Lock.Present && !ValidDigest(m.Lock.Hash) || !m.Lock.Present && (m.Lock.Hash != "" || m.Lock.Size != 0 || len(m.Providers) > 0) {
		return Fail("PROVIDER_LOCK_REQUIRED")
	}
	previous := ""
	for _, p := range m.Providers {
		parts := strings.Split(p.Address, "/")
		if len(parts) != 3 || (parts[0] != "registry.terraform.io" && parts[0] != "registry.opentofu.org") || p.Address <= previous || p.Platform != "linux_amd64" || p.Version == "" || len(p.Hashes) == 0 || !sort.StringsAreSorted(p.Hashes) {
			return Fail("PROVIDER_LOCK_INVALID")
		}
		previous = p.Address
	}
	return nil
}
