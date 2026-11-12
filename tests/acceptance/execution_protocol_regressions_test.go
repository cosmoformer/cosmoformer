//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestExecutionRecoverySurvivesEveryCommandEvent(t *testing.T) {
	for _, cause := range []string{"unknown result", "conflicting replay"} {
		for _, event := range []string{"spawned", "not_executed", "stopped"} {
			t.Run(cause+"/"+event, func(t *testing.T) {
				f := newExecutionFixture(t)
				f.prepare(t)
				request := f.request(t, 1, "init", "")
				accepted := f.message(request)
				raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", accepted, 200)
				var permission struct {
					ID string `json:"permission_id"`
				}
				if e := json.Unmarshal(raw, &permission); e != nil || permission.ID == "" {
					t.Fatal("missing permission", e)
				}
				f.seq++
				pid, exit, empty := int64(1234), 0, true
				spawn := runs.CommandEvent{Ordinal: 1, Permission: permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
				if event == "stopped" {
					f.transition(t, "command-events", spawn, 200)
				}
				if cause == "unknown result" {
					unknown := runs.ExecutionResult{Outcome: "unknown", Commands: []runs.CommandResult{}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Unknown: true}}
					expectWorkerCode(t, f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/result", f.message(unknown), 409), "MESSAGE_CONFLICT")
				} else {
					changed := accepted
					request.Digest = strings.Repeat("0", 64)
					changed.Payload, _ = json.Marshal(request)
					expectWorkerCode(t, f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", changed, 409), "MESSAGE_CONFLICT")
				}
				inputs := runs.ExecutionInputs{Identity: f.agent.Identity, RequestID: runs.ID(), SpecDigest: f.assignment.Digest, Secrets: f.assignment.Execution.ExpectedSecrets()}
				inputPath := "/attempts/" + f.assignment.AttemptID + "/execution-inputs"
				f.workerCall(t, inputPath, inputs, 403)
				observation := spawn
				if event == "stopped" {
					observation.Event, observation.Exit, observation.GroupEmpty, observation.Pipes = "stopped", &exit, &empty, "drained"
				} else if event == "not_executed" {
					observation = runs.CommandEvent{Ordinal: 1, Permission: permission.ID, Event: "not_executed", ProofReason: "local_deadline_expired_discarded"}
				}
				message := f.message(observation)
				path := "/attempts/" + f.assignment.AttemptID + "/command-events"
				first := f.workerCall(t, path, message, 200)
				if !bytes.Equal(first, f.workerCall(t, path, message, 200)) {
					t.Fatal("late event replay changed its receipt")
				}
				f.seq++
				var out struct {
					State  string `json:"attempt_state"`
					Status string `json:"operation_status"`
					Ready  bool   `json:"ready"`
					Next   int    `json:"next_command_ordinal"`
				}
				if e := json.Unmarshal(first, &out); e != nil || out.State != "uncertain" || out.Status != "needs_attention" || out.Ready || out.Next != 0 {
					t.Fatal("late event cleared recovery state or readiness", out, e)
				}
				duplicate := f.transition(t, "command-events", observation, 200)
				if duplicate.AttemptState != "uncertain" || duplicate.OperationStatus != "needs_attention" || duplicate.Ready || duplicate.NextCommandOrdinal != 0 {
					t.Fatal("fresh identical observation cleared recovery state", duplicate)
				}
				f.workerCall(t, inputPath, inputs, 403)
				expectWorkerCode(t, f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/start-permission", f.message(f.request(t, 2, "schema", strings.Repeat("a", 64))), 409), "EXECUTION_UNCERTAIN")
				var state, status, observed string
				var cases, epoch int64
				e := f.db.Owner.QueryRow(context.Background(), `SELECT a.state,o.status,p.status,(SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=a.id AND status='open'),cosmoformer.execution_target_epoch(a.id) FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id JOIN cosmoformer.start_permissions p ON p.attempt_id=a.id WHERE a.id=$1`, f.assignment.AttemptID).Scan(&state, &status, &observed, &cases, &epoch)
				want := event
				if event == "spawned" {
					want = "started"
				}
				if e != nil || state != "uncertain" || status != "needs_attention" || observed != want || cases != 1 || epoch != 1 {
					t.Fatal("late event lost attention or its process facts", state, status, observed, cases, epoch, e)
				}
				f.retained(t)
			})
		}
	}
}

func TestExecutionClosedAttemptFailsSafelyOnUnreadableEvidence(t *testing.T) {
	f := newExecutionFixture(t)
	stop := f.stoppedInit(t)
	result := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{{Ordinal: stop.Ordinal, Permission: stop.Permission, Status: "stopped", Exit: stop.Exit, Stop: stop}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
	f.transition(t, "result", result, 200)
	// Inject damaged stored evidence, not a worker-supplied payload. A read
	// failure must never be treated as either matching facts or a contradiction.
	f.exec(t, `UPDATE cosmoformer.start_permissions SET stop_facts='[]'::jsonb WHERE id=$1`, stop.Permission)
	message := f.message(stop)
	raw := f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/command-events", message, 503)
	if bytes.Contains(raw, []byte("unmarshal")) || bytes.Contains(raw, []byte("CommandEvent")) {
		t.Fatal("evidence failure exposed an internal diagnostic")
	}
	var closed bool
	var receipts, cases, reservations int
	e := f.db.Owner.QueryRow(context.Background(), `SELECT closed_at IS NOT NULL,(SELECT count(*) FROM cosmoformer.attempt_messages WHERE attempt_id=a.id AND sequence=$2),(SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=a.id),(SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=a.id)+(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=a.id) FROM cosmoformer.attempts a WHERE a.id=$1`, f.assignment.AttemptID, message.Seq).Scan(&closed, &receipts, &cases, &reservations)
	if e != nil || !closed || receipts != 0 || cases != 0 || reservations != 0 {
		t.Fatal("failed evidence read changed closure, ownership or accepted history", closed, receipts, cases, reservations, e)
	}
}

func TestExecutionAttentionReadFailureRollsBackCommandFacts(t *testing.T) {
	f := newExecutionFixture(t)
	f.prepare(t)
	permission := f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200).Permission
	// Restrict this disposable database's table, not a shared role or service.
	// Earlier process-fact writes must roll back when the final hold read fails.
	f.exec(t, `REVOKE SELECT ON cosmoformer.recovery_cases FROM cosmoformer_app`)
	pid := int64(1234)
	message := f.message(runs.CommandEvent{Ordinal: 1, Permission: permission.ID, Event: "spawned", PID: &pid, PGID: &pid})
	path := "/attempts/" + f.assignment.AttemptID + "/command-events"
	raw := f.workerCall(t, path, message, 503)
	if bytes.Contains(raw, []byte("permission denied")) || bytes.Contains(raw, []byte("recovery_cases")) {
		t.Fatal("storage permission failure leaked internal details")
	}
	var state, status string
	var receipts int
	var noSpawn bool
	e := f.db.Owner.QueryRow(context.Background(), `SELECT a.state,p.status,p.spawn_facts IS NULL,(SELECT count(*) FROM cosmoformer.attempt_messages WHERE attempt_id=a.id AND sequence=$2) FROM cosmoformer.attempts a JOIN cosmoformer.start_permissions p ON p.attempt_id=a.id WHERE a.id=$1`, f.assignment.AttemptID, message.Seq).Scan(&state, &status, &noSpawn, &receipts)
	if e != nil || state != "permitted" || status != "issued" || !noSpawn || receipts != 0 {
		t.Fatal("failed hold lookup partially accepted process facts", state, status, noSpawn, receipts, e)
	}
	f.retained(t)
	f.exec(t, `GRANT SELECT ON cosmoformer.recovery_cases TO cosmoformer_app`)
	f.workerCall(t, path, message, 200)
}

func TestExecutionPermissionBudgetClockFailureCommitsFence(t *testing.T) {
	f := newExecutionFixture(t)
	f.stoppedInit(t)
	seal := f.seal(t, []runs.Provider{}, nil)
	message := f.message(f.request(t, 2, "schema", seal.Digest))
	path := "/attempts/" + f.assignment.AttemptID + "/start-permission"
	// Schema needs no source observation: the first two reads are the initial
	// clock check and the gated clock check; the third reads the attempt budget.
	f.ticks.mu.Lock()
	f.ticks.failAfter = 3
	f.ticks.mu.Unlock()
	expectWorkerCode(t, f.workerCall(t, path, message, 409), "CLOCK_UNRELIABLE")
	var latched bool
	var permissions, receipts int
	err := f.db.Owner.QueryRow(context.Background(), `SELECT clock_unreliable,(SELECT count(*) FROM cosmoformer.start_permissions WHERE attempt_id=$1 AND ordinal=2),(SELECT count(*) FROM cosmoformer.attempt_messages WHERE attempt_id=$1 AND sequence=$2) FROM cosmoformer.installation`, f.assignment.AttemptID, message.Seq).Scan(&latched, &permissions, &receipts)
	if err != nil || !latched || permissions != 0 || receipts != 0 {
		t.Fatal("denied budget read lost its durable clock fence", latched, permissions, receipts, err)
	}
	// The injected source recovers immediately, but durable validation is still
	// required. A healthy subsequent read must not clear the installation latch.
	expectWorkerCode(t, f.workerCall(t, path, message, 409), "STALE_AGENT")
	if err = f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.start_permissions WHERE attempt_id=$1 AND ordinal=2`, f.assignment.AttemptID).Scan(&permissions); err != nil || permissions != 0 {
		t.Fatal("recovered clock issued authority without validation", permissions, err)
	}
}

func TestExecutionPRObservationCannotReviveOldAuthority(t *testing.T) {
	for _, scenario := range []string{"retargeted", "closed", "unchanged"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionFixtureForSource(t, map[string]any{"kind": "pr", "pr_number": 3})
			f.prepare(t)
			f.remote.mu.Lock()
			if scenario == "retargeted" {
				f.remote.prDestination = "release"
			} else if scenario == "closed" {
				f.remote.prState = "DECLINED"
			}
			f.remote.mu.Unlock()
			path := "/attempts/" + f.assignment.AttemptID + "/start-permission"
			message := f.message(f.request(t, 1, "init", ""))
			if scenario == "unchanged" {
				f.workerCall(t, path, message, 200)
				return
			}
			expectWorkerCode(t, f.workerCall(t, path, message, 409), "SOURCE_CHANGED")
			var revision int64
			var state, destination string
			e := f.db.Owner.QueryRow(context.Background(), `SELECT facts_revision,facts->>'state',facts->>'destination' FROM cosmoformer.source_subjects WHERE id=$1`, f.assignment.Source.SubjectID).Scan(&revision, &state, &destination)
			if e != nil || revision != f.assignment.Source.FactsRevision+1 || scenario == "retargeted" && destination != "release" || scenario == "closed" && state != "DECLINED" {
				t.Fatal("authoritative changed PR facts were discarded", revision, state, destination, e)
			}
			// Exercise the wired server adapter, then restore exactly the old remote
			// facts. The earlier revision must remain unusable, including for reads.
			f.remote.mu.Lock()
			f.remote.prDestination, f.remote.prState = "", ""
			f.remote.mu.Unlock()
			expectWorkerCode(t, f.workerCall(t, path, message, 409), "SOURCE_CHANGED")
			inputs := runs.ExecutionInputs{Identity: f.agent.Identity, RequestID: runs.ID(), SpecDigest: f.assignment.Digest, Secrets: f.assignment.Execution.ExpectedSecrets()}
			expectWorkerCode(t, f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/execution-inputs", inputs, 409), "SOURCE_CHANGED")
			var permissions, messages int
			e = f.db.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.start_permissions WHERE attempt_id=$1),(SELECT count(*) FROM cosmoformer.attempt_messages WHERE attempt_id=$1 AND sequence=$2)`, f.assignment.AttemptID, message.Seq).Scan(&permissions, &messages)
			if e != nil || permissions != 0 || messages != 0 {
				t.Fatal("rejected authority issued a permission or consumed sequence", permissions, messages, e)
			}
			f.retained(t)
		})
	}
}

