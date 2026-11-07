package source

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func Commit(s string) bool { return commitPattern.MatchString(s) }
func Repository(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Host == "bitbucket.org" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && regexp.MustCompile(`^/[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+\.git$`).MatchString(u.Path) && !strings.Contains(u.Path, "..") && u.RawPath == ""
}

type Runner func(context.Context, string, []string, []string) ([]byte, error)
type Git struct {
	Run    Runner
	Helper string
}

// Checkout never reads user Git configuration and never executes a shell. The
// credential helper is a fixed worker subcommand, not repository-controlled text.
func (g Git) Checkout(ctx context.Context, root, repo, commit, credentialContext string) error {
	if g.Run == nil || !Repository(repo) || !Commit(commit) {
		return ErrSource
	}
	if e := os.Mkdir(root, 0700); e != nil {
		return ErrSource
	}
	home := filepath.Join(filepath.Dir(root), "git-home-"+filepath.Base(root))
	if e := os.Mkdir(home, 0700); e != nil {
		return ErrSource
	}
	defer os.RemoveAll(home)
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + home, "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	if credentialContext != "" {
		env = append(env, "COSMOFORMER_GIT_CONTEXT="+credentialContext)
	}
	fixed := []string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "http.followRedirects=false", "-c", "credential.useHttpPath=true", "-c", "credential.helper=", "-c", "filter.lfs.required=false", "-c", "filter.lfs.smudge=", "-c", "submodule.recurse=false"}
	if credentialContext != "" {
		if !filepath.IsAbs(g.Helper) || strings.ContainsAny(g.Helper, " \t\r\n\"'`$\\") {
			return ErrSource
		}
		fixed = append(fixed, "-c", "credential.helper="+g.Helper+" credential-helper")
	}
	run := func(args ...string) ([]byte, error) {
		return g.Run(ctx, root, append(append([]string{}, fixed...), args...), env)
	}
	if _, e := run("init", "--quiet"); e != nil {
		return e
	}
	if _, e := run("fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--depth=1", repo, commit); e != nil {
		return e
	}
	got, e := run("rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if e != nil {
		return e
	}
	if strings.TrimSpace(string(got)) != commit {
		return ErrSource
	}
	tree, e := run("ls-tree", "-rz", commit)
	if e != nil {
		return e
	}
	for _, entry := range strings.Split(string(tree), "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			return ErrSource
		}
	}
	if _, e = run("checkout", "--quiet", "--detach", commit); e != nil {
		return e
	}
	if _, e = os.Stat(filepath.Join(root, ".gitmodules")); e == nil {
		return ErrSource
	}
	// Administrative data must not be traversed as source or reused on another
	// attempt. It is removed at attempt cleanup by the agent.
	return nil
}

type ModuleSource struct{ Repository, Commit, Subdirectory string }

func ParseModule(s string) (ModuleSource, error) {
	var m ModuleSource
	if !strings.HasPrefix(s, "git::https://") {
		return m, ErrSource
	}
	u, e := url.Parse(strings.TrimPrefix(s, "git::"))
	if e != nil || u.User != nil || u.Fragment != "" {
		return m, ErrSource
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil || len(q) != 1 || len(q["ref"]) != 1 || !Commit(q.Get("ref")) {
		return m, ErrSource
	}
	m.Commit = q.Get("ref")
	u.RawQuery = ""
	parts := strings.SplitN(u.Path, "//", 2)
	u.Path = parts[0]
	m.Repository = u.String()
	if !Repository(m.Repository) {
		return m, ErrSource
	}
	m.Subdirectory = "."
	if len(parts) == 2 {
		m.Subdirectory = parts[1]
		if !safeSubdir(m.Subdirectory) {
			return m, ErrSource
		}
	}
	return m, nil
}
func safeSubdir(s string) bool {
	return s != "" && !strings.HasPrefix(s, "/") && !strings.Contains(s, "..") && !strings.ContainsAny(s, "\\\x00?*${}")
}

var ErrProcess = errors.New("source process failed or stop could not be proven")
