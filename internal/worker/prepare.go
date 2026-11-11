package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

type checkoutTree struct{ ID, Commit, Dir, URL string }

func snapshotTree(tree checkoutTree, skipLock string) ([]runs.ManifestFile, error) {
	out := []runs.ManifestFile{}
	total := int64(0)
	e := filepath.WalkDir(tree.Dir, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return ErrAgent
		}
		if p == tree.Dir {
			return nil
		}
		rel, e := filepath.Rel(tree.Dir, p)
		if e != nil {
			return ErrAgent
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			return filepath.SkipDir
		}
		for _, part := range strings.Split(rel, "/") {
			if part == ".git" || part == ".terraform" || part == "plan.bin" || part == "terraform.tfstate" || part == ".terraform.tfstate.lock.info" {
				return ErrAgent
			}
		}
		if rel == skipLock {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, e := os.Lstat(p)
		if e != nil {
			return ErrAgent
		}
		f := runs.ManifestFile{Repository: tree.ID, Commit: tree.Commit, Path: rel, Executable: st.Mode().Perm()&0111 != 0, Kind: "regular"}
		var b []byte
		if st.Mode()&os.ModeSymlink != 0 {
			target, e := os.Readlink(p)
			if e != nil {
				return ErrAgent
			}
			f.Kind = "symlink"
			f.Executable = false
			f.Symlink = target
			b = []byte(target)
			real, e := filepath.EvalSymlinks(p)
			if e != nil {
				return ErrAgent
			}
			within, e := filepath.Rel(tree.Dir, real)
			if e != nil || within == ".." || strings.HasPrefix(within, "../") {
				return ErrAgent
			}
		} else {
			if !st.Mode().IsRegular() {
				return ErrAgent
			}
			b, e = protectedBytes(p, 67108864)
			if e != nil {
				return e
			}
		}
		if bytes.HasPrefix(b, []byte("version https://git-lfs.github.com/spec/v1\n")) {
			return ErrAgent
		}
		f.Size = int64(len(b))
		total += f.Size
		if total > 67108864 || len(out) >= 10000 {
			return ErrAgent
		}
		sum := sha256.Sum256(b)
		f.Hash = hex.EncodeToString(sum[:])
		out = append(out, f)
		return nil
	})
	return out, e
}
func reviewedInputs(assignment runs.Assignment, trees map[string]checkoutTree, inspection source.Inspection) (runs.ReviewedInputs, error) {
	s := assignment.Execution
	if s == nil {
		return runs.ReviewedInputs{}, ErrAgent
	}
	b := s.Backend
	out := runs.ReviewedInputs{Version: 1, Stack: assignment.StackID, StackRevision: s.StackRevisionID, Subject: s.Source.SubjectID, Facts: s.Source.FactsRevision, Commit: s.Source.Commit, Engine: runs.ManifestEngine{Name: s.Engine.Name, Version: s.Engine.Version, Arch: s.Engine.Arch, Image: s.Engine.ImageDigest, Git: "1:2.47.3-0+deb13u1"}, Backend: runs.ManifestBackend{Partition: b.Target.Partition, Bucket: b.Target.Bucket, Key: b.Target.Key, Region: b.Region, Owner: b.ExpectedOwner, Workspace: s.Workspace, Profile: b.Profile, Table: b.TableARN, AttributesDigest: runs.Digest(b.Attributes)}, AWS: runs.ManifestAWS{ID: s.AWS.ID, Revision: s.AWS.Revision, Role: s.AWS.RoleARN}, Pool: s.Pool, Files: []runs.ManifestFile{}, Modules: []runs.ManifestModule{}, VarFiles: []runs.ManifestInputFile{}, BackendFiles: []runs.ManifestInputFile{}, Secrets: s.Secrets, Grants: s.Grants, Approval: runs.ApprovalPolicy{Floor: assignment.ApprovalFloor, Requested: s.Review.Minimum, Minimum: s.Review.Minimum, Revision: s.Review.Revision}, AllowPR: s.Review.AllowPRApply, Credentials: s.SourceCredentials, Rules: s.RuleVersion, Profile: s.ProfileID, Epoch: assignment.StateEpoch, Provenance: s.Provenance()}
	for _, tree := range trees {
		files, e := snapshotTree(tree, "")
		if e != nil {
			return out, e
		}
		out.Files = append(out.Files, files...)
	}
	sort.Slice(out.Files, func(i, j int) bool {
		a, b := out.Files[i], out.Files[j]
		return a.Repository+"/"+a.Commit+"/"+a.Path < b.Repository+"/"+b.Commit+"/"+b.Path
	})

	for _, m := range inspection.Modules {
		var caller checkoutTree
		for _, tree := range trees {
			if tree.Dir == m.Checkout {
				caller = tree
				break
			}
		}
		if caller.ID == "" {
			return out, ErrAgent
		}
		callerPath := path.Join(m.CallerDirectory, path.Base(m.Path))
		edge := runs.ManifestModule{CallerRepo: caller.ID, CallerCommit: caller.Commit, CallerPath: m.Path, LiteralDigest: hash([]byte(m.Source))}
		if remote, e := source.ParseModule(m.Source); e == nil {
			target := trees[remote.Repository+"@"+remote.Commit]
			edge.SourceKind = "git"
			edge.TargetRepo = target.ID
			edge.TargetCommit = remote.Commit
			edge.TargetDir = remote.Subdirectory
		} else {
			edge.SourceKind = "local"
			edge.TargetRepo = caller.ID
			edge.TargetCommit = caller.Commit
			edge.TargetDir = path.Clean(path.Join(path.Dir(callerPath), m.Source))
		}
		out.Modules = append(out.Modules, edge)
	}
	for _, pair := range []struct {
		paths []string
		dest  *[]runs.ManifestInputFile
	}{{s.VarFiles, &out.VarFiles}, {s.BackendFiles, &out.BackendFiles}} {
		for _, p := range pair.paths {
			found := false
			for _, f := range out.Files {
				if f.Repository == s.Source.RepositoryID && f.Commit == s.Source.Commit && f.Path == p {
					*pair.dest = append(*pair.dest, runs.ManifestInputFile{Path: p, Size: f.Size, Hash: f.Hash})
					found = true
					break
				}
			}
			if !found {
				return out, ErrAgent
			}
		}
	}
	return out, out.Validate(*s, assignment.StackID, assignment.StateEpoch)
}
func verifyTrees(before runs.ReviewedInputs, trees map[string]checkoutTree, generatedLock, rootID string) error {
	files := []runs.ManifestFile{}
	for _, tree := range trees {
		skip := ""
		if tree.Commit == before.Commit && tree.ID == rootID { // only the assigned root lock may be generated
			for _, f := range before.Files {
				if f.Repository == tree.ID && f.Commit == tree.Commit && f.Path == generatedLock {
					skip = ""
					goto tracked
				}
			}
			skip = generatedLock
		}
	tracked:
		current, e := snapshotTree(tree, skip)
		if e != nil {
			return e
		}
		files = append(files, current...)
	}
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		return a.Repository+"/"+a.Commit+"/"+a.Path < b.Repository+"/"+b.Commit+"/"+b.Path
	})
	if runs.Digest(files) != runs.Digest(before.Files) {
		return ErrAgent
	}
	return nil
}
func canonicalArtifact(v any) ([]byte, error) { return catalog.Canonical(v) }