func TestExecutionClosedAttemptReconcilesFreshTerminalFacts(t *testing.T) {
	for _, scenario := range []string{"identical stop", "changed stop", "identical result", "changed result", "unknown permission", "sequence gap", "stale agent"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionFixture(t)
			stop := f.stoppedInit(t)
			result := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{{Ordinal: stop.Ordinal, Permission: stop.Permission, Status: "stopped", Exit: stop.Exit, Stop: stop}}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
			f.transition(t, "result", result, 200)
			// A later owner is injected at the existing claim boundary. Late facts
			// must fence the target without touching that owner's reservations.
			next := runs.ID()
			f.exec(t, `INSERT INTO cosmoformer.attempts(id,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,execution_spec,execution_spec_digest) SELECT $2,org_id,work_item_id,operation_id,stack_id,target_id,worker_id,generation,agent_instance,'claimed',budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,execution_spec,execution_spec_digest FROM cosmoformer.attempts WHERE id=$1`, f.assignment.AttemptID, next)
			f.exec(t, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) SELECT org_id,worker_id,id,generation,agent_instance FROM cosmoformer.attempts WHERE id=$1`, next)
			f.exec(t, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, next)
			var payload any = stop
			route, code := "command-events", "ATTEMPT_CLOSED"
			conflict := scenario == "changed stop" || scenario == "changed result"
			switch scenario {
			case "changed stop":
				changed := stop
				empty := false
				changed.GroupEmpty = &empty
				payload = changed
			case "identical result", "changed result":
				route, payload = "result", result
				if conflict {
					changed := result
					changed.Outcome = "unknown"
					payload = changed
				}
			case "unknown permission":
				changed := stop
				changed.Permission = runs.ID()
				payload = changed
			}
			message := f.message(payload)
			if conflict {
				code = "MESSAGE_CONFLICT"
			} else if scenario == "sequence gap" {
				message.Seq++
				code = "EXPECTED_SEQUENCE"
			} else if scenario == "stale agent" {
				message.Agent = runs.ID()
				code = "STALE_AGENT"
			}
			path := "/attempts/" + f.assignment.AttemptID + "/" + route
			expectWorkerCode(t, f.workerCall(t, path, message, 409), code)
			expectWorkerCode(t, f.workerCall(t, path, message, 409), code)
			var cases, epoch, reservations int64
			var closed bool
			var acceptedStop []byte
			var acceptedResult string
			e := f.db.Owner.QueryRow(context.Background(), `SELECT a.closed_at IS NOT NULL,(SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=a.id AND status='open'),cosmoformer.execution_target_epoch(a.id),(SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=$2)+(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=$2),(SELECT stop_facts FROM cosmoformer.start_permissions WHERE id=$3),encode(a.result_digest,'hex') FROM cosmoformer.attempts a WHERE a.id=$1`, f.assignment.AttemptID, next, stop.Permission).Scan(&closed, &cases, &epoch, &reservations, &acceptedStop, &acceptedResult)
			want := int64(0)
			if conflict {
				want = 1
			}
			var original runs.CommandEvent
			if e != nil || !closed || cases != want || epoch != want || reservations != 2 || runs.Decode(acceptedStop, &original) != nil || runs.Digest(original) != runs.Digest(stop) || acceptedResult != runs.Digest(result) {
				t.Fatal("late facts changed history/ownership or failed to fence", closed, cases, epoch, reservations, e)
			}
		})
	}
}

func expectWorkerCode(t *testing.T, raw []byte, code string) {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &problem) != nil || problem.Code != code {
		t.Fatalf("expected %s: %s", code, raw)
	}
}

func TestExecutionEarlierTerminalFactsAfterAdvance(t *testing.T) {
	for _, scenario := range []string{"identical", "contradictory"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionFixture(t)
			init := f.stoppedInit(t)
			seal := f.seal(t, []runs.Provider{}, nil)
			schema := f.transition(t, "start-permission", f.request(t, 2, "schema", seal.Digest), 200)
			path := "/attempts/" + f.assignment.AttemptID + "/command-events"
			if scenario == "identical" {
				// This is a fresh message at the next sequence, after ordinal 2 was issued.
				// Re-observing the same earlier stop must not rewind the current command.
				out := f.transition(t, "command-events", init, 200)
				if out.AttemptState != "permitted" {
					t.Fatal("earlier stop rewound the current attempt", out)
				}
				var status string
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT status FROM cosmoformer.start_permissions WHERE id=$1`, schema.Permission.ID).Scan(&status); e != nil || status != "issued" {
					t.Fatal("current permission changed", status, e)
				}
				var cases int
				if e := f.db.Owner.QueryRow(context.Background(), `SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=$1`, f.assignment.AttemptID).Scan(&cases); e != nil || cases != 0 {
					t.Fatal("identical facts opened recovery", e)
				}
				f.retained(t)
				return
			}
			stoppedSchema := f.commandStopped(t, schema.Permission, 0)
			changed := init
			exit := 1
			changed.Exit = &exit
			raw := f.workerCall(t, path, f.message(changed), 409)
			expectWorkerCode(t, raw, "MESSAGE_CONFLICT")
			// A repeated contradiction cannot bump the backend epoch twice.
			expectWorkerCode(t, f.workerCall(t, path, f.message(changed), 409), "MESSAGE_CONFLICT")
			var state, status string
			var cases, epoch int64
			e := f.db.Owner.QueryRow(context.Background(), `SELECT a.state,o.status,(SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=a.id AND status='open'),cosmoformer.execution_target_epoch(a.id) FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id WHERE a.id=$1`, f.assignment.AttemptID).Scan(&state, &status, &cases, &epoch)
			if e != nil || state != "uncertain" || status != "needs_attention" || cases != 1 || epoch != 1 {
				t.Fatal("earlier contradiction was not fenced", state, status, cases, epoch, e)
			}
			var retained []byte
			if e = f.db.Owner.QueryRow(context.Background(), `SELECT stop_facts FROM cosmoformer.start_permissions WHERE id=$1`, init.Permission).Scan(&retained); e != nil {
				t.Fatal(e)
			}
			var original runs.CommandEvent
			if runs.Decode(retained, &original) != nil || runs.Digest(original) != runs.Digest(init) {
				t.Fatal("contradiction replaced the accepted stop")
			}
			result := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), ManifestDigest: seal.Digest, LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Complete: true}}
			for _, stop := range []runs.CommandEvent{init, stoppedSchema} {
				result.Commands = append(result.Commands, runs.CommandResult{Ordinal: stop.Ordinal, Permission: stop.Permission, Status: "stopped", Exit: stop.Exit, Stop: stop})
			}
			raw = f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/result", f.message(result), 409)
			expectWorkerCode(t, raw, "INDEPENDENT_STOP_PROOF_REQUIRED")
			f.retained(t)
		})
	}
}

