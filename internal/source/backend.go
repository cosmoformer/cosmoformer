// Package source inspects exact source trees without initializing native state.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var ErrSource = errors.New("source is unsupported, incomplete or outside its authorized bounds")

type Target struct {
	Partition string `json:"partition"`
	Bucket    string `json:"bucket"`
	Key       string `json:"effective_key"`
}
type Claim struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Key       string `json:"resource_key"`
	Purpose   string `json:"purpose"`
}
type Backend struct {
	Target        Target         `json:"canonical_target"`
	Profile       string         `json:"profile"`
	Region        string         `json:"region"`
	ExpectedOwner string         `json:"expected_owner"`
	TableARN      string         `json:"table_arn,omitempty"`
	Attributes    map[string]any `json:"attributes"`
	Claims        []Claim        `json:"derived_claims"`
}
type File struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	SHA256 string `json:"sha256"`
}
type Module struct {
	// Local inspection provenance never enters wire metadata or digests.
	Checkout        string `json:"-"`
	CallerDirectory string `json:"-"`
	Path            string `json:"path"`
	Source          string `json:"source"`
}
type Inspection struct {
	Backend Backend  `json:"backend"`
	Files   []File   `json:"files"`
	Modules []Module `json:"module_graph"`
	Digest  string   `json:"source_digest"`
}

// Read confines all paths, including each symlink component. O_NOFOLLOW alone
// would not protect parent components. Checkout trees are private and immutable.
func Read(root, p string) ([]byte, error) {
	if !catalog.RepoPath(p) {
		return nil, ErrSource
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return nil, ErrSource
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return nil, ErrSource
	}
	real, e := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(p)))
	if e != nil {
		return nil, ErrSource
	}
	rel, e := filepath.Rel(root, real)
	if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !catalog.RepoPath(filepath.ToSlash(rel)) {
		return nil, ErrSource
	}
	before, e := os.Stat(real)
	if e != nil || !before.Mode().IsRegular() || before.Size() > 1048576 {
		return nil, ErrSource
	}
	f, e := os.Open(real)
	if e != nil {
		return nil, ErrSource
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 1048576 {
		return nil, ErrSource
	}
	data, e := io.ReadAll(io.LimitReader(f, 1048577))
	if e != nil || len(data) > 1048576 {
		return nil, ErrSource
	}
	return data, nil
}
func parsed(b []byte, name string) (*hcl.File, error) {
	p := hclparse.NewParser()
	var f *hcl.File
	var d hcl.Diagnostics
	if strings.HasSuffix(name, ".json") {
		f, d = p.ParseJSON(b, name)
	} else {
		f, d = p.ParseHCL(b, name)
	}
	if d.HasErrors() {
		return nil, ErrSource
	}
	return f, nil
}
func literal(e hcl.Expression) (cty.Value, error) {
	// Value(nil) allows constant arithmetic in native HCL; the pilot accepts only
	// literal values, with JSON scalar expressions handled by the JSON parser.
	switch x := e.(type) {
	case *hclsyntax.LiteralValueExpr:
		return x.Val, nil
	case *hclsyntax.TemplateExpr:
		if !x.IsStringLiteral() {
			return cty.NilVal, ErrSource
		}
	case *hclsyntax.ObjectConsExpr, *hclsyntax.TupleConsExpr, *hclsyntax.BinaryOpExpr, *hclsyntax.FunctionCallExpr, *hclsyntax.ScopeTraversalExpr:
		return cty.NilVal, ErrSource
	}
	v, d := e.Value(nil)
	if d.HasErrors() || !v.IsKnown() || v.IsNull() || (v.Type() != cty.String && v.Type() != cty.Bool && v.Type() != cty.Number) {
		return cty.NilVal, ErrSource
	}
	return v, nil
}
func attrs(body hcl.Body) (map[string]any, error) {
	a, d := body.JustAttributes()
	if d.HasErrors() {
		return nil, ErrSource
	}
	out := map[string]any{}
	for k, v := range a {
		x, e := literal(v.Expr)
		if e != nil {
			return nil, e
		}
		switch x.Type() {
		case cty.String:
			out[k] = x.AsString()
		case cty.Bool:
			out[k] = x.True()
		default:
			return nil, ErrSource
		}
	}
	return out, nil
}

