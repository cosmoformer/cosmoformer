package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"go.yaml.in/yaml/v3"
)

type agentClock struct {
	tick time.Duration
	err  error
}

func (c agentClock) Now() (time.Duration, error) { return c.tick, c.err }

func TestAgentRejectsUnsafeHTTPResponses(t *testing.T) {
	for name, body := range map[string]string{"unknown field": `{"unexpected":true}`, "trailing JSON": `{} {}`, "oversized": strings.Repeat("x", 1048577), "not JSON": "fixture"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			a := Agent{HTTP: server.Client(), Origin: server.URL}
			var out struct{}
			if e := a.call(context.Background(), "/poll", map[string]string{}, &out); e == nil {
				t.Fatal("unsafe worker response accepted")
			}
		})
	}
	targetCalls := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	a := Agent{HTTP: server.Client(), Origin: server.URL, Token: "synthetic-only"}
	var out struct{}
	if e := a.call(context.Background(), "/poll", nil, &out); e == nil || targetCalls != 0 {
		t.Fatal("worker followed redirect", e)
	}
	for _, origin := range []string{"http://localhost", "https://user@localhost", "https://localhost/path", "https://localhost?token=fixture", "https://localhost#fragment", "%"} {
		a.Origin = origin
		if e := a.call(context.Background(), "/poll", nil, &out); e == nil {
			t.Fatal("unsafe origin accepted")
		}
	}
}

func TestAgentModulePreflightAndCredentialRedaction(t *testing.T) {
	for _, scenario := range []string{"success", "grant refused", "known credential in backend", "module stop uncertain"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			commit := strings.Repeat("a", 40)
			runtimeID := runs.ID()
			repoID := runs.ID()
			moduleID := runs.ID()
			stack := catalog.Stack{ID: runs.ID(), Name: "Fixture", ProjectID: runs.ID(), EnvironmentID: runs.ID(), Root: ".", TrackedBranch: "main", Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Workspace: "default", BackendOwnerAccountID: "111122223333", Worker: catalog.Pool{ID: runs.ID()}, Approvals: catalog.Approvals{Minimum: 1}}
			manifest, e := yaml.Marshal(catalog.Manifest{Version: 1, Stacks: []catalog.Stack{stack}})
			if e != nil {
				t.Fatal(e)
			}
			spec := runs.PreflightSpec{Version: 1, Phase: "preflight", Source: runs.Source{Root: ".", RepositoryID: repoID, URL: "https://bitbucket.org/fictional/repo.git", Commit: commit}, Engine: runs.Runtime{ID: runtimeID, Name: stack.Engine.Name, Version: stack.Engine.Version}, Workspace: stack.Workspace, BackendOwnerAccountID: stack.BackendOwnerAccountID, Pool: runs.Pool{ID: stack.Worker.ID}, Grants: []runs.ResourceRef{{Kind: "repository", ResourceID: moduleID}}}
			spec.VarFiles = []string{}
			spec.BackendFiles = []string{}
			canonical, _ := catalog.Canonical(spec)
			assignment := runs.Assignment{AttemptID: runs.ID(), WorkItemID: runs.ID(), Phase: "preflight", Spec: spec, Source: spec.Source, Digest: hash(canonical), RemainingMS: 300000, NextSeq: 1}
			var result runs.Result
			var prepared runs.Prepare
			seq := int64(1)
			credentials := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fictional-platform-token" {
					t.Error("worker authentication missing")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/admit"):
					json.NewEncoder(w).Encode(map[string]any{"generation": 1, "idle_lease_seconds": 30, "heartbeat_seconds": 5})
				case strings.HasSuffix(r.URL.Path, "/poll"):
					json.NewEncoder(w).Encode(map[string]any{"assignment": assignment})
				case strings.HasSuffix(r.URL.Path, "/source-credential"):
					credentials++
					if scenario == "grant refused" {
						w.WriteHeader(403)
						w.Write([]byte(`{"code":"FORBIDDEN"}`))
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"username": "x-token-auth", "token": "fictional-registered-token", "credential_revision": 1})
				default:
					var m runs.Message
					if json.NewDecoder(r.Body).Decode(&m) != nil {
						t.Error("bad message")
					}
					if m.Seq != seq || m.AttemptID != assignment.AttemptID {
						t.Error("sequence or attempt mismatch")
					}
					seq++
					if bytes.Contains(m.Payload, []byte("fictional-registered-token")) {
						t.Error("known credential leaked into protocol")
					}
					if strings.HasSuffix(r.URL.Path, "/prepare-result") {
						if e := json.Unmarshal(m.Payload, &prepared); e != nil {
							t.Error(e)
						}
					}
					if strings.HasSuffix(r.URL.Path, "/result") {
						if e := json.Unmarshal(m.Payload, &result); e != nil {
							t.Error(e)
						}
					}
					json.NewEncoder(w).Encode(runs.Transition{Accepted: true, NextSeq: seq})
				}
			}))
			defer server.Close()
			dir, e := os.MkdirTemp("", "cf-agent-")
			if e != nil {
				t.Fatal(e)
			}
			defer os.RemoveAll(dir)
			a := Agent{HTTP: server.Client(), Origin: server.URL, Token: "fictional-platform-token", Clock: agentClock{tick: time.Second}, AttemptDir: dir, Admission: runs.Admission{WorkerID: runs.ID(), Agent: runs.ID(), RuntimeID: runtimeID}}
			moduleCheckouts := 0
			a.Git.Helper = "/synthetic/worker"
			a.Git.Run = func(ctx context.Context, dir string, args, env []string) ([]byte, error) {
				joined := strings.Join(args, " ")
				if strings.Contains(joined, "rev-parse") {
					return []byte(commit), nil
				}
				if !strings.Contains(joined, "checkout --quiet --detach") {
					return nil, nil
				}
				if strings.HasPrefix(filepath.Base(dir), "module-") {
					moduleCheckouts++
					if scenario == "module stop uncertain" {
						return nil, source.ErrProcess
					}
					return nil, os.WriteFile(filepath.Join(dir, "module.tf"), []byte(`output "fixture" { value = "local only" }`), 0600)
				}
				if e := os.WriteFile(filepath.Join(dir, "cosmoformer.yaml"), manifest, 0600); e != nil {
					return nil, e
				}
				key := "state.tfstate"
				if scenario == "known credential in backend" {
					key = "fictional-registered-token"
				}
				body := `terraform {
 backend "s3" {
 bucket = "fictional-state-bucket"
 key = "` + key + `"
 region = "eu-west-1"
 encrypt = true
 use_lockfile = true
 }
}
module "first" { source = "git::https://bitbucket.org/fictional/modules.git?ref=` + commit + `" }
module "second" { source = "git::https://bitbucket.org/fictional/modules.git?ref=` + commit + `" }
`
				return nil, os.WriteFile(filepath.Join(dir, "main.tf"), []byte(body), 0600)
			}
			if e := a.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			runErr := a.RunOne(ctx)
			if scenario == "module stop uncertain" {
				if !errors.Is(runErr, ErrAgent) || moduleCheckouts != 1 || result.Reaped || prepared.Digest != "" {
					t.Fatal("uncertain module stop produced a terminal proof", runErr, result)
				}
				entries, e := os.ReadDir(dir)
				if e != nil || len(entries) != 1 {
					t.Fatal("unproven module stop removed attempt files", e)
				}
				if seq != 2 {
					t.Fatal("uncertain process sent more than its ack", seq)
				}
				return
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			if !result.NoStateChild || !result.Reaped || credentials != 1 {
				t.Fatal("missing preparation proof or repeated capability lookup", credentials, result)
			}
			if scenario == "success" {
				if result.Outcome != "success" || len(prepared.Modules) != 2 || moduleCheckouts != 1 || len(prepared.Digest) != 64 || result.Digest != prepared.Digest {
					t.Fatal("module closure not prepared once", result, prepared, moduleCheckouts)
				}
			} else if result.Outcome != "preparation_failed" || prepared.Digest != "" {
				t.Fatal("unsafe preparation was reported ready", result)
			}
			entries, e := os.ReadDir(dir)
			if e != nil || len(entries) != 0 {
				t.Fatal("source capability or checkout retained", entries, e)
			}
			if len(a.terms) != 0 {
				t.Fatal("credential redaction registry outlived attempt")
			}
		})
	}
}

