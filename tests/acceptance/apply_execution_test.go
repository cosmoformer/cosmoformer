//go:build integration

package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/worker"
)

// This journey uses real request, scheduling, restoration, custody and result
// boundaries. Native/AWS I/O is injected here; pinned engine checks are separate.
func TestSavedPlanApplyWorkerJourney(t *testing.T) {
	for _, scenario := range []string{"absent lock", "retained lock", "session expired", "grant after spawn", "native failure", "unknown stop", "manifest mismatch", "missing binary", "source before init", "source after init", "approval after init", "cancel after init", "expired init", "expired apply", "preparation retry", "result response lost", "final evidence unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPlanningFixture(t)
			f.ticks.mu.Lock()
			f.ticks.start = time.Now()
			f.ticks.mu.Unlock()
			receipt := f.requestNative(t)
			ctx := context.Background()
			if e := f.agent.RunOne(ctx); e != nil {
				t.Fatal(e)
			}
			calls := []string{}
			applying := false
			f.agent.Native = func(ctx context.Context, in worker.NativeCommand, spawn func(runs.CommandEvent) error) (runs.CommandEvent, error) {
				if e := in.Start(func() error { return nil }); e != nil {
					return runs.CommandEvent{}, e
				}
				command := in.Descriptor.Command
				calls = append(calls, command)
				if applying && command == "init" {
					readonly := false
					for _, arg := range in.Args {
						readonly = readonly || arg == "-lockfile=readonly"
					}
					if readonly != (scenario == "retained lock") {
						t.Fatal("wrong readonly-lock behavior", in.Args)
					}
				}
				if command == "apply" {
					if len(in.Args) != 6 || in.Args[0] != "apply" || in.Args[3] != "-lock=true" || in.Args[4] != "-lock-timeout=60s" {
						t.Fatal("not fixed saved-plan argv", in.Args)
					}
					raw, e := os.ReadFile(in.Args[5])
					if e != nil || string(raw) != "synthetic protected saved plan" {
						t.Fatal("saved binary not restored", e)
					}
				}
				pid := int64(41000 + in.Descriptor.Ordinal)
				event := runs.CommandEvent{Ordinal: in.Permission.Ordinal, Permission: in.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
				if e := spawn(event); e != nil {
					return event, e
				}
				if applying && command == "apply" {
					// Duplicate accepted spawn facts cannot increment the epoch twice.
					if e := spawn(event); e != nil {
						return event, e
					}
					var epoch int
					if e := f.db.Owner.QueryRow(ctx, `SELECT state_epoch FROM cosmoformer.backend_targets`).Scan(&epoch); e != nil || epoch != 1 {
						t.Fatal("epoch not advanced exactly once", epoch, e)
					}
					if scenario == "grant after spawn" {
						f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp() WHERE role='deployer'`)
					}
					if scenario == "final evidence unavailable" {
						f.exec(t, `UPDATE cosmoformer.artifacts SET state='unavailable' WHERE id=(SELECT sealed_manifest_artifact_id FROM cosmoformer.attempts WHERE closed_at IS NULL AND target_id IS NOT NULL)`)
					}
					if scenario == "unknown stop" {
						return event, errors.New("synthetic unresolved descendants")
					}
				}
				switch command {
				case "init":
					if applying {
						if scenario == "source after init" {
							f.remote.mu.Lock()
							f.remote.commit = strings.Repeat("b", 40)
							f.remote.mu.Unlock()
						}
						if scenario == "approval after init" {
							f.admin.call("DELETE", "/organizations/"+f.org+"/plans/"+planID(t, f, receipt)+"/approvals/me", map[string]any{"expected_plan_revision": 2}, 200)
						}
						if scenario == "cancel after init" {
							var revision int64
							var attempt string
							if e := f.db.Owner.QueryRow(ctx, `SELECT id::text,revision FROM cosmoformer.attempts WHERE closed_at IS NULL AND target_id IS NOT NULL`).Scan(&attempt, &revision); e != nil {
								t.Fatal(e)
							}
							f.admin.call("POST", "/organizations/"+f.org+"/attempts/"+attempt+"/cancel", map[string]any{"expected_attempt_revision": revision}, 202)
						}
					}
					if scenario == "retained lock" && !applying {
						if e := os.WriteFile(filepath.Join(in.Dir, ".terraform.lock.hcl"), []byte("# exact native empty-provider lock\n"), 0600); e != nil {
							t.Fatal(e)
						}
					}
				case "schema":
					in.Output.WriteString(`{"format_version":"1.0"}`)
				case "plan":
					for _, arg := range in.Args {
						if strings.HasPrefix(arg, "-out=") {
							os.WriteFile(strings.TrimPrefix(arg, "-out="), []byte("synthetic protected saved plan"), 0600)
						}
					}
				case "show":
					json.NewEncoder(in.Output).Encode(map[string]any{"format_version": "1.2", "terraform_version": "1.13.8", "planned_values": map[string]any{}, "configuration": map[string]any{}})
				}
				exit, empty := 0, true
				if applying && command == "apply" && scenario == "native failure" {
					exit = 1
				}
				event.Event = "stopped"
				event.Exit = &exit
				event.GroupEmpty = &empty
				event.Pipes = "drained"
				return event, nil
			}
			if e := f.agent.RunOne(ctx); e != nil {
				t.Fatal("planning", e)
			}
			run := f.admin.call("GET", "/organizations/"+f.org+"/runs/"+receipt["subject_id"].(string), nil, 200)["run"].(map[string]any)
			p := run["plan"].(map[string]any)
			base := "/organizations/" + f.org + "/plans/" + p["id"].(string)
			for _, role := range []string{"approver", "deployer"} {
				f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,$1 FROM cosmoformer.grants WHERE role='planner' LIMIT 1`, role)
			}
			body := map[string]any{"expected_plan_revision": p["revision"], "manifest_digest": p["manifest_digest"]}
			f.admin.call("POST", base+"/approvals", body, 200)
			applyBody := map[string]any{"expected_plan_revision": p["revision"], "manifest_digest": p["manifest_digest"], "priority": "medium", "confirmed_context": map[string]any{"stack_id": f.stack, "environment_id": run["environment_id"], "source_commit": run["commit"]}}
			accepted := f.admin.call("POST", base+"/apply", applyBody, 202)
			applying = true
			calls = nil
			if scenario == "result response lost" {
				original := f.agent.HTTP.Transport
				dropped := false
				f.agent.HTTP.Transport = planningTransport(func(r *http.Request) (*http.Response, error) {
					res, e := original.RoundTrip(r)
					if e == nil && res.StatusCode == 200 && strings.HasSuffix(r.URL.Path, "/result") && !dropped {
						dropped = true
						res.Body.Close()
						return nil, errors.New("synthetic result response loss after commit")
					}
					return res, e
				})
			}
			originalAccess := f.agent.Access
			if scenario == "preparation retry" {
				f.agent.Access = func(string, runs.Assignment, runs.Identity, time.Duration, *evidence.Registry) (worker.ExecutionAccess, error) {
					return nil, errors.New("synthetic temporary preparation failure")
				}
			}
			if strings.HasPrefix(scenario, "expired ") {
				workerClock := &testClock{tick: time.Second, start: time.Now()}
				f.agent.Clock = workerClock
				baseTransport := f.agent.HTTP.Transport
				f.agent.HTTP.Transport = planningTransport(func(r *http.Request) (*http.Response, error) {
					res, e := baseTransport.RoundTrip(r)
					if e == nil && res.StatusCode == 200 && strings.HasSuffix(r.URL.Path, "/start-permission") {
						body, _ := r.GetBody()
						defer body.Close()
						var m runs.Message
						json.NewDecoder(body).Decode(&m)
						var request runs.CommandRequest
						json.Unmarshal(m.Payload, &request)
						if request.Command == strings.TrimPrefix(scenario, "expired ") {
							workerClock.mu.Lock()
							workerClock.tick += 20 * time.Second
							workerClock.mu.Unlock()
						}
					}
					return res, e
				})
			}
			switch scenario {
			case "source before init":
				f.remote.mu.Lock()
				f.remote.commit = strings.Repeat("b", 40)
				f.remote.mu.Unlock()
			case "session expired":
				f.exec(t, `UPDATE cosmoformer.sessions SET absolute_expires_at=clock_timestamp()`)
			case "manifest mismatch":
				original := f.agent.Git.Run
				f.agent.Git.Run = func(c context.Context, dir string, args, env []string) ([]byte, error) {
					b, e := original(c, dir, args, env)
					if e == nil && strings.Contains(strings.Join(args, " "), "checkout --quiet --detach") {
						e = os.WriteFile(filepath.Join(dir, "unexpected.txt"), []byte("changed fresh source"), 0600)
					}
					return b, e
				}
			case "missing binary":
				var key string
				if e := f.db.Owner.QueryRow(ctx, `SELECT object_key FROM cosmoformer.artifacts WHERE plan_id=$1 AND kind='binary_plan'`, p["id"]).Scan(&key); e != nil {
					t.Fatal(e)
				}
				f.storage.mu.Lock()
				delete(f.storage.objects, key)
				f.storage.mu.Unlock()
			}
			if e := f.agent.RunOne(ctx); e != nil && scenario != "unknown stop" {
				t.Fatal("apply", e)
			}
			var status string
			var reservations, epoch int
			if e := f.db.Owner.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM cosmoformer.backend_reservations),(SELECT state_epoch FROM cosmoformer.backend_targets) FROM cosmoformer.operations WHERE id=$1`, accepted["operation_id"]).Scan(&status, &reservations, &epoch); e != nil {
				t.Fatal(e)
			}
			switch scenario {
			case "unknown stop":
				if status != "needs_attention" || reservations != 1 || epoch < 1 {
					t.Fatal(status, reservations, epoch)
				}
				runPath := "/organizations/" + f.org + "/runs/" + receipt["subject_id"].(string)
				before := f.admin.call("GET", runPath, nil, 200)["recovery"].(map[string]any)
				recoveryPath := "/organizations/" + f.org + "/recoveries/" + before["id"].(string)
				r := f.admin.call("PATCH", recoveryPath+"/evidence", recoveryBody(before["revision"], "host", "protected-apply-stop", true), 200)
				vote := map[string]any{"expected_evidence_revision": r["evidence_revision"]}
				f.admin.call("POST", recoveryPath+"/approvals", vote, 200)
				f.admin.call("POST", recoveryPath+"/clear", vote, 200)
				after := f.admin.call("GET", runPath, nil, 200)["recovery"].(map[string]any)
				if after["id"] != before["id"] || after["status"] != "cleared" || after["evidence"] == nil {
					t.Fatal("cleared Apply recovery disappeared from original run", after)
				}
			case "native failure", "final evidence unavailable":
				if status != "failed" || reservations != 0 || epoch != 1 {
					t.Fatal(status, reservations, epoch)
				}
			case "missing binary", "manifest mismatch", "source before init", "source after init", "approval after init", "preparation retry", "expired init", "expired apply", "cancel after init":
				wantStatus := "failed"
				if strings.HasPrefix(scenario, "expired ") {
					wantStatus = "retired"
				}
				if scenario == "cancel after init" {
					wantStatus = "cancelled"
				}
				if status != wantStatus || reservations != 0 || epoch != 0 {
					t.Fatal(status, reservations, epoch)
				}
				for _, c := range calls {
					if c == "apply" {
						t.Fatal("rejected restoration applied")
					}
				}
			default:
				if status != "succeeded" || reservations != 0 || epoch != 1 || strings.Join(calls, ",") != "init,apply" {
					t.Fatal(status, reservations, epoch, calls)
				}
			}
			if scenario == "absent lock" {
				f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'raw_evidence_read' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
				var preInit string
				if err := f.db.Owner.QueryRow(ctx, `SELECT pre_init_spec_artifact_id::text FROM cosmoformer.attempts WHERE operation_id=$1 AND pre_init_spec_artifact_id IS NOT NULL`, accepted["operation_id"]).Scan(&preInit); err != nil {
					t.Fatal(err)
				}
				runPath := "/organizations/" + f.org + "/runs/" + receipt["subject_id"].(string)
				page := f.admin.call("GET", runPath, nil, 200)
				found := false
				for _, item := range page["raw_artifacts"].([]any) {
					found = found || item.(map[string]any)["id"] == preInit
				}
				if !found {
					t.Fatal("Apply attestation omitted from run evidence", page)
				}
				// A cursor from the Apply operation must page through the same
				// run's earlier planning evidence without changing its scope.
				page = f.admin.call("GET", runPath+"?artifact_before="+preInit, nil, 200)
				if len(page["raw_artifacts"].([]any)) == 0 {
					t.Fatal("Apply evidence cursor lost original plan evidence", page)
				}
			}
			if scenario == "preparation retry" {
				f.admin.call("POST", base+"/apply", applyBody, 409)
				var revision int64
				if e := f.db.Owner.QueryRow(ctx, `SELECT revision FROM cosmoformer.operations WHERE id=$1`, accepted["operation_id"]).Scan(&revision); e != nil {
					t.Fatal(e)
				}
				f.agent.Access = originalAccess
				f.admin.call("POST", "/organizations/"+f.org+"/operations/"+accepted["operation_id"].(string)+"/retry", map[string]any{"expected_operation_revision": revision}, 200)
				if e := f.agent.RunOne(ctx); e != nil {
					t.Fatal("explicit retry", e)
				}
				if e := f.db.Owner.QueryRow(ctx, `SELECT status FROM cosmoformer.operations WHERE id=$1`, accepted["operation_id"]).Scan(&status); e != nil || status != "succeeded" {
					t.Fatal(status, e)
				}
			}
			if strings.HasPrefix(scenario, "expired ") {
				next := f.admin.call("POST", base+"/apply", applyBody, 202)
				if next["operation_id"] == accepted["operation_id"] {
					t.Fatal("expired authority resumed")
				}
				var priority string
				if e := f.db.Owner.QueryRow(ctx, `SELECT priority FROM cosmoformer.operations WHERE id=$1`, next["operation_id"]).Scan(&priority); e != nil || priority != "medium" {
					t.Fatal(priority, e)
				}
			}

		})
	}
}

func planID(t *testing.T, f *preflightFixture, receipt map[string]any) string {
	t.Helper()
	var id string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.plans WHERE operation_id=$1`, receipt["operation_id"]).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return id
}