func TestExecutionControlAckUsesAcceptedStopMessageIdentity(t *testing.T) {
	f := newExecutionFixture(t)
	f.prepare(t)
	initPermit := f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200)
	sendEvent := func(event runs.CommandEvent) runs.Message {
		t.Helper()
		message := f.message(event)
		f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/command-events", message, 200)
		f.seq++
		return message
	}
	pid := int64(1234)
	exit := 0
	empty := true
	event := runs.CommandEvent{Ordinal: 1, Permission: initPermit.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
	sendEvent(event)
	event.Event = "stopped"
	event.Exit = &exit
	event.GroupEmpty = &empty
	event.Pipes = "drained"
	initStop := sendEvent(event)
	seal := f.seal(t, []runs.Provider{}, nil)
	schema := f.transition(t, "start-permission", f.request(t, 2, "schema", seal.Digest), 200)
	pid = 2345
	event = runs.CommandEvent{Ordinal: 2, Permission: schema.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
	spawn := sendEvent(event)
	// Control creation is injected just like the execution claim: this does not
	// implement or enable the later browser cancellation workflow.
	control := runs.ID()
	f.exec(t, `INSERT INTO cosmoformer.control_requests(id,org_id,attempt_id,kind,command_ordinal,confirmed,state,requested_at) VALUES($1,$2,$3,'cancel',2,false,'requested',clock_timestamp())`, control, f.org, f.assignment.AttemptID)
	f.exec(t, `UPDATE cosmoformer.operations SET cancel_requested=true WHERE id=$1`, f.assignment.OperationID)
	ack := runs.ControlAck{ID: control, State: "delivered", Ordinal: 2, Signal: "SIGINT"}
	f.transition(t, "controls/ack", ack, 200)
	ack.State = "observed"
	ack.Signal = ""
	ack.StopEvent = spawn.MessageID
	path := "/attempts/" + f.assignment.AttemptID + "/controls/ack"
	expectWorkerCode(t, f.workerCall(t, path, f.message(ack), 409), "STOP_EVIDENCE_REQUIRED")
	event.Event = "stopped"
	event.Exit = &exit
	event.GroupEmpty = &empty
	event.Pipes = "drained"
	stop := sendEvent(event)
	// Once the permission is stopped, its earlier spawn event is still not stop
	// evidence. Nor may a different command/attempt or private database ID stand in.
	var rowID string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT id::text FROM cosmoformer.attempt_messages WHERE attempt_id=$1 AND message_id=$2`, f.assignment.AttemptID, stop.MessageID).Scan(&rowID); e != nil {
		t.Fatal(e)
	}
	if rowID == stop.MessageID {
		t.Fatal("fixture did not separate wire and database identities")
	}
	// The worker uses only the message_id it chose, with no returned/internal ID.
	ack.StopEvent = stop.MessageID
	message := f.message(ack)
	first := f.workerCall(t, path, message, 200)
	replay := f.workerCall(t, path, message, 200)
	if !bytes.Equal(first, replay) {
		t.Fatal("control response-loss replay changed the receipt")
	}
	f.seq++
	for _, reference := range []string{spawn.MessageID, initStop.MessageID, f.wire.ack.MessageID, rowID, runs.ID(), "invalid-uuid"} {
		ack.StopEvent = reference
		expectWorkerCode(t, f.workerCall(t, path, f.message(ack), 409), "STOP_EVIDENCE_REQUIRED")
	}
	var state string
	var linked bool
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT c.state,c.observed_event_id=m.id FROM cosmoformer.control_requests c JOIN cosmoformer.attempt_messages m ON m.attempt_id=c.attempt_id AND m.message_id=$2 WHERE c.id=$1`, control, stop.MessageID).Scan(&state, &linked); e != nil || state != "observed" || !linked {
		t.Fatal("control did not retain the exact accepted stop", state, linked, e)
	}
	f.retained(t)
}