func TestAgentRequiresWorkingClockAndAdmissionInputs(t *testing.T) {
	a := Agent{}
	if e := a.Admit(context.Background()); e == nil {
		t.Fatal("incomplete worker admitted")
	}
	sentinel := errors.New("synthetic clock failure")
	a.Clock = agentClock{err: sentinel}
	a.HTTP = &http.Client{}
	a.AttemptDir = t.TempDir()
	a.Git.Run = RunGit
	if e := a.Admit(context.Background()); !errors.Is(e, sentinel) {
		t.Fatal("clock failure ignored", e)
	}
	if e := a.RunOne(context.Background()); !errors.Is(e, sentinel) {
		t.Fatal("preflight started without elapsed budget", e)
	}
}

func TestDeclarationRejectsChangedBackendOwnerMetadata(t *testing.T) {
	s := catalog.Stack{Root: ".", Workspace: "default", BackendOwnerAccountID: "111122223333", Engine: catalog.Engine{Name: "opentofu", Version: "1.13.8"}, Worker: catalog.Pool{ID: "fixture-pool"}, TrackedBranch: "main"}
	p := runs.PreflightSpec{Source: runs.Source{Root: s.Root}, Workspace: s.Workspace, BackendOwnerAccountID: s.BackendOwnerAccountID, Engine: runs.Runtime{Name: s.Engine.Name, Version: s.Engine.Version}, Pool: runs.Pool{ID: s.Worker.ID}}
	if !matches(s, p) {
		t.Fatal("matching metadata rejected")
	}
	p.BackendOwnerAccountID = "999900001111"
	if matches(s, p) {
		t.Fatal("changed owner accepted against immutable assignment")
	}
	p.BackendOwnerAccountID = ""
	if matches(s, p) {
		t.Fatal("legacy assignment without owner accepted")
	}
}
