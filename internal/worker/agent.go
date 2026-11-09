// Package worker runs source preflight and supplies protected input/permission
// primitives. Native dispatch and AWS session creation remain unimplemented.
package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

var ErrAgent = errors.New("worker request failed; retained assignments require reconciliation before retry")

type RuntimeMetadata struct {
	Engine     string `json:"engine"`
	Version    string `json:"version"`
	Arch       string `json:"arch"`
	SHA256     string `json:"binary_sha256"`
	GitPackage string `json:"git_package_version"`
	GitVersion string `json:"git_version"`
}

func VerifyRuntime(ctx context.Context, path string) (RuntimeMetadata, error) {
	var m RuntimeMetadata
	b, e := os.ReadFile(path)
	if e != nil || len(b) > 16384 || runs.Decode(b, &m) != nil {
		return m, ErrAgent
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || m.Arch != "amd64" || (m.Engine != "terraform" && m.Engine != "opentofu") || len(m.SHA256) != 64 {
		return m, ErrAgent
	}
	program := "/usr/local/bin/terraform"
	if m.Engine == "opentofu" {
		program = "/usr/local/bin/tofu"
	}
	f, e := os.Open(program)
	if e != nil {
		return m, ErrAgent
	}
	h := sha256.New()
	_, e = io.Copy(h, io.LimitReader(f, 268435457))
	f.Close()
	if e != nil || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return m, ErrAgent
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8", "CHECKPOINT_DISABLE=1"}
	git, e := runProgram(ctx, "/usr/bin/git", "/", []string{"--version"}, env)
	if e != nil || strings.TrimSpace(string(git)) != m.GitVersion {
		return m, ErrAgent
	}
	pkg, e := runProgram(ctx, "/usr/bin/dpkg-query", "/", []string{"-W", "-f=${Version}", "git"}, env)
	if e != nil || strings.TrimSpace(string(pkg)) != m.GitPackage {
		return m, ErrAgent
	}
	native, e := runProgram(ctx, program, "/", []string{"version", "-json"}, env)
	if e != nil {
		return m, ErrAgent
	}
	var version struct {
		Version  string `json:"terraform_version"`
		Platform string `json:"platform"`
	}
	if json.Unmarshal(native, &version) != nil || version.Version != m.Version || version.Platform != "linux_amd64" {
		return m, ErrAgent
	}
	return m, nil
}

type Agent struct {
	HTTP                      *http.Client
	Origin, Token, AttemptDir string
	Admission                 runs.Admission
	Identity                  runs.Identity
	Clock                     clock.Source
	Git                       source.Git
	mu                        sync.Mutex
	terms                     []string
	redactor                  *evidence.Registry
}

func (a *Agent) call(ctx context.Context, path string, in, out any) error {
	u, e := url.Parse(a.Origin)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return ErrAgent
	}
	b, e := json.Marshal(in)
	if e != nil {
		return ErrAgent
	}
	r, e := http.NewRequestWithContext(ctx, "POST", a.Origin+"/worker/v1"+path, bytes.NewReader(b))
	if e != nil {
		return ErrAgent
	}
	r.Header.Set("Authorization", "Bearer "+a.Token)
	r.Header.Set("Content-Type", "application/json")
	client := *a.HTTP
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, e := client.Do(r)
	if e != nil {
		return ErrAgent
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 1048577))
	if e != nil || len(raw) > 1048576 {
		return ErrAgent
	}
	if resp.StatusCode != 200 {
		var response struct {
			Code string `json:"code"`
		}
		json.Unmarshal(raw, &response)
		code := response.Code
		for _, c := range code {
			if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				code = "REQUEST_REJECTED"
				break
			}
		}
		return fmt.Errorf("worker request rejected (%d, %s)", resp.StatusCode, code)
	}
	// Huma adds its schema-link annotation to HTTP response objects. It is not
	// worker protocol data; all remaining response fields stay closed.
	var annotated map[string]json.RawMessage
	if json.Unmarshal(raw, &annotated) != nil {
		return ErrAgent
	}
	delete(annotated, "$schema")
	raw, _ = json.Marshal(annotated)
	if runs.Decode(raw, out) != nil {
		return ErrAgent
	}
	return nil
}
func (a *Agent) Admit(ctx context.Context) error {
	if a.Clock == nil || a.HTTP == nil || a.Git.Run == nil || !filepath.IsAbs(a.AttemptDir) {
		return ErrAgent
	}
	if _, e := a.Clock.Now(); e != nil {
		return e
	}
	var response struct {
		Generation int64 `json:"generation"`
		IdleLease  int   `json:"idle_lease_seconds"`
		Heartbeat  int   `json:"heartbeat_seconds"`
	}
	if e := a.call(ctx, "/admit", a.Admission, &response); e != nil {
		return e
	}
	a.Identity = runs.Identity{WorkerID: a.Admission.WorkerID, Generation: response.Generation, Agent: a.Admission.Agent}
	return nil
}
func (a *Agent) Run(ctx context.Context) error {
	if e := a.Admit(ctx); e != nil {
		return e
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				var out struct {
					Contact  time.Time `json:"contact_received_at"`
					Controls []string  `json:"controls"`
					Paused   bool      `json:"paused"`
				}
				tick, e := a.Clock.Now()
				if e != nil {
					return
				}
				a.call(heartbeatCtx, "/heartbeat", map[string]any{"worker_id": a.Identity.WorkerID, "generation": a.Identity.Generation, "agent_instance": a.Identity.Agent, "request_id": runs.ID(), "agent_tick": tick.Milliseconds(), "phase": "preflight"}, &out)
			}
		}
	}()
	for ctx.Err() == nil {
		if e := a.RunOne(ctx); e != nil {
			return e
		}
	}
	return ctx.Err()
}
func (a *Agent) RunOne(ctx context.Context) error {
	anchor, e := a.Clock.Now()
	if e != nil {
		return e
	}
	poll := runs.Poll{Identity: a.Identity, RequestID: runs.ID(), Wait: 20}
	var out struct {
		Assignment *runs.Assignment `json:"assignment"`
	}
	if e = a.call(ctx, "/poll", poll, &out); e != nil {
		return e
	}
	if out.Assignment == nil {
		return nil
	}
	assignment := out.Assignment
	deadline := anchor + clock.Conservative(assignment.RemainingMS)
	now, e := a.Clock.Now()
	if e != nil || now >= deadline || assignment.NextSeq != 1 {
		return ErrAgent
	}
	if assignment.Phase != "preflight" || assignment.Spec.Phase != "preflight" || assignment.Spec.Version != 1 || assignment.Spec.Engine.ID != a.Admission.RuntimeID {
		return ErrAgent
	}
	b, _ := catalog.Canonical(assignment.Spec)
	if hash(b) != assignment.Digest {
		return ErrAgent
	}
	seq := int64(1)
	send := func(route string, payload any) error {
		raw, _ := json.Marshal(payload)
		m := runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: a.Identity, AttemptID: assignment.AttemptID, Seq: seq, Payload: raw}
		var response runs.Transition
		// Retransmit exactly the same message after an ambiguous response. A new
		// process cannot repeat RunOne over an unresolved assignment.
		e := a.call(ctx, "/attempts/"+assignment.AttemptID+"/"+route, m, &response)
		if e != nil && ctx.Err() == nil {
			e = a.call(ctx, "/attempts/"+assignment.AttemptID+"/"+route, m, &response)
		}
		if e != nil {
			return e
		}
		if !response.Accepted || response.NextSeq != seq+1 {
			return ErrAgent
		}
		seq++
		return nil
	}
	if e = send("ack", map[string]string{"work_item_id": assignment.WorkItemID}); e != nil {
		return e
	}
	if e = os.MkdirAll(a.AttemptDir, 0700); e != nil {
		return send("result", runs.Result{Outcome: "preparation_failed", Reason: "ATTEMPT_DIRECTORY_UNAVAILABLE", NoStateChild: true, Reaped: true})
	}
	dir, e := os.MkdirTemp(a.AttemptDir, "attempt-")
	if e != nil {
		return send("result", runs.Result{Outcome: "preparation_failed", Reason: "ATTEMPT_DIRECTORY_UNAVAILABLE", NoStateChild: true, Reaped: true})
	}
	stopped := true
	defer func() {
		if stopped {
			os.RemoveAll(dir)
		}
	}()
	a.mu.Lock()
	a.terms = nil
	a.redactor = &evidence.Registry{}
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.terms = nil; a.redactor = nil; a.mu.Unlock() }()
	repositories := map[string]string{assignment.Source.URL: assignment.Source.RepositoryID}
	credentialContext, stopHelper, e := a.serveCredentials(ctx, dir, *assignment, repositories)
	if e != nil {
		return send("result", runs.Result{Outcome: "preparation_failed", Reason: "CREDENTIAL_HELPER_UNAVAILABLE", NoStateChild: true, Reaped: true})
	}
	defer stopHelper()
	// Elapsed cap cancels preparation gracefully. It never forces a process kill
	// or releases ownership when process stop could not be proven.
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-attemptCtx.Done():
				return
			case <-ticker.C:
				n, e := a.Clock.Now()
				if e != nil || n >= deadline {
					cancel()
					return
				}
			}
		}
	}()
	tree := filepath.Join(dir, "source")
	e = a.Git.Checkout(attemptCtx, tree, assignment.Source.URL, assignment.Source.Commit, credentialContext)
	stage := "SOURCE_CHECKOUT_FAILED"
	var inspected source.Inspection
	if e == nil {
		stage = "MANIFEST_INVALID"
		raw, readErr := source.Read(tree, "cosmoformer.yaml")
		if readErr != nil {
			e = readErr
		} else {
			m, parseErr := catalog.Parse(raw)
			if parseErr != nil {
				e = parseErr
			} else {
				stage = "DECLARATION_MISMATCH"
				var selected *catalog.Stack
				for _, s := range m.Stacks {
					if matches(s, assignment.Spec) {
						if selected != nil {
							e = source.ErrSource
							break
						}
						copy := s
						selected = &copy
					}
				}
				if selected == nil {
					e = source.ErrSource
				}
				if e == nil {
					stage = "SOURCE_INSPECTION_FAILED"
					cache := map[string]string{}
					resolver := func(m source.ModuleSource) (string, error) {
						key := m.Repository + "@" + m.Commit
						if p, ok := cache[key]; ok {
							return p, nil
						}
						approvedID := ""
						for _, grant := range assignment.Spec.Grants {
							if grant.Kind != "repository" {
								continue
							}
							var response struct {
								Username string `json:"username"`
								Token    string `json:"token"`
								Revision int64  `json:"credential_revision"`
							}
							body := map[string]any{"request_id": runs.ID(), "worker_id": a.Identity.WorkerID, "generation": a.Identity.Generation, "agent_instance": a.Identity.Agent, "repository_id": grant.ResourceID, "canonical_https_url": m.Repository}
							if a.call(attemptCtx, "/attempts/"+assignment.AttemptID+"/source-credential", body, &response) == nil {
								if !a.register(response.Username, response.Token) {
									return "", source.ErrSource
								}
								approvedID = grant.ResourceID
								break
							}
						}
						if approvedID == "" {
							return "", source.ErrSource
						}
						a.mu.Lock()
						repositories[m.Repository] = approvedID
						updated, _ := json.Marshal(helperContext{Socket: filepath.Join(dir, "git.sock"), AttemptID: assignment.AttemptID, Identity: a.Identity, Repositories: repositories})
						writeErr := os.WriteFile(credentialContext+".new", updated, 0600)
						if writeErr == nil {
							writeErr = os.Rename(credentialContext+".new", credentialContext)
						}
						a.mu.Unlock()
						if writeErr != nil {
							return "", source.ErrSource
						}
						p := filepath.Join(dir, "module-"+hash([]byte(key)))
						if err := a.Git.Checkout(attemptCtx, p, m.Repository, m.Commit, credentialContext); err != nil {
							return "", err
						}
						cache[key] = p
						return p, nil
					}
					tables := []string{""}
					for _, grant := range assignment.Spec.Grants {
						if grant.Kind == "ddb_table" {
							tables = append(tables, grant.Namespace)
						}
					}
					for _, table := range tables {
						inspected, e = source.InspectResolved(tree, *selected, table, resolver)
						if e == nil || errors.Is(e, source.ErrProcess) {
							break
						}
					}
				}
			}
		}
	}
	if e != nil {
		if errors.Is(e, source.ErrProcess) {
			stopped = false
			return ErrAgent
		}
		return send("result", runs.Result{Outcome: "preparation_failed", Reason: stage, NoStateChild: true, Reaped: true})
	}
	safe, _ := json.Marshal(inspected)
	a.mu.Lock()
	leak := false
	for _, term := range a.terms {
		if bytes.Contains(safe, []byte(term)) {
			leak = true
		}
	}
	a.mu.Unlock()
	if leak {
		return send("result", runs.Result{Outcome: "preparation_failed", Reason: stage, NoStateChild: true, Reaped: true})
	}
	if e = send("prepare-result", runs.Prepare{Phase: "preflight", Result: "success", Digest: inspected.Digest, Backend: inspected.Backend, Modules: inspected.Modules, NoStateChild: true, Reaped: true}); e != nil {
		return e
	}
	return send("result", runs.Result{Outcome: "success", Digest: inspected.Digest, NoStateChild: true, Reaped: true})
}
func matches(s catalog.Stack, p runs.PreflightSpec) bool {
	if s.Root != p.Source.Root || s.Workspace != p.Workspace || s.BackendOwnerAccountID != p.BackendOwnerAccountID || s.Engine.Name != p.Engine.Name || s.Engine.Version != p.Engine.Version || s.Worker.ID != p.Pool.ID || s.TrackedBranch == "" {
		return false
	}
	equal := func(a, b any) bool {
		x, _ := catalog.Canonical(a)
		y, _ := catalog.Canonical(b)
		return bytes.Equal(x, y)
	}
	if !equal(s.VarFiles, p.VarFiles) || !equal(s.BackendFiles, p.BackendFiles) || len(s.Secrets) != len(p.Secrets) {
		return false
	}
	for _, v := range s.Secrets {
		found := false
		for _, ref := range p.Secrets {
			if ref.ID == v.Source.ID && ref.Scope == v.Source.Scope && ref.Destination == v.Env {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type helperContext struct {
	Socket       string            `json:"socket"`
	AttemptID    string            `json:"attempt_id"`
	Identity     runs.Identity     `json:"identity"`
	Repositories map[string]string `json:"repositories"`
}
type helperRequest struct {
	AttemptID string        `json:"attempt_id"`
	Identity  runs.Identity `json:"identity"`
	URL       string        `json:"url"`
}
type helperReply struct {
	Username string `json:"username"`
	Token    string `json:"token"`
}

func (a *Agent) serveCredentials(ctx context.Context, dir string, assignment runs.Assignment, repositories map[string]string) (string, func(), error) {
	socket := filepath.Join(dir, "git.sock")
	listener, e := net.Listen("unix", socket)
	if e != nil {
		return "", nil, ErrAgent
	}
	if os.Chmod(socket, 0600) != nil {
		listener.Close()
		return "", nil, ErrAgent
	}
	hc := helperContext{socket, assignment.AttemptID, a.Identity, repositories}
	file := filepath.Join(dir, "git-context.json")
	b, _ := json.Marshal(hc)
	if os.WriteFile(file, b, 0600) != nil {
		listener.Close()
		return "", nil, ErrAgent
	}
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				var req helperRequest
				if runs.Decode(readBounded(conn, 16384), &req) != nil || req.AttemptID != hc.AttemptID || req.Identity != hc.Identity {
					return
				}
				a.mu.Lock()
				repo, ok := hc.Repositories[req.URL]
				a.mu.Unlock()
				if !ok {
					return
				}
				var response struct {
					Username string `json:"username"`
					Token    string `json:"token"`
					Revision int64  `json:"credential_revision"`
				}
				body := map[string]any{"request_id": runs.ID(), "worker_id": a.Identity.WorkerID, "generation": a.Identity.Generation, "agent_instance": a.Identity.Agent, "repository_id": repo, "canonical_https_url": req.URL}
				if a.call(ctx, "/attempts/"+assignment.AttemptID+"/source-credential", body, &response) != nil {
					return
				}
				if !a.register(response.Username, response.Token) {
					return
				}
				json.NewEncoder(conn).Encode(helperReply{response.Username, response.Token})
			}()
		}
	}()
	return file, func() { listener.Close(); os.Remove(file) }, nil
}
func readBounded(r io.Reader, n int64) []byte {
	scanner := bufio.NewReader(io.LimitReader(r, n+1))
	b, e := scanner.ReadBytes('\n')
	if e != nil || int64(len(b)) > n {
		return nil
	}
	return b
}
func (a *Agent) register(values ...string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	terms := append([]string{}, a.terms...)
	for _, v := range values {
		if v != "" {
			if strings.ContainsAny(v, "\r\n\x00") {
				return false
			}
			terms = append(terms, v)
		}
	}
	total := 0
	for _, v := range terms {
		total += len(v)
	}
	if len(terms) > 4096 || total > 262144 {
		return false
	}
	if a.redactor == nil {
		a.redactor = &evidence.Registry{}
	}
	nonempty := []string{}
	for _, v := range values {
		if v != "" {
			nonempty = append(nonempty, v)
		}
	}
	if a.redactor.Register(nonempty...) != nil {
		return false
	}
	a.terms = terms
	return true
}

// CredentialHelper has no platform HTTP client or bearer-file access. Only the
// already supervising agent may retrieve and register credential bytes.
func CredentialHelper(operation, contextFile string, in io.Reader, out io.Writer) error {
	if operation != "get" {
		if operation == "store" || operation == "erase" {
			return nil
		}
		return ErrAgent
	}
	f, e := os.Open(contextFile)
	if e != nil {
		return ErrAgent
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 16384 {
		return ErrAgent
	}
	var hc helperContext
	if json.NewDecoder(f).Decode(&hc) != nil {
		return ErrAgent
	}
	fields := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(in, 16385))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || fields[k] != "" {
			return ErrAgent
		}
		fields[k] = v
	}
	if scanner.Err() != nil || fields["protocol"] != "https" || fields["host"] != "bitbucket.org" {
		return ErrAgent
	}
	url := "https://bitbucket.org/" + fields["path"]
	if _, ok := hc.Repositories[url]; !ok {
		return ErrAgent
	}
	conn, e := net.DialTimeout("unix", hc.Socket, 5*time.Second)
	if e != nil {
		return ErrAgent
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if json.NewEncoder(conn).Encode(helperRequest{hc.AttemptID, hc.Identity, url}) != nil {
		return ErrAgent
	}
	var response helperReply
	if runs.Decode(readBounded(conn, 65536), &response) != nil {
		return ErrAgent
	}
	if strings.ContainsAny(response.Username+response.Token, "\r\n\x00") {
		return ErrAgent
	}
	if response.Username == "" || response.Token == "" {
		return nil
	}
	_, e = io.WriteString(out, "username="+response.Username+"\npassword="+response.Token+"\n\n")
	return e
}