// Validate native-installed module copies against the preflight's complete
// pinned closure; init cannot introduce an unreviewed source or mutate inputs.
func verifyInstalledModules(dir, root string, trees map[string]checkoutTree, inspection source.Inspection) error {
	raw, e := protectedBytes(filepath.Join(dir, "data", "modules", "modules.json"), 1048576)
	if e != nil {
		if len(inspection.Modules) == 0 && os.IsNotExist(fileError(filepath.Join(dir, "data", "modules", "modules.json"))) {
			return nil
		}
		return ErrAgent
	}
	var installed struct {
		Modules []struct{ Key, Source, Version, Dir string }
	}
	if json.Unmarshal(raw, &installed) != nil {
		return ErrAgent
	}
	allowed := map[string]bool{}
	for _, m := range inspection.Modules {
		allowed[m.Source] = true
	}
	seen := map[string]bool{}
	for _, m := range installed.Modules {
		if m.Key == "" {
			continue
		}
		if !allowed[m.Source] || m.Version != "" {
			return ErrAgent
		}
		seen[m.Source] = true
		if remote, e := source.ParseModule(m.Source); e == nil {
			expected, ok := trees[remote.Repository+"@"+remote.Commit]
			if !ok {
				return ErrAgent
			}
			actual := m.Dir
			if !filepath.IsAbs(actual) {
				actual = filepath.Join(root, actual)
			}
			if remote.Subdirectory != "." {
				suffix := filepath.FromSlash(remote.Subdirectory)
				if !strings.HasSuffix(actual, string(filepath.Separator)+suffix) {
					return ErrAgent
				}
				actual = strings.TrimSuffix(actual, string(filepath.Separator)+suffix)
			}
			actual, e = filepath.EvalSymlinks(actual)
			if e != nil {
				return ErrAgent
			}
			base, baseErr := filepath.EvalSymlinks(filepath.Join(dir, "data", "modules"))
			if baseErr != nil {
				return ErrAgent
			}
			rel, e := filepath.Rel(base, actual)
			if e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return ErrAgent
			}
			before, e := snapshotTree(expected, "")
			if e != nil {
				return e
			}
			copy := expected
			copy.Dir = actual
			after, e := snapshotTree(copy, "")
			if e != nil || runs.Digest(before) != runs.Digest(after) {
				return ErrAgent
			}
		}
	}
	for src := range allowed {
		if !seen[src] {
			return ErrAgent
		}
	}
	return nil
}
