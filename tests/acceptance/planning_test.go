//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/awsaccess"
	"github.com/cosmoformer/cosmoformer/internal/evidence"

	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/worker"
)

type planningAccess struct{ registry *evidence.Registry }

func (a *planningAccess) Prepare(context.Context) error {
	return a.registry.Register("synthetic-execution-only-secret")
}
func (a *planningAccess) RemainingMS() (int64, error) { return 3200000, nil }
func (a *planningAccess) Environment(context.Context, awsaccess.Binding) ([]string, error) {
	return []string{"AWS_ACCESS_KEY_ID=synthetic-access", "AWS_SECRET_ACCESS_KEY=synthetic-execution-only-secret", "AWS_SESSION_TOKEN=synthetic-session", "AWS_REGION=eu-west-1", "AWS_DEFAULT_REGION=eu-west-1"}, nil
}

func newPlanningFixture(t *testing.T) *preflightFixture {
	t.Helper()
	f := newPreflightFixture(t)
	ctx := context.Background()
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	f.exec(t, `UPDATE cosmoformer.stacks SET assessment='compatible' WHERE id=$1`, f.stack)
	f.exec(t, `UPDATE cosmoformer.aws_connections SET expected_bootstrap_principal='arn:aws:iam::111122223333:role/synthetic-bootstrap',session_seconds=3600,revision=revision+1`)
	// A fresh agent may add the implemented capability only after the old source
	// worker is positively idle. The fixture fences its old admission explicitly.
	f.exec(t, `UPDATE cosmoformer.workers SET last_contact_at=clock_timestamp()-interval '60 seconds' WHERE id=$1`, f.workerID)
	f.agent.Admission.Agent = runs.ID()
	f.agent.Admission.Phases = []string{"preflight", "execution"}
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	f.agent.ExpectedBootstrap = "arn:aws:iam::111122223333:role/synthetic-bootstrap"
	f.agent.Access = func(_ string, _ runs.Assignment, _ runs.Identity, _ time.Duration, reg *evidence.Registry) (worker.ExecutionAccess, error) {
		return &planningAccess{registry: reg}, nil
	}
	return f
}
func (f *preflightFixture) requestNative(t *testing.T) map[string]any {
	t.Helper()
	return f.admin.call("POST", "/organizations/"+f.org+"/stacks/"+f.stack+"/plans", map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium", "intent": "native_plan"}, 200)
}
func TestNativePlanningJourneys(t *testing.T) {
	for _, scenario := range []string{"no changes", "output only", "mutated init", "schema rejected", "nonzero exit", "process uncertain", "source advanced", "grant revoked", "missing evidence", "permission response lost", "expired launch", "expired at spawn", "cancel seal", "cancel finalization"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPlanningFixture(t)
			if strings.HasPrefix(scenario, "cancel ") {
				f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'deployer' FROM cosmoformer.grants WHERE role='planner' LIMIT 1`)
			}
			receipt := f.requestNative(t)
			ctx := context.Background()
			if e := f.agent.RunOne(ctx); e != nil {
				t.Fatal("preflight", e)
			}
			calls := []string{}
			if scenario == "permission response lost" {
				f.agent.HTTP.Transport = lostPermissionResponse{base: f.wire.base}
			}
			if scenario == "expired launch" {
				f.agent.HTTP.Transport = planningTransport(func(r *http.Request) (*http.Response, error) {
					res, e := f.wire.base.RoundTrip(r)
					if strings.HasSuffix(r.URL.Path, "/start-permission") && e == nil && res.StatusCode == 200 {
						f.ticks.mu.Lock()
						f.ticks.tick += 20 * time.Second
						f.ticks.mu.Unlock()
					}
					return res, e
				})
			}
			f.agent.Native = func(ctx context.Context, in worker.NativeCommand, spawn func(runs.CommandEvent) error) (runs.CommandEvent, error) {
				if scenario == "expired at spawn" {
					f.ticks.mu.Lock()
					f.ticks.tick += 20 * time.Second
					f.ticks.mu.Unlock()
				}
				if e := in.Start(func() error { return nil }); e != nil {
					return runs.CommandEvent{}, e
				}
				calls = append(calls, in.Descriptor.Command)
				if in.Program != "/usr/local/bin/tofu" || !filepath.IsAbs(in.Dir) {
					t.Fatal("unapproved native program or root")
				}
				for _, value := range in.Env {
					if strings.HasPrefix(value, "AWS_PROFILE=") || strings.HasPrefix(value, "TF_CLI_ARGS") || strings.Contains(value, "COSMOFORMER_TOKEN") {
						t.Fatal("inherited child environment")
					}
				}
				pid := int64(31000 + in.Descriptor.Ordinal)
				event := runs.CommandEvent{Ordinal: in.Permission.Ordinal, Permission: in.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
				if e := spawn(event); e != nil {
					return event, e
				}
				if scenario == "process uncertain" {
					return event, errors.New("synthetic unproven process")
				}
				exit := 0
				if in.Descriptor.Command == "plan" && scenario == "output only" {
					exit = 2
				}
				if scenario == "nonzero exit" && in.Descriptor.Command == "plan" {
					exit = 1
				}
				switch in.Descriptor.Command {
				case "init":
					if scenario == "cancel seal" {
						var attempt string
						var revision int64
						if e := f.db.Owner.QueryRow(ctx, `SELECT a.id::text,a.revision FROM cosmoformer.attempts a JOIN cosmoformer.work_items w ON w.id=a.work_item_id WHERE a.operation_id=$1 AND w.phase='execution'`, receipt["operation_id"]).Scan(&attempt, &revision); e != nil {
							t.Fatal(e)
						}
						f.admin.call("POST", "/organizations/"+f.org+"/attempts/"+attempt+"/cancel", map[string]any{"expected_attempt_revision": revision}, 202)
					}
					if scenario == "mutated init" {
						if e := os.WriteFile(filepath.Join(in.Dir, "main.tf"), []byte("mutated"), 0600); e != nil {
							t.Fatal(e)
						}
					}
				case "schema":
					schema := `{"format_version":"1.0"}`
					if scenario == "schema rejected" {
						schema = `{"format_version":"unknown"}`
					}
					if _, e := in.Output.WriteString(schema); e != nil {
						t.Fatal(e)
					}
				case "plan":
					for _, arg := range in.Args {
						if strings.HasPrefix(arg, "-out=") {
							if e := os.WriteFile(strings.TrimPrefix(arg, "-out="), []byte("synthetic protected saved plan"), 0600); e != nil {
								t.Fatal(e)
							}
						}
					}
				case "show":
					if scenario == "cancel finalization" {
						var attempt string
						var revision int64
						if e := f.db.Owner.QueryRow(ctx, `SELECT a.id::text,a.revision FROM cosmoformer.attempts a JOIN cosmoformer.work_items w ON w.id=a.work_item_id WHERE a.operation_id=$1 AND w.phase='execution'`, receipt["operation_id"]).Scan(&attempt, &revision); e != nil {
							t.Fatal(e)
						}
						f.admin.call("POST", "/organizations/"+f.org+"/attempts/"+attempt+"/cancel", map[string]any{"expected_attempt_revision": revision}, 202)
					}
					if scenario == "source advanced" {
						f.remote.mu.Lock()
						f.remote.commit = strings.Repeat("b", 40)
						f.remote.mu.Unlock()
					}
					if scenario == "grant revoked" {
						f.exec(t, `UPDATE cosmoformer.resource_grants SET enabled=false,revision=revision+1 WHERE kind='pool'`)
					}
					if scenario == "missing evidence" {
						var key string
						if e := f.db.Owner.QueryRow(ctx, `SELECT object_key FROM cosmoformer.artifacts WHERE kind='provider_schema'`).Scan(&key); e != nil {
							t.Fatal(e)
						}
						f.storage.mu.Lock()
						delete(f.storage.objects, key)
						f.storage.mu.Unlock()
					}
					raw := map[string]any{"format_version": "1.2", "terraform_version": "1.13.8", "planned_values": map[string]any{}, "configuration": map[string]any{}}
					if scenario == "output only" {
						raw["output_changes"] = map[string]any{"example": map[string]any{"actions": []string{"create"}, "before": nil, "after": "synthetic-execution-only-secret", "after_unknown": false, "before_sensitive": false, "after_sensitive": false}}
					}
					if e := json.NewEncoder(in.Output).Encode(raw); e != nil {
						t.Fatal(e)
					}
				}
				empty := true
				event.Event = "stopped"
				event.Exit = &exit
				event.GroupEmpty = &empty
				event.Pipes = "drained"
				return event, nil
			}
			e := f.agent.RunOne(ctx)
			if scenario == "process uncertain" || scenario == "permission response lost" {
				if e == nil {
					t.Fatal("uncertain process reported success")
				}
			} else if e != nil {
				t.Fatal("execution", e)
			}
			var status, reason string
			var workerHolds, backendHolds, ready int
			if e := f.db.Owner.QueryRow(ctx, `SELECT status,coalesce(terminal_reason,'') FROM cosmoformer.operations WHERE id=$1`, receipt["operation_id"]).Scan(&status, &reason); e != nil {
				t.Fatal(e)
			}
			if e := f.db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.worker_reservations),(SELECT count(*) FROM cosmoformer.backend_reservations),(SELECT count(*) FROM cosmoformer.plans WHERE status='ready')`).Scan(&workerHolds, &backendHolds, &ready); e != nil {
				t.Fatal(e)
			}
			if scenario == "process uncertain" || scenario == "permission response lost" {
				if scenario == "permission response lost" && len(calls) != 0 {
					t.Fatal("lost permission response launched a child")
				}
				if ready != 0 || workerHolds != 1 || backendHolds != 1 {
					t.Fatal("uncertainty released ownership", status, workerHolds, backendHolds)
				}
				return
			}
			if workerHolds != 0 || backendHolds != 0 {
				t.Fatal("positively stopped work retained temporary reservations", status)
			}
			if strings.HasPrefix(scenario, "cancel ") {
				if status != "cancelled" || reason != "CANCELLED" || ready != 0 {
					t.Fatal("cancelled finalization produced a deployable plan", status, reason, ready)
				}
				var plans int
				if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.plans WHERE operation_id=$1 AND status='unsupported'`, receipt["operation_id"]).Scan(&plans); e != nil || plans != 1 {
					t.Fatal("cancelled plan evidence was not retired", plans, e)
				}
				return
			}
			if scenario != "no changes" && scenario != "output only" {
				if status != "failed" || ready != 0 {
					t.Fatal("invalid evidence became ready", status, reason)
				}
				return
			}
			if status != "succeeded" || ready != 1 {
				t.Fatal("plan not ready", status, reason)
			}
			if strings.Join(calls, ",") != "init,schema,plan,show" {
				t.Fatal("wrong fixed sequence", calls)
			}
			want := "NO_CHANGES"
			if scenario == "output only" {
				want = "PLAN_READY"
			}
			if reason != want {
				t.Fatal("output-only changes misclassified", reason)
			}
			plans := f.admin.call("GET", "/organizations/"+f.org+"/plans", nil, 200)["plans"].([]any)
			p := plans[0].(map[string]any)
			changes := f.admin.call("GET", "/organizations/"+f.org+"/runs/"+receipt["subject_id"].(string)+"/changes?plan_id="+p["id"].(string)+"&revision=2", nil, 200)
			raw, _ := json.Marshal(changes)
			if strings.Contains(string(raw), "synthetic-execution-only-secret") {
				t.Fatal("known AWS value leaked into scoped changes")
			}
			var missing int
			if e = f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.artifacts a WHERE plan_id=$1 AND kind<>'log_segment' AND NOT EXISTS(SELECT FROM cosmoformer.artifact_holds h WHERE h.artifact_id=a.id AND h.owner_kind='plan' AND h.released_at IS NULL)`, p["id"]).Scan(&missing); e != nil || missing != 0 {
				t.Fatal("ready plan lacks evidence holds", e, missing)
			}
		})
	}
}
func TestPlanIntentDoesNotUpgradeChecksOrUnassessedStacks(t *testing.T) {
	f := newPreflightFixture(t)
	ctx := context.Background()
	f.admin.call("POST", "/organizations/"+f.org+"/stacks/"+f.stack+"/plans", map[string]any{"source": map[string]any{"kind": "branch"}, "priority": "medium", "intent": "native_plan"}, 409)
	if e := f.agent.Admit(ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.agent.RunOne(ctx); e != nil {
		t.Fatal(e)
	}
	var native, execution int
	if e := f.db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.operations WHERE native_planning),(SELECT count(*) FROM cosmoformer.work_items WHERE phase='execution')`).Scan(&native, &execution); e != nil || native != 0 || execution != 0 {
		t.Fatal("source check upgraded", e, native, execution)
	}
	if _, e := f.db.Owner.Exec(ctx, `UPDATE cosmoformer.operations SET native_planning=true`); e == nil {
		t.Fatal("existing request accepted upgraded intent")
	}
}

type planningTransport func(*http.Request) (*http.Response, error)

func (f planningTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadinessIndependentlyRejectsManagedSecretLeak(t *testing.T) {
	f := newExecutionFixture(t)
	init := f.stoppedInit(t)
	seal := f.seal(t, []runs.Provider{}, nil)
	schemaPermission := f.transition(t, "start-permission", f.request(t, 2, "schema", seal.Digest), 200).Permission
	schemaStop := f.commandStopped(t, schemaPermission, 0)
	schemaID, _ := f.artifact(t, "provider_schema", []byte(`{"format_version":"1.0","provider_schemas":{}}`))
	planPermission := f.transition(t, "start-permission", f.request(t, 3, "plan", seal.Digest), 200).Permission
	planStop := f.commandStopped(t, planPermission, 0)
	binaryID, _ := f.artifact(t, "binary_plan", []byte("synthetic saved plan"))
	showPermission := f.transition(t, "start-permission", f.request(t, 4, "show", seal.Digest), 200).Permission
	showStop := f.commandStopped(t, showPermission, 0)
	rawID, _ := f.artifact(t, "raw_plan_json", []byte(`{"format_version":"1.2","terraform_version":"1.13.8","planned_values":{},"configuration":{}}`))
	// A compromised/mistaken worker cannot promote its own masking assertion.
	// This deliberately unsafe artifact is protected and remains unready.
	unsafe := plan.Analysis{Identities: plan.Identities{Resources: []string{}, Outputs: []string{}}, Changes: &plan.Result{RuleVersion: f.pre.Rules, ProfileID: f.pre.Profile, Resources: []plan.Resource{}, Drift: []plan.Resource{}, Outputs: []plan.Resource{}, Findings: []plan.Finding{}, Coverage: []plan.Coverage{}, Summary: plan.Summary{Actions: map[string]int{}, Findings: map[string]int{}}, Notices: []string{f.secret}}}
	changesID, _ := f.artifact(t, "normalized_changes", unsafe)
	findingsID, _ := f.artifact(t, "findings", []plan.Finding{})
	var preID string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT pre_init_spec_artifact_id::text FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID).Scan(&preID); e != nil {
		t.Fatal(e)
	}
	commands := []runs.CommandResult{}
	for _, event := range []runs.CommandEvent{init, schemaStop, planStop, showStop} {
		commands = append(commands, runs.CommandResult{Ordinal: event.Ordinal, Permission: event.Permission, Status: "stopped", Exit: event.Exit, Stop: event})
	}
	result := runs.ExecutionResult{Outcome: "success", Commands: commands, Artifacts: []string{preID, seal.Artifact, schemaID, binaryID, rawID, changesID, findingsID}, PreInitDigest: runs.Digest(f.pre), ManifestDigest: seal.Digest, LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/result", f.message(result), 409)
	if !bytes.Contains(raw, []byte("NORMALIZED_SECRET_DETECTED")) || bytes.Contains(raw, []byte(f.secret)) {
		t.Fatal("independent managed-secret gate failed", string(raw))
	}
	f.retained(t)
	result.Outcome = "failure"
	f.transition(t, "result", result, 200)
	var ready int
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.plans WHERE status='ready'`).Scan(&ready); e != nil || ready != 0 {
		t.Fatal("leaking evidence became ready", e)
	}
}
