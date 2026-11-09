package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutFailuresStopBeforeAnyLaterGitCommand(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, stage := range []string{"init", "fetch", "rev-parse", "ls-tree", "checkout"} {
		t.Run(stage, func(t *testing.T) {
			sentinel := errors.New("synthetic Git failure")
			failed := false
			g := Git{Run: func(_ context.Context, _ string, args, env []string) ([]byte, error) {
				if failed {
					t.Error("Git command ran after failure")
				}
				for _, command := range []string{"init", "fetch", "rev-parse", "ls-tree", "checkout"} {
					for _, arg := range args {
						if arg == command {
							if command == stage {
								failed = true
								return nil, sentinel
							}
							if command == "rev-parse" {
								return []byte(commit), nil
							}
							return nil, nil
						}
					}
				}
				t.Fatal("unexpected Git command")
				return nil, nil
			}}
			parent := t.TempDir()
			root := filepath.Join(parent, "checkout")
			if e := g.Checkout(context.Background(), root, "https://bitbucket.org/fictional/repo.git", commit, ""); !errors.Is(e, sentinel) || !failed {
				t.Fatal(e)
			}
			if _, e := os.Stat(filepath.Join(parent, "git-home-checkout")); !os.IsNotExist(e) {
				t.Fatal("private Git home survived failure", e)
			}
		})
	}
}
func TestCheckoutRejectsCredentialHelperAndUntrustedTrees(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, scenario := range []string{"existing checkout", "existing private home", "relative helper", "shell helper", "wrong commit", "submodule", "gitmodules"} {
		t.Run(scenario, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "checkout")
			calls := 0
			g := Git{Helper: "/usr/local/bin/worker", Run: func(_ context.Context, _ string, args, env []string) ([]byte, error) {
				calls++
				for i, arg := range args {
					if arg == "rev-parse" {
						if scenario == "wrong commit" {
							return []byte(strings.Repeat("b", 40)), nil
						}
						return []byte(commit), nil
					}
					if arg == "ls-tree" && scenario == "submodule" {
						return []byte("160000 commit " + commit + "\tmodule\x00"), nil
					}
					if arg == "checkout" && i+1 < len(args) && scenario == "gitmodules" {
						if e := os.WriteFile(filepath.Join(root, ".gitmodules"), []byte("untrusted"), 0600); e != nil {
							t.Fatal(e)
						}
					}
				}
				return nil, nil
			}}
			switch scenario {
			case "existing checkout":
				os.Mkdir(root, 0700)
			case "existing private home":
				os.Mkdir(filepath.Join(parent, "git-home-checkout"), 0700)
			case "relative helper":
				g.Helper = "worker"
			case "shell helper":
				g.Helper = "/tmp/worker; sh"
			}
			if e := g.Checkout(context.Background(), root, "https://bitbucket.org/fictional/repo.git", commit, "synthetic-context"); e == nil {
				t.Fatal("unsafe checkout accepted")
			}
			if strings.Contains(scenario, "helper") && calls != 0 {
				t.Fatal("invalid helper reached Git", calls)
			}
		})
	}
}
