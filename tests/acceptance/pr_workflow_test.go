//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/worker"
)

// Connected local journey: signed ingress -> authoritative PR reconciliation ->
// native evidence -> local exact-plan vote/Apply -> stable feedback -> actual
// merged branch -> independent normal plan and approvals. No external I/O.
func TestConnectedPRDeploymentAndFreshMergedPlan(t *testing.T) {
	for _, remaining := range []bool{false, true} {
		t.Run(map[bool]string{false: "no changes", true: "remaining changes"}[remaining], func(t *testing.T) {
			f := newPreflightFixtureForSourcePolicyClock(t, nil, map[string]any{"kind": "branch"}, nil, true)
			f.enablePlanning(t)
			ctx := context.Background()
			f.ticks.mu.Lock()
			f.ticks.start = time.Now()
			f.ticks.mu.Unlock()
			currentSource := ""
			originalGit := f.agent.Git.Run
			f.agent.Git.Run = func(ctx context.Context, dir string, args, env []string) ([]byte, error) {
				for _, arg := range args {
					if arg == "fetch" {
						currentSource = args[len(args)-1]
					}
				}
				if strings.Contains(strings.Join(args, " "), "rev-parse") {
					return []byte(currentSource + "\n"), nil
				}
				return originalGit(ctx, dir, args, env)
			}
			phase := "baseline"
			applies := 0
			plans := 0
			f.agent.Native = func(ctx context.Context, in worker.NativeCommand, spawn func(runs.CommandEvent) error) (runs.CommandEvent, error) {
				if e := in.Start(func() error { return nil }); e != nil {
					return runs.CommandEvent{}, e
				}
				pid := int64(51000 + in.Descriptor.Ordinal)
				event := runs.CommandEvent{Ordinal: in.Permission.Ordinal, Permission: in.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
				if e := spawn(event); e != nil {
					return event, e
				}
				switch in.Descriptor.Command {
				case "schema":
					io.WriteString(in.Output, `{"format_version":"1.0"}`)
				case "plan":
					plans++
					for _, arg := range in.Args {
						if strings.HasPrefix(arg, "-out=") {
							if e := os.WriteFile(strings.TrimPrefix(arg, "-out="), []byte("protected binary for "+currentSource), 0600); e != nil {
								t.Fatal(e)
							}
						}
					}
				case "show":
					raw := map[string]any{"format_version": "1.2", "terraform_version": "1.13.8", "planned_values": map[string]any{}, "configuration": map[string]any{}}
					if phase == "pr" || phase == "merged" && remaining {
						raw["output_changes"] = map[string]any{"result": map[string]any{"actions": []string{"create"}, "before": nil, "after": "synthetic value", "after_unknown": false, "before_sensitive": false, "after_sensitive": false}}
					}
					json.NewEncoder(in.Output).Encode(raw)
				case "apply":
					applies++
					b, e := os.ReadFile(in.Args[len(in.Args)-1])
					if e != nil || string(b) != "protected binary for "+strings.Repeat("b", 40) {
						t.Fatal("Apply did not restore exact PR binary", e)
					}
				}
				exit, empty := 0, true
				event.Event = "stopped"
				event.Exit = &exit
				event.GroupEmpty = &empty
				event.Pipes = "drained"
				return event, nil
			}
			baseline := f.requestNative(t)
			for i := 0; i < 2; i++ {
				if e := f.agent.RunOne(ctx); e != nil {
					t.Fatal(e)
				}
			}
			_ = baseline
			var integration string
			if e := f.db.Owner.QueryRow(ctx, `SELECT integration_id::text FROM cosmoformer.repositories WHERE id=$1`, f.repo).Scan(&integration); e != nil {
				t.Fatal(e)
			}
			cipher, _ := cryptostore.New(bytes.Repeat([]byte{42}, 32))
			secret := strings.Repeat("w", 32)
			credentials, _ := json.Marshal(map[string]any{"client_id": "synthetic", "client_secret": "synthetic", "webhook_secret": secret, "tokens": bitbucket.Tokens{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh"}, "expires_at": time.Now().Add(time.Hour)})
			sealed, e := cipher.Seal(credentials, cryptostore.AAD(f.org, integration, "integration", 1), func(fp [32]byte) error {
				_, e := f.db.Owner.Exec(ctx, `SELECT cosmoformer.reserve_encryptions($1,1)`, fp[:])
				return e
			})
			if e != nil {
				t.Fatal(e)
			}
			f.exec(t, `UPDATE cosmoformer.integrations SET encrypted_credentials=$2,hook_uuid='fixture-hook' WHERE id=$1`, integration, sealed)
			var feedbackMu sync.Mutex
			comment := ""
			creates := 0
			lost := false
			bb := bitbucket.New(&http.Client{Transport: planningTransport(func(r *http.Request) (*http.Response, error) {
				if !strings.Contains(r.URL.Path, "/comments") {
					return f.remote.RoundTrip(r)
				}
				feedbackMu.Lock()
				defer feedbackMu.Unlock()
				raw := `{"values":[]}`
				if r.Method == "GET" && comment != "" {
					b, _ := json.Marshal(map[string]any{"values": []any{map[string]any{"id": 17, "content": map[string]any{"raw": comment}}}})
					raw = string(b)
				}
				if r.Method != "GET" {
					var b struct {
						Content struct {
							Raw string `json:"raw"`
						} `json:"content"`
					}
					json.NewDecoder(r.Body).Decode(&b)
					comment = b.Content.Raw
					if r.Method == "POST" {
						creates++
					}
					if !lost {
						lost = true
						return nil, errors.New("synthetic lost create response")
					}
					raw = `{"id":17}`
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
			})})
			bg, cancel := context.WithCancel(ctx)
			defer cancel()
			handler, _ := httpapi.NewWithOptions(f.db.App, origin, httpapi.Options{Clock: f.ticks, Cipher: cipher, Bitbucket: bb, Evidence: &evidence.Custody{Storage: f.storage, Bucket: "synthetic"}, Background: bg})
			browserServer := httptest.NewServer(handler)
			defer browserServer.Close()
			f.admin.base = browserServer.URL
			f.admin.handler = handler
			workerServer := httptest.NewTLSServer(handler)
			defer workerServer.Close()
			f.agent.Origin = workerServer.URL
			f.agent.HTTP = workerServer.Client()
			f.exec(t, `UPDATE cosmoformer.workers SET last_contact_at=clock_timestamp()-interval '60 seconds'`)
			f.agent.Admission.Agent = runs.ID()
			if e = f.agent.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			hook := func(event string) {
				raw := []byte(`{"repository":{"uuid":"{fictional-repository}"},"pullrequest":{"id":7}}`)
				sum := hmac.New(sha256.New, []byte(secret))
				sum.Write(raw)
				req := httptest.NewRequest("POST", "/webhooks/bitbucket/"+f.org+"/"+integration, bytes.NewReader(raw))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Hub-Signature", "sha256="+hex.EncodeToString(sum.Sum(nil)))
				req.Header.Set("X-Hook-UUID", "fixture-hook")
				req.Header.Set("X-Request-UUID", runs.ID())
				req.Header.Set("X-Event-Key", event)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != 200 {
					t.Fatal(response.Code, response.Body.String())
				}
			}
			wait := func(check func() bool) {
				t.Helper()
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) {
					if check() {
						return
					}
					time.Sleep(30 * time.Millisecond)
				}
				t.Fatal("local workflow did not settle")
			}
			phase = "pr"
			f.remote.mu.Lock()
			f.remote.prCommit = strings.Repeat("b", 40)
			f.remote.mu.Unlock()
			hook("pullrequest:created")
			var prRun string
			wait(func() bool {
				return f.db.Owner.QueryRow(ctx, `SELECT id::text FROM cosmoformer.runs WHERE purpose='pr_preview' AND source_commit=$1`, strings.Repeat("b", 40)).Scan(&prRun) == nil
			})
			for i := 0; i < 2; i++ {
				if e = f.agent.RunOne(ctx); e != nil {
					t.Fatal(e)
				}
			}
			base := "/organizations/" + f.org
			review := f.admin.call("GET", base+"/runs/"+prRun, nil, 200)["run"].(map[string]any)
			prPlan := review["plan"].(map[string]any)
			f.admin.call("GET", base+"/runs/"+prRun+"/changes?plan_id="+prPlan["id"].(string)+"&revision=2", nil, 200)
			for _, role := range []string{"approver", "deployer"} {
				f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,$1 FROM cosmoformer.grants WHERE role='planner' LIMIT 1`, role)
			}
			planPath := base + "/plans/" + prPlan["id"].(string)
			f.admin.call("POST", planPath+"/approvals", map[string]any{"expected_plan_revision": prPlan["revision"], "manifest_digest": prPlan["manifest_digest"]}, 200)
			applyBody := map[string]any{"expected_plan_revision": prPlan["revision"], "manifest_digest": prPlan["manifest_digest"], "priority": "medium", "confirmed_context": map[string]any{"stack_id": f.stack, "environment_id": review["environment_id"], "source_commit": review["commit"]}}
			f.admin.call("POST", planPath+"/apply", applyBody, 202)
			if e = f.agent.RunOne(ctx); e != nil {
				t.Fatal(e)
			}
			wait(func() bool {
				feedbackMu.Lock()
				defer feedbackMu.Unlock()
				return strings.Contains(comment, "Deployment: succeeded")
			})
			phase = "merged"
			f.remote.mu.Lock()
			f.remote.prState = "MERGED"
			f.remote.commit = strings.Repeat("c", 40)
			f.remote.mu.Unlock()
			hook("pullrequest:fulfilled")
			var mergedRun string
			wait(func() bool {
				return f.db.Owner.QueryRow(ctx, `SELECT id::text FROM cosmoformer.runs WHERE purpose='tracked' AND source_commit=$1`, strings.Repeat("c", 40)).Scan(&mergedRun) == nil
			})
			for i := 0; i < 2; i++ {
				if e = f.agent.RunOne(ctx); e != nil {
					t.Fatal(e)
				}
			}
			tracked := f.admin.call("GET", base+"/runs/"+mergedRun, nil, 200)["run"].(map[string]any)
			mergedPlan := tracked["plan"].(map[string]any)
			if mergedPlan["id"] == prPlan["id"] || mergedPlan["manifest_digest"] == prPlan["manifest_digest"] || tracked["commit"] == review["commit"] {
				t.Fatal("merge reused PR evidence")
			}
			summary := f.admin.call("GET", base+"/plans/"+mergedPlan["id"].(string)+"/approvals", nil, 200)
			if summary["counted_approvals"].(float64) != 0 {
				t.Fatal("PR approvals transferred", summary)
			}
			want := "NO_CHANGES"
			if remaining {
				want = "PLAN_READY"
			}
			if tracked["reason"] != want {
				t.Fatal("untruthful merged plan outcome", tracked)
			}
			wait(func() bool {
				feedbackMu.Lock()
				defer feedbackMu.Unlock()
				return strings.Contains(comment, "Fresh tracked plan:") && strings.Contains(comment, strings.Repeat("c", 40))
			})
			feedbackMu.Lock()
			if creates != 1 || !lost {
				t.Error("lost feedback duplicated comment", creates)
			}
			feedbackMu.Unlock()
			var applyCount int
			f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.operations WHERE kind='apply'`).Scan(&applyCount)
			if applies != 1 || applyCount != 1 || plans != 3 {
				t.Fatal("feedback/merge repeated infrastructure execution", applies, applyCount, plans)
			}
			f.admin.call("GET", base+"/pull-requests?project_id="+review["project_id"].(string), nil, 200)
		})
	}
}
