// Package catalog validates Git declarations against administrator-owned limits.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var ErrManifest = errors.New("invalid or unsupported manifest")
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var tagPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var workspacePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

func UUID(s string) bool { return uuidPattern.MatchString(s) }
func RepoPath(s string) bool {
	for _, component := range strings.Split(s, "/") {
		if component == ".git" || component == ".terraform" {
			return false
		}
	}
	return utf8.ValidString(s) && s != "" && len(s) <= 4096 && !strings.ContainsAny(s, "\\\x00\r\n") && !strings.HasPrefix(s, "/") && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../") && !strings.ContainsAny(s, "*?[]${}")
}
func Branch(s string) bool {
	for _, c := range s {
		if c < 32 || c == 127 {
			return false
		}
	}
	if s == "" || len(s) > 255 || s == "@" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.ContainsAny(s, " ~^:?*[\\\x00\r\n\t") {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "" || strings.HasPrefix(p, ".") || strings.HasSuffix(p, ".") || strings.HasSuffix(p, ".lock") {
			return false
		}
	}
	return utf8.ValidString(s)
}

type Engine struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version" json:"version"`
}
type Pool struct {
	ID   string   `yaml:"pool_id" json:"pool_id"`
	Tags []string `yaml:"tags" json:"tags"`
}
type SecretSource struct {
	Scope string `yaml:"scope" json:"scope"`
	ID    string `yaml:"id" json:"id"`
}
type Secret struct {
	Source SecretSource `yaml:"source" json:"source"`
	Env    string       `yaml:"env" json:"env"`
}
type Approvals struct {
	Minimum int `yaml:"minimum" json:"minimum"`
}
type Stack struct {
	ID                    string    `yaml:"id" json:"id"`
	Name                  string    `yaml:"name" json:"name"`
	ProjectID             string    `yaml:"project_id" json:"project_id"`
	EnvironmentID         string    `yaml:"environment_id" json:"environment_id"`
	Root                  string    `yaml:"root" json:"root"`
	TrackedBranch         string    `yaml:"tracked_branch" json:"tracked_branch"`
	Engine                Engine    `yaml:"engine" json:"engine"`
	Workspace             string    `yaml:"workspace" json:"workspace"`
	BackendOwnerAccountID string    `yaml:"backend_owner_account_id" json:"backend_owner_account_id"`
	BackendFiles          []string  `yaml:"backend_config_files" json:"backend_config_files"`
	VarFiles              []string  `yaml:"var_files" json:"var_files"`
	Worker                Pool      `yaml:"worker" json:"worker"`
	Secrets               []Secret  `yaml:"secrets" json:"secrets"`
	Approvals             Approvals `yaml:"approvals" json:"approvals"`
	AllowPRApply          bool      `yaml:"allow_pr_apply" json:"allow_pr_apply"`
	ChangePaths           []string  `yaml:"change_paths" json:"change_paths"`
	DependsOn             []string  `yaml:"depends_on" json:"depends_on"`
}
type Manifest struct {
	Version int     `yaml:"version" json:"version"`
	Stacks  []Stack `yaml:"stacks" json:"stacks"`
}

