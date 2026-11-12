package coordinator

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The row fixture tests policy decisions against storage responses. Acceptance
// tests separately exercise the real queries, transaction gate and ownership.
type policyRow struct {
	values []any
	err    error
}

func (r policyRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("unexpected row shape")
	}
	for i, value := range r.values {
		target := reflect.ValueOf(dest[i]).Elem()
		if value == nil {
			target.SetZero()
		} else {
			target.Set(reflect.ValueOf(value))
		}
	}
	return nil
}

type policyTx struct {
	pgx.Tx
	rows     []policyRow
	writes   int
	writeErr error
}

func (tx *policyTx) QueryRow(context.Context, string, ...any) pgx.Row {
	if len(tx.rows) == 0 {
		return policyRow{err: errors.New("unexpected read")}
	}
	row := tx.rows[0]
	tx.rows = tx.rows[1:]
	return row
}
func (tx *policyTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	tx.writes++
	return pgconn.CommandTag{}, tx.writeErr
}
func row(values ...any) policyRow { return policyRow{values: values} }
func codeIs(t *testing.T, e error, want string) {
	t.Helper()
	if runs.Code(e) != want {
		t.Fatalf("want %s, got %v", want, e)
	}
}
func policyCoordinator() *Coordinator {
	return &Coordinator{Instance: "platform", Clock: &clock.Checked{Read: func() (time.Duration, error) { return 5 * time.Second, nil }}}
}

func TestAttentionLookupMustSucceedBeforeReadinessCanBePublished(t *testing.T) {
	failure := errors.New("recovery lookup unavailable")
	for _, scenario := range []string{"no recovery", "recovery", "read failure"} {
		t.Run(scenario, func(t *testing.T) {
			tx := &policyTx{rows: []policyRow{row(scenario == "recovery")}}
			if scenario == "read failure" {
				tx.rows[0] = policyRow{err: failure}
			}
			before := runs.Transition{AttemptState: "ready", OperationStatus: "executing", Ready: true, NextCommandOrdinal: 4}
			out := before
			e := policyCoordinator().preserveExecutionAttention(context.Background(), tx, "attempt", &out)
			if scenario == "read failure" {
				if !errors.Is(e, failure) || !reflect.DeepEqual(out, before) {
					t.Fatal("failed lookup was hidden or changed readiness", e, out)
				}
			} else if e != nil {
				t.Fatal(e)
			} else if scenario == "recovery" {
				if out.AttemptState != "uncertain" || out.OperationStatus != "needs_attention" || out.Ready || out.NextCommandOrdinal != 0 {
					t.Fatal("recovery did not clear every readiness hint", out)
				}
			} else if !reflect.DeepEqual(out, before) {
				t.Fatal("ordinary execution state changed", out)
			}
			if tx.writes != 0 || len(tx.rows) != 0 {
				t.Fatal("readiness policy unexpectedly mutated storage")
			}
		})
	}
}