// BackendContext separates platform metadata from native backend literals.
type BackendContext struct {
	Engine        catalog.Engine
	Workspace     string
	ExpectedOwner string
}

// OwnerSessionPolicy generates the policy the native AWS adapter must attach to
// the worker's assumed-role session. The Allow preserves the role's existing upper
// bound; it cannot grant permissions absent from that role. The explicit Deny
// protects every request to this backend bucket, including native CLI requests
// that do not support the ExpectedBucketOwner request header.
func (b Backend) OwnerSessionPolicy() ([]byte, error) {
	if !validBucket(b.Target.Bucket) || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(b.ExpectedOwner) || (b.Target.Partition != "aws" && b.Target.Partition != "aws-cn" && b.Target.Partition != "aws-us-gov") {
		return nil, ErrSource
	}
	arn := "arn:" + b.Target.Partition + ":s3:::" + b.Target.Bucket
	return catalog.Canonical(map[string]any{
		"Version": "2012-10-17",
		"Statement": []any{
			map[string]any{"Effect": "Allow", "Action": "*", "Resource": "*"},
			map[string]any{"Effect": "Deny", "Action": "s3:*", "Resource": []string{arn, arn + "/*"}, "Condition": map[string]any{"StringNotEquals": map[string]string{"s3:ResourceAccount": b.ExpectedOwner}}},
		},
	})
}

func validBucket(bucket string) bool {
	return regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(bucket) && !strings.Contains(bucket, "..") && !strings.Contains(bucket, "--x-s3") && !strings.HasSuffix(bucket, "-s3alias") && !regexp.MustCompile(`^[0-9.]+$`).MatchString(bucket)
}