// Parse inspects syntax before decoding. No YAML expansion or implicit template
// language is accepted; error messages never include submitted scalar values.
func Parse(b []byte) (Manifest, error) {
	var m Manifest
	if len(b) > 1048576 || !utf8.Valid(b) || bytes.Contains(b, []byte("${")) {
		return m, ErrManifest
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	var node yaml.Node
	if d.Decode(&node) != nil || len(node.Content) != 1 {
		return m, ErrManifest
	}
	var next yaml.Node
	if d.Decode(&next) != io.EOF {
		return m, ErrManifest
	}
	var check func(*yaml.Node, int) bool
	check = func(n *yaml.Node, depth int) bool {
		if depth > 32 || n.Kind == yaml.AliasNode || n.Anchor != "" || n.Style&yaml.TaggedStyle != 0 {
			return false
		}
		if n.Tag != "" && n.Tag != "!!map" && n.Tag != "!!seq" && n.Tag != "!!str" && n.Tag != "!!int" && n.Tag != "!!bool" {
			return false
		}
		if n.Kind == yaml.MappingNode {
			seen := map[string]bool{}
			for i := 0; i < len(n.Content); i += 2 {
				k := n.Content[i]
				if k.Tag != "!!str" || seen[k.Value] {
					return false
				}
				seen[k.Value] = true
			}
		}
		for _, c := range n.Content {
			if !check(c, depth+1) {
				return false
			}
		}
		return true
	}
	if !check(&node, 0) {
		return m, ErrManifest
	}
	d = yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if d.Decode(&m) != nil || m.Version != 1 || len(m.Stacks) < 1 || len(m.Stacks) > 1000 {
		return m, ErrManifest
	}
	seen := map[string]bool{}
	// Defaults apply only when omitted, never to explicitly invalid zero values.
	stacksNode := node.Content[0]
	var items *yaml.Node
	for i := 0; i < len(stacksNode.Content); i += 2 {
		if stacksNode.Content[i].Value == "stacks" {
			items = stacksNode.Content[i+1]
		}
	}
	if items == nil || items.Kind != yaml.SequenceNode {
		return m, ErrManifest
	}
	for i := range m.Stacks {
		s := &m.Stacks[i]
		hasApprovals := false
		for j := 0; j < len(items.Content[i].Content); j += 2 {
			if items.Content[i].Content[j].Value == "backend_owner_account_id" && items.Content[i].Content[j+1].Tag != "!!str" {
				return m, ErrManifest
			}
			if items.Content[i].Content[j].Value == "approvals" {
				hasApprovals = true
			}
		}
		if !hasApprovals {
			s.Approvals.Minimum = 1
		}
		if s.BackendFiles == nil {
			s.BackendFiles = []string{}
		}
		if s.VarFiles == nil {
			s.VarFiles = []string{}
		}
		if s.Worker.Tags == nil {
			s.Worker.Tags = []string{}
		}
		if s.Secrets == nil {
			s.Secrets = []Secret{}
		}
		if s.ChangePaths == nil {
			s.ChangePaths = []string{}
		}
		if s.DependsOn == nil {
			s.DependsOn = []string{}
		}
		if seen[s.ID] || s.Validate() != nil {
			return m, ErrManifest
		}
		seen[s.ID] = true
	}
	return m, nil
}
func unique(values []string, limit int, valid func(string) bool) bool {
	if len(values) > limit {
		return false
	}
	seen := map[string]bool{}
	for _, s := range values {
		if seen[s] || !valid(s) {
			return false
		}
		seen[s] = true
	}
	return true
}
func (s Stack) Validate() error {
	if !UUID(s.ID) || !UUID(s.ProjectID) || !UUID(s.EnvironmentID) || utf8.RuneCountInString(s.Name) < 1 || utf8.RuneCountInString(s.Name) > 100 || !RepoPath(s.Root) || !Branch(s.TrackedBranch) || (s.Engine.Name != "opentofu" && s.Engine.Name != "terraform") || !versionPattern.MatchString(s.Engine.Version) || !workspacePattern.MatchString(s.Workspace) || !regexp.MustCompile(`^[0-9]{12}$`).MatchString(s.BackendOwnerAccountID) || !UUID(s.Worker.ID) || s.Approvals.Minimum < 1 || s.Approvals.Minimum > 100 {
		return ErrManifest
	}
	if !unique(s.BackendFiles, 32, RepoPath) || !unique(s.VarFiles, 32, RepoPath) || !unique(s.Worker.Tags, 16, tagPattern.MatchString) || !unique(s.ChangePaths, 64, RepoPath) || !unique(s.DependsOn, 64, func(v string) bool { return UUID(v) && v != s.ID }) || len(s.Secrets) > 64 {
		return ErrManifest
	}
	seen := map[string]bool{}
	for _, v := range s.Secrets {
		if !UUID(v.Source.ID) || (v.Source.Scope != "org" && v.Source.Scope != "project") || !SecretEnv(v.Env) || seen[v.Env] {
			return ErrManifest
		}
		seen[v.Env] = true
	}
	return nil
}
func SecretEnv(s string) bool {
	if !envPattern.MatchString(s) {
		return false
	}
	if strings.HasPrefix(s, "TF_VAR_") {
		return len(s) > 7 && envPattern.MatchString(s[7:])
	}
	for _, p := range []string{"AWS_", "TF_", "TOFU_", "GIT_", "COSMOFORMER_", "LD_", "DYLD_"} {
		if strings.HasPrefix(s, p) {
			return false
		}
	}
	for _, p := range []string{"PATH", "HOME", "TMPDIR", "LANG", "SHELL", "BASH_ENV", "ENV", "PWD", "PYTHONPATH", "NODE_OPTIONS"} {
		if s == p {
			return false
		}
	}
	return true
}

// Canonical uses sorted JSON object keys while preserving array order.
func Canonical(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var x any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if e = d.Decode(&x); e != nil {
		return nil, e
	}
	return json.Marshal(x)
}