func TestClosedExecutionEvidenceIsComparedWithoutMutation(t *testing.T) {
	stop := runs.CommandEvent{Ordinal: 1, Permission: runs.ID(), Event: "not_executed", ProofReason: "local_deadline_expired_discarded"}
	result := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{}, Artifacts: []string{}}
	failure := errors.New("evidence read unavailable")
	for _, scenario := range []string{"identical stop", "changed stop", "missing permission", "stop read failure", "malformed stop", "identical result", "changed result", "missing result", "result read failure", "other route"} {
		t.Run(scenario, func(t *testing.T) {
			var payload any = &stop
			tx := &policyTx{rows: []policyRow{row(mustJSON(stop))}}
			wantConflict, wantError := false, false
			switch scenario {
			case "changed stop":
				changed := stop
				changed.Event = "spawned"
				payload, wantConflict = &changed, true
			case "missing permission":
				tx.rows[0] = policyRow{err: pgx.ErrNoRows}
			case "stop read failure":
				tx.rows[0], wantError = policyRow{err: failure}, true
			case "malformed stop":
				tx.rows[0], wantError = row([]byte(`[]`)), true
			case "identical result", "changed result", "missing result", "result read failure":
				payload = &result
				tx.rows[0] = row(runs.Digest(result))
				if scenario == "changed result" {
					changed := result
					changed.Outcome = "unknown"
					payload, wantConflict = &changed, true
				} else if scenario == "missing result" {
					tx.rows[0] = row("")
				} else if scenario == "result read failure" {
					tx.rows[0], wantError = policyRow{err: failure}, true
				}
			case "other route":
				payload, tx.rows = &runs.CommandRequest{}, nil
			}
			conflict, e := policyCoordinator().closedExecutionConflict(context.Background(), tx, "attempt", payload)
			if conflict != wantConflict || (e != nil) != wantError || tx.writes != 0 || len(tx.rows) != 0 {
				t.Fatal("evidence comparison changed history or hid a read failure", conflict, e, tx.writes)
			}
			if (scenario == "stop read failure" || scenario == "result read failure") && !errors.Is(e, failure) {
				t.Fatal("evidence storage error was hidden", e)
			}
		})
	}
}
func budgetRow() policyRow {
	instance := "platform"
	deadline := int64(40 * time.Second)
	return row("platform", int64(time.Second), int64(30*time.Second), &instance, &deadline)
}