func CanonicalBackend(a map[string]any, context BackendContext, tableARN string) (Backend, error) {
	workspace := context.Workspace
	b := Backend{Attributes: a}
	allowed := map[string]bool{"bucket": true, "key": true, "region": true, "workspace_key_prefix": true, "encrypt": true, "kms_key_id": true, "use_lockfile": true, "dynamodb_table": true}
	for k, v := range a {
		if !allowed[k] {
			return b, ErrSource
		}
		if k == "encrypt" || k == "use_lockfile" {
			if _, ok := v.(bool); !ok {
				return b, ErrSource
			}
		} else {
			if _, ok := v.(string); !ok {
				return b, ErrSource
			}
		}
	}
	str := func(k string) string { s, _ := a[k].(string); return s }
	bucket, key, region := str("bucket"), str("key"), str("region")
	if !validBucket(bucket) || !regexp.MustCompile(`^(us|eu|ap|sa|ca|me|af|il|mx|cn)-(gov-)?[a-z]+-[0-9]+$`).MatchString(region) || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(context.ExpectedOwner) || a["encrypt"] != true || !utf8.ValidString(key) || key == "" || strings.ContainsRune(key, 0) {
		return b, ErrSource
	}
	// Validate the raw key before workspace joining. Never repair a rejected key.
	if (context.Engine.Name != "terraform" || context.Engine.Version != "1.17.1") && (context.Engine.Name != "opentofu" || context.Engine.Version != "1.13.8") {
		return b, ErrSource
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") || (context.Engine.Name == "terraform" && strings.Contains(key, "//")) {
		return b, ErrSource
	}
	if prefix, ok := a["workspace_key_prefix"].(string); ok && (!utf8.ValidString(prefix) || strings.ContainsRune(prefix, 0) || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
		return b, ErrSource
	}
	partition := "aws"
	if strings.HasPrefix(region, "cn-") {
		partition = "aws-cn"
	}
	if strings.HasPrefix(region, "us-gov-") {
		partition = "aws-us-gov"
	}
	if workspace != "default" {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`).MatchString(workspace) {
			return b, ErrSource
		}
		prefix, ok := a["workspace_key_prefix"].(string)
		if !ok {
			prefix = "env:"
		}
		key = path.Join(prefix, workspace, key)
	}
	if len(key)+len(".tflock") > 1024 {
		return b, ErrSource
	}
	b.Target = Target{partition, bucket, key}
	b.Region = region
	b.ExpectedOwner = context.ExpectedOwner
	b.Profile = "s3"
	b.Claims = []Claim{{"s3_object", partition + "/" + bucket, key, "state"}, {"s3_object", partition + "/" + bucket, key + ".tflock", "lock"}}
	table := str("dynamodb_table")
	if table != "" {
		parts := strings.Split(tableARN, ":")
		if len(parts) != 6 || parts[0] != "arn" || parts[1] != partition || parts[2] != "dynamodb" || parts[3] != region || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(parts[4]) || parts[5] != "table/"+table || !regexp.MustCompile(`^[A-Za-z0-9_.-]{3,255}$`).MatchString(table) {
			return b, ErrSource
		}
		b.TableARN = tableARN
		b.Profile = "dynamodb"
		if a["use_lockfile"] == true {
			b.Profile = "dual"
		}
		lockID := bucket + "/" + key
		if len(lockID)+4 > 2048 {
			return b, ErrSource
		}
		b.Claims = append(b.Claims, Claim{"ddb_item", tableARN, lockID, "lock"}, Claim{"ddb_item", tableARN, lockID + "-md5", "checksum"})
	} else if a["use_lockfile"] != true {
		return b, ErrSource
	}
	if k := str("kms_key_id"); k != "" && !regexp.MustCompile(`^arn:`+partition+`:kms:`+region+`:[0-9]{12}:key/[A-Za-z0-9-]+$`).MatchString(k) {
		return b, ErrSource
	}
	return b, nil
}

// Inspect walks contained local modules and collects file/mode digests. Remote
// modules are resolved by the caller with the same fixed Git capability boundary.
func Inspect(root string, s catalog.Stack, tableARN string) (Inspection, error) {
	return InspectResolved(root, s, tableARN, nil)
}

type Resolver func(ModuleSource) (string, error)

func InspectResolved(root string, s catalog.Stack, tableARN string, resolve Resolver) (Inspection, error) {
	out := Inspection{Files: []File{}, Modules: []Module{}}
	if s.Validate() != nil {
		return out, ErrSource
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return out, ErrSource
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return out, ErrSource
	}
	a := map[string]any{}
	backends := 0
	seen := map[string]bool{}
	active := map[string]bool{}
	variables := map[string]bool{}
	roots := map[string]string{root: ""}
	var visit func(string, string, int) error
	visit = func(tree, dir string, depth int) error {
		if depth > 32 || len(seen) > 256 || !catalog.RepoPath(dir) {
			return ErrSource
		}
		key := tree + "/" + dir
		if active[key] {
			return ErrSource
		}
		if seen[key] {
			return nil
		}
		seen[key] = true
		active[key] = true
		defer delete(active, key)
		entries, e := os.ReadDir(filepath.Join(tree, filepath.FromSlash(dir)))
		if e != nil {
			return ErrSource
		}
		for _, entry := range entries {
			n := entry.Name()
			if !strings.HasSuffix(n, ".tf") && !strings.HasSuffix(n, ".tf.json") {
				continue
			}
			p := path.Join(dir, n)
			raw, e := Read(tree, p)
			if e != nil {
				return e
			}
			f, e := parsed(raw, p)
			if e != nil {
				return e
			}
			content, _, d := f.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}, {Type: "module", LabelNames: []string{"name"}}, {Type: "variable", LabelNames: []string{"name"}}}})
			if d.HasErrors() {
				return ErrSource
			}
			for _, block := range content.Blocks {
				switch block.Type {
				case "terraform":
					c, _, d := block.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "backend", LabelNames: []string{"type"}}}})
					if d.HasErrors() {
						return ErrSource
					}
					for _, bb := range c.Blocks {
						if tree != root || dir != s.Root || bb.Labels[0] != "s3" {
							return ErrSource
						}
						backends++
						v, e := attrs(bb.Body)
						if e != nil {
							return e
						}
						for k, x := range v {
							a[k] = x
						}
					}
				case "module":
					c, _, d := block.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source", Required: true}}})
					if d.HasErrors() {
						return ErrSource
					}
					v, e := literal(c.Attributes["source"].Expr)
					if e != nil || v.Type() != cty.String {
						return ErrSource
					}
					src := v.AsString()
					out.Modules = append(out.Modules, Module{Path: roots[tree] + path.Join(dir, block.Labels[0]), Source: src, Checkout: tree, CallerDirectory: dir})
					if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") {
						next := path.Join(dir, src)
						if !catalog.RepoPath(next) {
							return ErrSource
						}
						if e = visit(tree, next, depth+1); e != nil {
							return e
						}
					} else {
						remote, e := ParseModule(src)
						if e != nil || resolve == nil {
							return ErrSource
						}
						remoteRoot, e := resolve(remote)
						if e != nil {
							return e
						}
						remoteRoot, e = filepath.EvalSymlinks(remoteRoot)
						if e != nil {
							return ErrSource
						}
						sum := sha256.Sum256([]byte(src))
						if remoteRoot != root {
							roots[remoteRoot] = "modules/" + hex.EncodeToString(sum[:]) + "/"
						}
						if e = visit(remoteRoot, remote.Subdirectory, depth+1); e != nil {
							return e
						}
					}

				case "variable":
					if tree != root || dir != s.Root {
						continue
					}
					c, _, d := block.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "sensitive"}, {Name: "ephemeral"}}})
					if d.HasErrors() {
						return ErrSource
					}
					sensitive := false
					for k, x := range c.Attributes {
						v, e := literal(x.Expr)
						if e != nil || v.Type() != cty.Bool {
							return ErrSource
						}
						if k == "ephemeral" && v.True() {
							return ErrSource
						}
						if k == "sensitive" {
							sensitive = v.True()
						}
					}
					variables[block.Labels[0]] = sensitive
				}
			}
		}
		return nil
	}
	if e = visit(root, s.Root, 0); e != nil {
		return out, e
	}
	if backends != 1 {
		return out, ErrSource
	}
	for _, p := range s.BackendFiles {
		b, e := Read(root, p)
		if e != nil {
			return out, e
		}
		f, e := parsed(b, p)
		if e != nil {
			return out, e
		}
		v, e := attrs(f.Body)
		if e != nil {
			return out, e
		}
		for k, x := range v {
			a[k] = x
		}
	}
	for _, p := range s.VarFiles {
		if _, e = Read(root, p); e != nil {
			return out, e
		}
	}
	for _, secret := range s.Secrets {
		if strings.HasPrefix(secret.Env, "TF_VAR_") && !variables[strings.TrimPrefix(secret.Env, "TF_VAR_")] {
			return out, ErrSource
		}
	}
	if out.Backend, e = CanonicalBackend(a, BackendContext{Engine: s.Engine, Workspace: s.Workspace, ExpectedOwner: s.BackendOwnerAccountID}, tableARN); e != nil {
		return out, e
	}
	// Retain the full repository closure, excluding Git administrative data. Reject
	// special files, submodules and escape symlinks before any native process exists.
	total := int64(0)
	for tree, prefix := range roots {
		e = filepath.WalkDir(tree, func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return ErrSource
			}
			rel, _ := filepath.Rel(tree, p)
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			if rel == ".git" {
				return filepath.SkipDir
			}
			if !utf8.ValidString(rel) || len(out.Files) >= 10000 {
				return ErrSource
			}
			if d.IsDir() {
				return nil
			}
			if d.Name() == ".gitmodules" || d.Name() == ".git" {
				return ErrSource
			}
			b, e := Read(tree, rel)
			if e != nil {
				return e
			}
			if strings.HasPrefix(string(b), "version https://git-lfs.github.com/spec/v1") {
				return ErrSource
			}
			st, e := os.Lstat(p)
			if e != nil {
				return ErrSource
			}
			total += int64(len(b))
			if total > 67108864 {
				return ErrSource
			}
			mode := uint32(0100644)
			if st.Mode()&0111 != 0 {
				mode = 0100755
			}
			if st.Mode()&os.ModeSymlink != 0 {
				mode = 0120000
				link, e := os.Readlink(p)
				if e != nil || !utf8.ValidString(link) {
					return ErrSource
				}
				b = []byte(link)
			}
			h := sha256.Sum256(b)
			out.Files = append(out.Files, File{prefix + rel, mode, hex.EncodeToString(h[:])})
			return nil
		})
		if e != nil {
			return out, e
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	sort.Slice(out.Modules, func(i, j int) bool { return out.Modules[i].Path < out.Modules[j].Path })
	b, e := catalog.Canonical(struct {
		Backend Backend  `json:"backend"`
		Files   []File   `json:"files"`
		Modules []Module `json:"modules"`
	}{out.Backend, out.Files, out.Modules})
	if e != nil {
		return out, e
	}
	h := sha256.Sum256(b)
	out.Digest = hex.EncodeToString(h[:])
	return out, nil
}
