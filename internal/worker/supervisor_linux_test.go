//go:build linux

package worker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/source"
)

func TestRealGitExactCheckoutOverLocalHTTPS(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	os.Mkdir(work, 0700)
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "LANG=C.UTF-8", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	run := func(cwd string, args ...string) []byte {
		t.Helper()
		b, e := RunGit(ctx, cwd, args, env)
		if e != nil {
			t.Fatal("fixture Git command failed", args[0], e)
		}
		return b
	}
	run(work, "init", "--quiet")
	os.WriteFile(filepath.Join(work, "main.tf"), []byte("output \"fictional\" { value = \"local only\" }\n"), 0600)
	run(work, "add", "main.tf")
	run(work, "-c", "user.name=Fictional", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "Synthetic fixture")
	commit := strings.TrimSpace(string(run(work, "rev-parse", "HEAD")))
	run(dir, "-c", "protocol.file.allow=always", "clone", "--quiet", "--bare", work, filepath.Join(dir, "fixture.git"))
	server := httptest.NewTLSServer(&cgi.Handler{Path: "/usr/lib/git-core/git-http-backend", Dir: dir, Env: []string{"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1"}})
	defer server.Close()
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600)
	git := source.Git{Run: func(ctx context.Context, cwd string, args, env []string) ([]byte, error) {
		args = append([]string{}, args...)
		for i, a := range args {
			if a == "https://bitbucket.org/fictional/repo.git" {
				args[i] = server.URL + "/fixture.git"
			}
		}
		args = append([]string{"-c", "http.sslCAInfo=" + ca}, args...)
		return RunGit(ctx, cwd, args, env)
	}}
	checkout := filepath.Join(dir, "checkout")
	if e := git.Checkout(ctx, checkout, "https://bitbucket.org/fictional/repo.git", commit, ""); e != nil {
		t.Fatal(e)
	}
	b, e := source.Read(checkout, "main.tf")
	if e != nil || !strings.Contains(string(b), "local only") {
		t.Fatal("exact source absent", e)
	}
	if e = git.Checkout(ctx, filepath.Join(dir, "wrong"), "https://bitbucket.org/fictional/repo.git", strings.Repeat("a", 40), ""); e == nil {
		t.Fatal("unknown commit fell back to branch")
	}
}

func TestSupervisorRejectsFailureOverflowAndCancellation(t *testing.T) {
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	if _, e := runProgram(context.Background(), "/nonexistent-fixture", "/", nil, env); !errors.Is(e, source.ErrSource) {
		t.Fatal("missing binary accepted", e)
	}
	if _, e := runProgram(context.Background(), "/bin/sh", "/", []string{"-c", "exit 23"}, env); !errors.Is(e, source.ErrSource) {
		t.Fatal("failed child accepted", e)
	}
	if _, e := runProgram(context.Background(), "/usr/bin/head", "/", []string{"-c", "1048577", "/dev/zero"}, env); !errors.Is(e, source.ErrSource) {
		t.Fatal("output overflow accepted", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := RunGit(ctx, "/", []string{"--version"}, env); !errors.Is(e, source.ErrSource) {
		t.Fatal("cancelled operation started", e)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	done := make(chan error, 1)
	go func() {
		_, e := runProgram(ctx, "/bin/sh", "/", []string{"-c", `printf '%s\n' "$$" > "$1"; exec /bin/sleep 30`, "fixture", pidFile}, env)
		done <- e
	}()
	var pid int
	for pid == 0 {
		if raw, e := os.ReadFile(pidFile); e == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if pid == 0 {
			select {
			case e := <-done:
				t.Fatalf("child never started: %v", e)
			case <-ctx.Done():
				t.Fatal("child did not signal readiness")
			case <-time.After(time.Millisecond):
			}
		}
	}
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, source.ErrSource) {
			t.Fatal("cancellation did not report stopped child", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled child was not reaped promptly")
	}
	if e := syscall.Kill(pid, 0); e != syscall.ESRCH {
		t.Fatal("supervisor returned before the started child was reaped", e)
	}
}
func TestBakedRuntimeIdentity(t *testing.T) {
	if _, e := os.Stat("/usr/local/share/cosmoformer/runtime.json"); e != nil {
		if os.Getenv("COSMOFORMER_TEST_SOURCE") == "1" {
			t.Fatal("selected source suite requires baked runtime metadata")
		}
		t.Skip("baked runtime identity is exercised by the required source-tests image")
	}
	if _, e := VerifyRuntime(context.Background(), "/usr/local/share/cosmoformer/runtime.json"); e != nil {
		t.Fatal(e)
	}
}

func TestBakedRuntimeRejectsAlteredIdentityClaims(t *testing.T) {
	raw, e := os.ReadFile("/usr/local/share/cosmoformer/runtime.json")
	if e != nil {
		if os.Getenv("COSMOFORMER_TEST_SOURCE") == "1" {
			t.Fatal(e)
		}
		t.Skip("requires baked runtime")
	}
	var original RuntimeMetadata
	if e := json.Unmarshal(raw, &original); e != nil {
		t.Fatal(e)
	}
	for _, scenario := range []string{"malformed", "architecture", "binary checksum", "Git version", "Git package", "native version", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			m := original
			switch scenario {
			case "architecture":
				m.Arch = "arm64"
			case "binary checksum":
				m.SHA256 = strings.Repeat("0", 64)
			case "Git version":
				m.GitVersion = "unapproved"
			case "Git package":
				m.GitPackage = "unapproved"
			case "native version":
				m.Version = "0.0.0"
			}
			data, e := json.Marshal(m)
			if e != nil {
				t.Fatal(e)
			}
			if scenario == "malformed" {
				data = []byte("{")
			}
			path := filepath.Join(t.TempDir(), "runtime.json")
			if e := os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "cancelled" {
				cancel()
			}
			if _, e := VerifyRuntime(ctx, path); e == nil {
				t.Fatal("altered runtime identity admitted")
			}
		})
	}
}