func TestExecutionBudgetRequiresCurrentInstanceElapsedTimeAndCredentialCoverage(t *testing.T) {
	cases := map[string]struct {
		change func(*policyRow)
		code   string
	}{
		"previous platform":           {func(r *policyRow) { r.values[0] = "old-platform" }, "ATTEMPT_BUDGET_INSTANCE_ENDED"},
		"before acceptance":           {func(r *policyRow) { r.values[1] = int64(6 * time.Second) }, "ATTEMPT_EXPIRED"},
		"at deadline":                 {func(r *policyRow) { r.values[2] = int64(5 * time.Second) }, "ATTEMPT_EXPIRED"},
		"missing credential instance": {func(r *policyRow) { r.values[3] = nil }, "CREDENTIAL_BUDGET_REQUIRED"},
		"missing credential deadline": {func(r *policyRow) { r.values[4] = nil }, "CREDENTIAL_BUDGET_REQUIRED"},
		"credentials end early":       {func(r *policyRow) { v := int64(29 * time.Second); r.values[4] = &v }, "CREDENTIAL_BUDGET_REQUIRED"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := budgetRow()
			c.change(&r)
			tx := &policyTx{rows: []policyRow{r}}
			_, e := policyCoordinator().executionBudget(context.Background(), tx, "attempt", true)
			codeIs(t, e, c.code)
			if tx.writes != 0 {
				t.Fatal("budget check mutated state")
			}
		})
	}
	c := policyCoordinator()
	c.Clock = &clock.Checked{Read: func() (time.Duration, error) { return 0, errors.New("clock read failed") }}
	_, e := c.executionBudget(context.Background(), &policyTx{}, "attempt", false)
	codeIs(t, e, "CLOCK_UNAVAILABLE")
	failed := errors.New("storage unavailable")
	_, e = policyCoordinator().executionBudget(context.Background(), &policyTx{rows: []policyRow{{err: failed}}}, "attempt", true)
	if !errors.Is(e, failed) {
		t.Fatal(e)
	}
	tick, e := policyCoordinator().executionBudget(context.Background(), &policyTx{rows: []policyRow{budgetRow()}}, "attempt", true)
	if e != nil || tick != 5*time.Second {
		t.Fatal(tick, e)
	}
}
func TestPermissionReplayCannotReviveConsumedExpiredOrUnreservedLaunch(t *testing.T) {
	for _, scenario := range []string{"missing reservation", "old platform", "expired", "already started", "storage failure"} {
		t.Run(scenario, func(t *testing.T) {
			p := &runs.Permission{ID: "permit", Platform: "platform"}
			rows := []policyRow{row(true), budgetRow(), row(int64(10*time.Second), "issued")}
			want := ""
			switch scenario {
			case "missing reservation":
				rows[0] = row(false)
				want = "RESERVATION_REQUIRED"
			case "old platform":
				p.Platform = "old"
				want = "PERMISSION_INSTANCE_ENDED"
			case "expired":
				rows[2] = row(int64(5*time.Second), "issued")
				want = "PERMISSION_EXPIRED"
			case "already started":
				rows[2] = row(int64(10*time.Second), "started")
				want = "PERMISSION_ACCOUNTED"
			case "storage failure":
				rows[2] = policyRow{err: errors.New("unavailable")}
				want = "UNAVAILABLE"
			}
			tx := &policyTx{rows: rows}
			codeIs(t, policyCoordinator().permissionRemaining(context.Background(), tx, "attempt", p), want)
			if tx.writes != 0 {
				t.Fatal("replay renewed permission")
			}
		})
	}
	p := &runs.Permission{ID: "permit", Platform: "platform"}
	tx := &policyTx{rows: []policyRow{row(true), budgetRow(), row(int64(10*time.Second), "issued")}}
	if e := policyCoordinator().permissionRemaining(context.Background(), tx, "attempt", p); e != nil || p.RemainingMS != 5000 {
		t.Fatal(p, e)
	}
}
func TestLaunchIssuanceRequiresUsableBudgetAndSuccessfulPersistence(t *testing.T) {
	for _, scenario := range []string{"too little time", "deadline unavailable", "insert failed", "attempt update failed", "issued"} {
		t.Run(scenario, func(t *testing.T) {
			end := int64(30 * time.Second)
			if scenario == "too little time" {
				end = int64(5*time.Second + 50*time.Millisecond)
			}
			budget := budgetRow()
			budget.values[2] = end
			rows := []policyRow{budget, row(true), row(end, int64(40*time.Second)), row(time.Unix(100, 0))}
			failure := errors.New("storage failed")
			tx := &policyTx{rows: rows}
			if scenario == "deadline unavailable" {
				tx.rows[2] = policyRow{err: failure}
			}
			if scenario == "insert failed" {
				tx.rows[3] = policyRow{err: failure}
			}
			if scenario == "attempt update failed" {
				tx.writeErr = failure
			}
			out := &runs.Transition{}
			e := policyCoordinator().issuePermission(context.Background(), tx, Worker{}, runs.Assignment{}, runs.Message{AttemptID: "attempt"}, runs.CommandRequest{Ordinal: 1, Command: "init"}, nil, out)
			if scenario == "issued" {
				if e != nil || out.Permission == nil || out.Permission.RemainingMS != 9900 || out.AttemptState != "permitted" {
					t.Fatal(out, e)
				}
			} else if scenario == "too little time" {
				codeIs(t, e, "PERMISSION_EXPIRED")
				if out.Permission != nil || tx.writes != 0 {
					t.Fatal("unusable budget issued launch")
				}
			} else if !errors.Is(e, failure) {
				t.Fatal(e)
			}
		})
	}
}
func TestCurrentHoldsAndControlsBlockLaunchWithoutMutation(t *testing.T) {
	for _, flags := range [][]any{{true, false, false}, {false, true, false}, {false, false, true}, {false, false, false}} {
		tx := &policyTx{rows: []policyRow{row(flags...)}}
		e := policyCoordinator().canLaunch(context.Background(), tx, "attempt", "operation")
		want := "UNAVAILABLE"
		if flags[0].(bool) {
			want = "CANCEL_REQUESTED"
		} else if flags[1].(bool) || flags[2].(bool) {
			want = "EXECUTION_UNCERTAIN"
		}
		if want == "UNAVAILABLE" {
			if e != nil {
				t.Fatal(e)
			}
		} else {
			codeIs(t, e, want)
		}
		if tx.writes != 0 {
			t.Fatal("launch guard mutated state")
		}
	}
}
func TestControlAcknowledgmentPreservesSignalAndObservationOrder(t *testing.T) {
	for _, scenario := range []string{"invalid state", "negative ordinal", "wrong ordinal", "missing control", "wrong interrupt", "wrong kill", "observation rollback", "invalid stop reference", "missing stop reference", "storage failure", "delivered"} {
		t.Run(scenario, func(t *testing.T) {
			p := runs.ControlAck{ID: runs.ID(), State: "delivered", Ordinal: 1, Signal: "SIGINT"}
			tx := &policyTx{rows: []policyRow{row("interrupt", "pending", int64(1))}}
			want := "CONTROL_IDENTITY_MISMATCH"
			switch scenario {
			case "invalid state":
				p.State = "ready"
				want = "INVALID_REQUEST"
			case "negative ordinal":
				p.Ordinal = -1
				want = "INVALID_REQUEST"
			case "wrong ordinal":
				p.Ordinal = 2
			case "missing control":
				tx.rows[0] = policyRow{err: pgx.ErrNoRows}
			case "wrong interrupt":
				p.Signal = "SIGKILL"
			case "wrong kill":
				tx.rows[0] = row("force_stop", "pending", int64(1))
			case "observation rollback":
				tx.rows[0] = row("interrupt", "observed", int64(1))
				want = "CONTROL_ORDER"
			case "invalid stop reference":
				p.StopEvent = "not-uuid"
				want = "STOP_EVIDENCE_REQUIRED"
			case "missing stop reference":
				p.StopEvent = runs.ID()
				tx.rows = append(tx.rows, policyRow{err: pgx.ErrNoRows})
				want = "STOP_EVIDENCE_REQUIRED"
			case "storage failure":
				p.StopEvent = runs.ID()
				tx.rows = append(tx.rows, policyRow{err: errors.New("storage failed")})
				want = "UNAVAILABLE"
			}
			e := policyCoordinator().controlAck(context.Background(), tx, runs.Message{}, p)
			if scenario == "delivered" {
				if e != nil || tx.writes != 1 {
					t.Fatal(e, tx.writes)
				}
			} else {
				codeIs(t, e, want)
				if tx.writes != 0 {
					t.Fatal("invalid control changed evidence")
				}
			}
		})
	}
}

