package worker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cosmoformer/cosmoformer/internal/awsaccess"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

// NativeIO is the process I/O seam used by the isolated protocol suite. Only
// fixed descriptors are constructed by the planning pipeline. The native suite
// separately exercises the real engines without any database connection.
type NativeIO func(context.Context, NativeCommand, func(runs.CommandEvent) error) (runs.CommandEvent, error)
type NativeCommand struct {
	Start        func(func() error) error
	Program, Dir string
	Args, Env    []string
	Descriptor   runs.CommandDescriptor
	Permission   runs.Permission
	Output       *os.File
	Logs         *LogCapture
}
type ExecutionAccess interface {
	Prepare(context.Context) error
	Environment(context.Context, awsaccess.Binding) ([]string, error)
	RemainingMS() (int64, error)
}
type AccessFactory func(string, runs.Assignment, runs.Identity, time.Duration, *evidence.Registry) (ExecutionAccess, error)

func defaultAccess(expected string, a runs.Assignment, id runs.Identity, anchor time.Duration, reg *evidence.Registry) (ExecutionAccess, error) {
	return awsaccess.New(expected, a, id, anchor, reg)
}

func nativeEnvironment(dir, helper string, s runs.ExecutionSpec, secrets []runs.EnvSecret, aws []string) ([]string, error) {
	for _, sub := range []string{"home", "tmp", "data"} {
		if e := os.Mkdir(filepath.Join(dir, sub), 0700); e != nil {
			return nil, ErrAgent
		}
	}
	// This config is platform-generated; it cannot enable arbitrary providers,
	// credentials helpers, repository CLI settings or plugin cache reuse.
	config := filepath.Join(dir, "native.tfrc")
	raw := `disable_checkpoint = true
provider_installation {
 direct {
  include = ["registry.terraform.io/*/*", "registry.opentofu.org/*/*"]
 }
}
`
	if e := os.WriteFile(config, []byte(raw), 0600); e != nil {
		return nil, ErrAgent
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"), "TMPDIR=" + filepath.Join(dir, "tmp"), "LANG=C.UTF-8", "TF_IN_AUTOMATION=1", "TF_INPUT=0", "TF_WORKSPACE=" + s.Workspace, "TF_DATA_DIR=" + filepath.Join(dir, "data"), "TF_CLI_CONFIG_FILE=" + config, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "git.config"), "GIT_TERMINAL_PROMPT=0", "COSMOFORMER_GIT_CONTEXT=" + helper}
	// Native module fetching uses the same strict helper and HTTPS policy as
	// preflight, with no global/system fallback or repository-authored hooks.
	git := `[core]
 hooksPath = /dev/null
[protocol]
 allow = never
[protocol "https"]
 allow = always
[http]
 followRedirects = false
[credential]
 useHttpPath = true
 helper =
 helper = /usr/local/bin/cosmoformer-worker credential-helper
[submodule]
 recurse = false
[filter "lfs"]
 required = false
 smudge =
`
	if e := os.WriteFile(filepath.Join(dir, "git.config"), []byte(git), 0600); e != nil {
		return nil, ErrAgent
	}
	expected := []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_DEFAULT_REGION"}
	if len(aws) != len(expected) {
		return nil, ErrAgent
	}
	for i, v := range aws {
		k, value, ok := strings.Cut(v, "=")
		if !ok || k != expected[i] || value == "" || strings.ContainsRune(value, 0) {
			return nil, ErrAgent
		}
		env = append(env, v)
	}
	seen := map[string]bool{}
	for _, v := range secrets {
		if !catalog.SecretEnv(v.Destination) || seen[v.Destination] || v.Value == "" || !utf8.ValidString(v.Value) || strings.ContainsRune(v.Value, 0) {
			return nil, ErrAgent
		}
		seen[v.Destination] = true
		env = append(env, v.Destination+"="+v.Value)
	}
	total := 0
	for _, v := range env {
		total += len(v) + 1
	}
	if total > 131072 {
		return nil, ErrAgent
	}
	return env, nil
}
func nativeArgs(s runs.ExecutionSpec, dir, tree, command string, lock bool) (string, string, []string, error) {
	root := filepath.Join(tree, filepath.FromSlash(s.Source.Root))
	program := "/usr/local/bin/tofu"
	if s.Engine.Name == "terraform" {
		program = "/usr/local/bin/terraform"
	}
	args := append([]string{}, runs.FixedFlags(command)...)
	if len(args) == 0 || command == "apply" {
		return "", "", nil, ErrAgent
	}
	switch command {
	case "init":
		for _, p := range s.BackendFiles {
			if _, e := sourceInput(tree, p); e != nil {
				return "", "", nil, e
			}
			args = append(args, "-backend-config="+filepath.Join(tree, filepath.FromSlash(p)))
		}
		if lock {
			args = append(args, "-lockfile=readonly")
		}
	case "plan":
		args = append(args, "-out="+filepath.Join(dir, "plan.bin"))
		for _, p := range s.VarFiles {
			if _, e := sourceInput(tree, p); e != nil {
				return "", "", nil, e
			}
			args = append(args, "-var-file="+filepath.Join(tree, filepath.FromSlash(p)))
		}
	case "show":
		args = append(args, filepath.Join(dir, "plan.bin"))
	}
	return program, root, args, nil
}
func sourceInput(root, p string) ([]byte, error) { return source.Read(root, p) }

func protectedBytes(path string, limit int64) ([]byte, error) {
	before, e := os.Lstat(path)
	if e != nil || !before.Mode().IsRegular() {
		return nil, ErrAgent
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, ErrAgent
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || !os.SameFile(before, st) || st.Size() > limit {
		return nil, ErrAgent
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, ErrAgent
	}
	return b, nil
}