type ledgerRows struct {
	pgx.Rows
	items   []policyRow
	current int
	err     error
	closed  bool
}

func (r *ledgerRows) Next() bool {
	if r.current >= len(r.items) {
		return false
	}
	r.current++
	return true
}
func (r *ledgerRows) Scan(dest ...any) error { return r.items[r.current-1].Scan(dest...) }
func (r *ledgerRows) Err() error             { return r.err }
func (r *ledgerRows) Close()                 { r.closed = true }

type ledgerTx struct {
	*policyTx
	ledger *ledgerRows
	err    error
}

func (tx *ledgerTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return tx.ledger, tx.err
}
func TestResultStopsOnUnreadableOrIncompletePermissionLedger(t *testing.T) {
	failure := errors.New("permission ledger unavailable")
	for _, scenario := range []string{"query failure", "row failure", "stream failure", "unaccounted launch", "malformed stop"} {
		t.Run(scenario, func(t *testing.T) {
			rows := &ledgerRows{}
			tx := &ledgerTx{policyTx: &policyTx{}, ledger: rows}
			want := "UNAVAILABLE"
			switch scenario {
			case "query failure":
				tx.err = failure
			case "row failure":
				rows.items = []policyRow{{err: failure}}
			case "stream failure":
				rows.err = failure
			case "unaccounted launch":
				rows.items = []policyRow{row(int64(1), "permit", "started", []byte(`{}`))}
				want = "PERMISSION_UNACCOUNTED"
			case "malformed stop":
				rows.items = []policyRow{row(int64(1), "permit", "stopped", []byte(`{`))}
				want = "STOP_EVIDENCE_REQUIRED"
			}
			result := runs.ExecutionResult{Outcome: "failure", Commands: []runs.CommandResult{}, Artifacts: []string{}}
			conflict, e := policyCoordinator().executionResult(context.Background(), tx, runs.Assignment{}, runs.Message{}, result, "", "", true, nil, &runs.Transition{})
			codeIs(t, e, want)
			if conflict || tx.writes != 0 {
				t.Fatal("unreadable permission ledger permitted finalization")
			}
			if scenario != "query failure" && !rows.closed {
				t.Fatal("ledger cursor leaked")
			}
		})
	}
}
func TestLogFinalizationRejectsContradictionsAndSurfacesPersistenceFailure(t *testing.T) {
	for _, scenario := range []string{"bad stream", "unsafe sequence", "contradictory completeness", "wrong sequence", "gapped complete stream", "ledger failure", "unknown gap write failure", "receipt write failure", "nil receipt"} {
		t.Run(scenario, func(t *testing.T) {
			receipt := &runs.LogFinal{Stream: "console", LastSequence: 2, Complete: true}
			tx := &policyTx{rows: []policyRow{row(int64(2), false)}}
			want := "INVALID_REQUEST"
			switch scenario {
			case "bad stream":
				receipt.Stream = "private"
			case "unsafe sequence":
				receipt.LastSequence = 9007199254740992
			case "contradictory completeness":
				receipt.Unknown = true
			case "wrong sequence":
				receipt.LastSequence = 1
				want = "LOG_FINAL_MISMATCH"
			case "gapped complete stream":
				tx.rows[0] = row(int64(2), true)
				want = "LOG_FINAL_MISMATCH"
			case "ledger failure":
				tx.rows[0] = policyRow{err: errors.New("unavailable")}
				want = "UNAVAILABLE"
			case "unknown gap write failure":
				receipt.Complete = false
				receipt.Unknown = true
				tx.writeErr = errors.New("unavailable")
				want = "UNAVAILABLE"
			case "receipt write failure":
				tx.writeErr = errors.New("unavailable")
				want = "UNAVAILABLE"
			case "nil receipt":
				receipt = nil
			}
			e := policyCoordinator().executionLogFinal(context.Background(), tx, "attempt", receipt)
			if scenario == "nil receipt" {
				if e != nil || tx.writes != 1 {
					t.Fatal(e, tx.writes)
				}
			} else {
				codeIs(t, e, want)
			}
		})
	}
}
func TestObservationFencesCannotBeRenewedAcrossInstancesTimeOrFacts(t *testing.T) {
	o := Observation{SubjectID: "subject", Instance: "platform", Started: time.Second, Baseline: 1, Ticket: 2}
	for _, scenario := range []string{"other instance", "before read", "expired read", "changed facts", "newer ticket", "storage failure", "current"} {
		t.Run(scenario, func(t *testing.T) {
			c := policyCoordinator()
			v := o
			tx := &policyTx{rows: []policyRow{row(int64(1), int64(2))}}
			switch scenario {
			case "other instance":
				v.Instance = "previous"
			case "before read":
				v.Started = 6 * time.Second
			case "expired read":
				v.Started = -30 * time.Second
			case "changed facts":
				tx.rows[0] = row(int64(2), int64(2))
			case "newer ticket":
				tx.rows[0] = row(int64(1), int64(3))
			case "storage failure":
				tx.rows[0] = policyRow{err: errors.New("unavailable")}
			}
			e := c.RequireObservation(context.Background(), tx, v, 1)
			if scenario == "current" {
				if e != nil {
					t.Fatal(e)
				}
			} else if scenario == "storage failure" {
				codeIs(t, e, "UNAVAILABLE")
			} else {
				codeIs(t, e, "SOURCE_CHANGED")
			}
			if tx.writes != 0 {
				t.Fatal("observation check renewed stored facts")
			}
		})
	}
}

func TestStorageReadFailuresCannotBeMistakenForAuthorityOrOwnership(t *testing.T) {
	failure := errors.New("synthetic storage outage")
	for name, call := range map[string]func(*Coordinator, *policyTx) error{
		"current worker": func(c *Coordinator, tx *policyTx) error {
			return c.current(context.Background(), tx, Worker{ID: "worker"}, runs.Identity{WorkerID: "worker", Agent: runs.ID(), Generation: 1}, true)
		},
		"source authority": func(c *Coordinator, tx *policyTx) error {
			return c.authority(context.Background(), tx, "operation", runs.PreflightSpec{})
		},
		"attempt owner": func(c *Coordinator, tx *policyTx) error {
			return c.owns(context.Background(), tx, Worker{}, runs.Message{}, true)
		},
		"launch guard": func(c *Coordinator, tx *policyTx) error {
			return c.canLaunch(context.Background(), tx, "attempt", "operation")
		},
		"reservations": func(c *Coordinator, tx *policyTx) error {
			return c.exactReservations(context.Background(), tx, "attempt")
		},
		"artifact identity": func(c *Coordinator, tx *policyTx) error {
			return c.verifyExecutionEvidence(context.Background(), tx, "attempt", []evidence.Artifact{{ID: "artifact"}})
		},
		"assignment": func(c *Coordinator, tx *policyTx) error {
			a, e := c.assignment(context.Background(), tx, "attempt")
			if a != nil {
				t.Error("unreadable assignment returned authority")
			}
			return e
		},
		"remaining budget": func(c *Coordinator, tx *policyTx) error {
			return c.remaining(context.Background(), tx, &runs.Assignment{Platform: c.Instance}, time.Second)
		},
		"begin observation": func(c *Coordinator, tx *policyTx) error {
			_, e := c.BeginObservation(context.Background(), tx, "org", "repo", "branch", "main")
			return e
		},
		"accept observation": func(c *Coordinator, tx *policyTx) error {
			_, e := c.AcceptObservation(context.Background(), tx, "org", Observation{}, bitbucket.Facts{})
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			tx := &policyTx{rows: []policyRow{{err: failure}}}
			if e := call(policyCoordinator(), tx); !errors.Is(e, failure) {
				t.Fatal("storage failure was replaced by an authorization result", e)
			}
			if tx.writes != 0 {
				t.Fatal("failed authority read mutated ownership")
			}
		})
	}
}
func TestCanonicalPreparationAndMetadataValidationFailBeforeStorage(t *testing.T) {
	for _, raw := range []string{`{`, `{"unknown":true}`, ` {"accepted":true}`} {
		var v struct {
			Accepted bool `json:"accepted"`
		}
		codeIs(t, canonicalEvidence([]byte(raw), &v), "INVALID_MANIFEST")
	}
	var v struct {
		Accepted bool `json:"accepted"`
	}
	if e := canonicalEvidence([]byte(`{"accepted":true}`), &v); e != nil {
		t.Fatal(e)
	}
	c := policyCoordinator()
	if _, _, e := c.readExecutionArtifact(context.Background(), "", runs.Message{}, "invalid", "pre_init_spec"); runs.Code(e) != "EVIDENCE_REQUIRED" {
		t.Fatal(e)
	}
	for _, route := range []string{"prepare-result", "manifest-seal", "start-permission"} {
		if _, e := c.executionEvidence(context.Background(), "", route, runs.Message{Payload: []byte(`{"unknown":true}`)}); e == nil {
			t.Fatal("unclosed evidence request accepted", route)
		}
	}
	codeIs(t, c.ExecutionInputs(context.Background(), "", "", runs.ExecutionInputs{}, nil), "INVALID_REQUEST")
}
