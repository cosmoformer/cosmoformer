package coordinator

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

func (c *Coordinator) preserveExecutionAttention(ctx context.Context, tx pgx.Tx, attempt string, out *runs.Transition) error {
	var uncertain bool
	if e := tx.QueryRow(ctx, `SELECT a.state='uncertain' OR EXISTS(SELECT FROM cosmoformer.recovery_cases rc WHERE target_id=a.target_id AND status='open' AND NOT EXISTS(SELECT FROM cosmoformer.operations ro WHERE ro.id=a.operation_id AND ro.recovery_case_id=rc.id AND ro.recovery_evidence_revision=rc.evidence_revision)) FROM cosmoformer.attempts a WHERE a.id=$1`, attempt).Scan(&uncertain); e != nil {
		return e
	}
	if uncertain {
		out.AttemptState = "uncertain"
		out.OperationStatus = "needs_attention"
		out.Ready = false
		out.NextCommandOrdinal = 0
	}
	return nil
}

// Closed attempts may acquire a recovery hold from new contradictory facts,
// but their accepted evidence, closure and released reservations stay immutable.
// The caller has already checked the current owning agent and next sequence.
func (c *Coordinator) closedExecutionConflict(ctx context.Context, tx pgx.Tx, attempt string, payload any) (bool, error) {
	switch p := payload.(type) {
	case *runs.CommandEvent:
		var stop []byte
		e := tx.QueryRow(ctx, `SELECT stop_facts FROM cosmoformer.start_permissions WHERE attempt_id=$1 AND id::text=$2 AND ordinal=$3 AND status IN ('stopped','not_executed')`, attempt, p.Permission, p.Ordinal).Scan(&stop)
		if errors.Is(e, pgx.ErrNoRows) {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		var prior runs.CommandEvent
		if e = json.Unmarshal(stop, &prior); e != nil {
			return false, e
		}
		return runs.Digest(prior) != runs.Digest(p), nil
	case *runs.ExecutionResult:
		var accepted string
		if e := tx.QueryRow(ctx, `SELECT coalesce(encode(result_digest,'hex'),'') FROM cosmoformer.attempts WHERE id=$1`, attempt).Scan(&accepted); e != nil {
			return false, e
		}
		return accepted != "" && accepted != runs.Digest(p), nil
	default:
		return false, nil
	}
}

func (c *Coordinator) commandEvent(ctx context.Context, tx pgx.Tx, a runs.Assignment, m runs.Message, p runs.CommandEvent, out *runs.Transition) (bool, error) {
	if p.Ordinal < 1 || p.Permission == "" {
		return false, runs.Fail("INVALID_REQUEST")
	}
	var status, command string
	var ordinal int64
	var spawn, stop []byte
	e := tx.QueryRow(ctx, `SELECT status,command,spawn_facts,stop_facts FROM cosmoformer.start_permissions WHERE id=$1 AND attempt_id=$2 AND ordinal=$3`, p.Permission, m.AttemptID, p.Ordinal).Scan(&status, &command, &spawn, &stop)
	if e != nil {
		return false, runs.Fail("PERMISSION_REQUIRED")
	}
	// Terminal facts remain authoritative after later commands advance the
	// ordinal. An identical observation is harmless; a contradiction still needs
	// attention instead of being discarded as an ordering error.
	if status == "stopped" || status == "not_executed" {
		var prior runs.CommandEvent
		if json.Unmarshal(stop, &prior) == nil && runs.Digest(prior) == runs.Digest(p) {
			return false, nil
		}
		return true, nil
	}
	if e = tx.QueryRow(ctx, `SELECT command_ordinal FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&ordinal); e != nil {
		return false, e
	}
	if p.Ordinal != ordinal {
		return false, runs.Fail("COMMAND_ORDER")
	}
	switch p.Event {
	case "spawned":
		if p.PID == nil || p.PGID == nil || *p.PID <= 0 || *p.PID > 2147483647 || *p.PID != *p.PGID || p.Exit != nil || p.Signal != nil || p.GroupEmpty != nil || p.Pipes != "" || p.ProofReason != "" {
			return false, runs.Fail("INVALID_REQUEST")
		}
		if status == "started" {
			var prior runs.CommandEvent
			if json.Unmarshal(spawn, &prior) == nil && runs.Digest(prior) == runs.Digest(p) {
				return false, nil
			}
			return true, nil
		}
		if status != "issued" {
			return true, nil
		}
		_, e = tx.Exec(ctx, `UPDATE cosmoformer.start_permissions SET status='started',spawn_facts=$2,started_at=clock_timestamp(),revision=revision+1 WHERE id=$1`, p.Permission, mustJSON(p))
		out.AttemptState = "running"
	case "stopped":
		if status != "started" {
			return false, runs.Fail("SPAWN_EVIDENCE_REQUIRED")
		}
		var prior runs.CommandEvent
		if json.Unmarshal(spawn, &prior) != nil || p.PID == nil || p.PGID == nil || prior.PID == nil || prior.PGID == nil || *p.PID != *prior.PID || *p.PGID != *prior.PGID {
			return true, nil
		}
		if (p.Exit == nil) == (p.Signal == nil) || p.Exit != nil && (*p.Exit < 0 || *p.Exit > 255) || p.Signal != nil && (*p.Signal < 1 || *p.Signal > 64) || p.ProofReason != "" {
			return false, runs.Fail("INVALID_REQUEST")
		}
		if p.GroupEmpty == nil || !*p.GroupEmpty || (p.Pipes != "drained" && p.Pipes != "gapped") {
			return true, nil
		}
		_, e = tx.Exec(ctx, `UPDATE cosmoformer.start_permissions SET status='stopped',stop_facts=$2,event_digest=decode($3,'hex'),stopped_at=clock_timestamp(),revision=revision+1 WHERE id=$1`, p.Permission, mustJSON(p), runs.Digest(p))
		okay := p.Exit != nil && (*p.Exit == 0 || command == "plan" && *p.Exit == 2)
		limit := int64(4)
		if a.Kind == "apply" {
			limit = 2
		}
		if okay && p.Ordinal < limit {
			out.AttemptState = "ready"
			out.Ready = true
			out.NextCommandOrdinal = int(p.Ordinal + 1)
		} else {
			out.AttemptState = "finalizing"
			out.OperationStatus = "finalizing"
		}
	case "not_executed":
		if status != "issued" {
			return true, nil
		}
		if p.PID != nil || p.PGID != nil || p.Exit != nil || p.Signal != nil || p.GroupEmpty != nil || p.Pipes != "" || (p.ProofReason != "local_deadline_expired_discarded" && p.ProofReason != "local_cancel_discarded") {
			return false, runs.Fail("NEVER_STARTED_PROOF_REQUIRED")
		}
		if p.ProofReason == "local_cancel_discarded" {
			var cancelled bool
			if e = tx.QueryRow(ctx, `SELECT cancel_requested FROM cosmoformer.operations WHERE id=$1`, a.OperationID).Scan(&cancelled); e != nil {
				return false, e
			}
			if !cancelled {
				return false, runs.Fail("CANCEL_EVIDENCE_REQUIRED")
			}
		}
		// The exact same process permanently discards the single-use launch state.
		// Local conservative expiry can precede the platform's display deadline.
		_, e = tx.Exec(ctx, `UPDATE cosmoformer.start_permissions SET status='not_executed',stop_facts=$2,event_digest=decode($3,'hex'),stopped_at=clock_timestamp(),revision=revision+1 WHERE id=$1`, p.Permission, mustJSON(p), runs.Digest(p))
		out.AttemptState = "finalizing"
		out.OperationStatus = "finalizing"
	default:
		return false, runs.Fail("COMMAND_UNSUPPORTED")
	}
	return false, e
}

func (c *Coordinator) executionResult(ctx context.Context, tx pgx.Tx, a runs.Assignment, m runs.Message, p runs.ExecutionResult, pre, sealed string, permitted bool, planning *planningEvidence, out *runs.Transition) (bool, error) {
	if p.Commands == nil || p.Artifacts == nil || len(p.Commands) > 4 || len(p.Artifacts) > 128 || p.PreInitDigest != pre || p.ManifestDigest != sealed {
		return false, runs.Fail("RESULT_IDENTITY_MISMATCH")
	}
	if p.Outcome != "success" && p.Outcome != "failure" && p.Outcome != "preparation_failed" && p.Outcome != "cancelled" && p.Outcome != "not_executed" && p.Outcome != "unknown" {
		return false, runs.Fail("INVALID_REQUEST")
	}
	if p.Outcome == "unknown" {
		return true, nil
	}
	if p.Outcome == "preparation_failed" && (permitted || !p.NoStateChild || !p.Reaped || p.EverPermitted || len(p.Commands) > 0) {
		return false, runs.Fail("PREPARATION_STOP_REQUIRED")
	}
	if !permitted && p.Outcome != "preparation_failed" {
		return false, runs.Fail("PREPARATION_STOP_REQUIRED")
	}
	rows, e := tx.Query(ctx, `SELECT ordinal,id::text,status,stop_facts FROM cosmoformer.start_permissions WHERE attempt_id=$1 ORDER BY ordinal`, m.AttemptID)
	if e != nil {
		return false, e
	}
	type issued struct {
		ordinal    int64
		id, status string
		stop       runs.CommandEvent
	}
	permissions := []issued{}
	for rows.Next() {
		var v issued
		var b []byte
		if e = rows.Scan(&v.ordinal, &v.id, &v.status, &b); e != nil {
			rows.Close()
			return false, e
		}
		if v.status == "issued" || v.status == "started" {
			rows.Close()
			return false, runs.Fail("PERMISSION_UNACCOUNTED")
		}
		if json.Unmarshal(b, &v.stop) != nil {
			rows.Close()
			return false, runs.Fail("STOP_EVIDENCE_REQUIRED")
		}
		permissions = append(permissions, v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	if len(permissions) != len(p.Commands) {
		return false, runs.Fail("PERMISSION_UNACCOUNTED")
	}
	never := false
	for i, v := range permissions {
		r := p.Commands[i]
		if v.ordinal != r.Ordinal || v.id != r.Permission || v.status != r.Status || runs.Digest(v.stop) != runs.Digest(r.Stop) || runs.Digest(v.stop.Exit) != runs.Digest(r.Exit) || runs.Digest(v.stop.Signal) != runs.Digest(r.Signal) {
			return true, nil
		}
		never = never || v.status == "not_executed"
	}
	if (p.Outcome == "not_executed") != never {
		return false, runs.Fail("NEVER_STARTED_PROOF_REQUIRED")
	}
	// Every named artifact is this attempt's finalized exact version. Evidence
	// from an earlier preparation attempt cannot substitute for this attestation.
	for _, id := range p.Artifacts {
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifacts WHERE id=$1 AND attempt_id=$2 AND (state='finalized' OR $3::boolean AND state='unavailable') AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=$1))`, id, m.AttemptID, p.Outcome != "success").Scan(&valid); e != nil {
			return false, e
		}
		if !valid {
			return false, runs.Fail("EVIDENCE_REQUIRED")
		}
	}
	var uncertain, cancelled bool
	if e = tx.QueryRow(ctx, `SELECT a.state='uncertain' OR EXISTS(SELECT FROM cosmoformer.recovery_cases rc WHERE target_id=a.target_id AND status='open' AND NOT EXISTS(SELECT FROM cosmoformer.operations ro WHERE ro.id=a.operation_id AND ro.recovery_case_id=rc.id AND ro.recovery_evidence_revision=rc.evidence_revision)),o.cancel_requested FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id WHERE a.id=$1`, m.AttemptID).Scan(&uncertain, &cancelled); e != nil {
		return false, e
	}
	if uncertain {
		return false, runs.Fail("INDEPENDENT_STOP_PROOF_REQUIRED")
	}
	if p.Outcome == "cancelled" && !cancelled {
		return false, runs.Fail("CANCEL_EVIDENCE_REQUIRED")
	}
	if p.Outcome == "success" && !cancelled && a.Kind == "plan" {
		if e = c.finalizePlan(ctx, tx, a, m, p, planning); e != nil {
			return false, e
		}
	}
	if p.Outcome == "success" && a.Kind == "apply" {
		if len(p.Commands) != 2 || sealed == "" || a.Execution.RetainedPlan == nil || sealed != a.Execution.RetainedPlan.ManifestDigest {
			return false, runs.Fail("RESULT_IDENTITY_MISMATCH")
		}
		var attestations bool
		if e = tx.QueryRow(ctx, `SELECT pre_init_spec_artifact_id=ANY($2::uuid[]) AND sealed_manifest_artifact_id=ANY($2::uuid[]) AND (provider_lock_artifact_id IS NULL OR provider_lock_artifact_id=ANY($2::uuid[])) FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID, p.Artifacts).Scan(&attestations); e != nil {
			return false, e
		}
		if !attestations {
			return false, runs.Fail("EVIDENCE_REQUIRED")
		}
		for _, r := range p.Commands {
			if r.Status != "stopped" || r.Exit == nil || *r.Exit != 0 {
				return false, runs.Fail("COMMAND_FAILED")
			}
		}
	}
	if e = c.executionLogFinal(ctx, tx, m.AttemptID, p.LogFinal); e != nil {
		return false, e
	}
	if e = c.exactReservations(ctx, tx, m.AttemptID); e != nil {
		return false, e
	}
	var budgetInstance string
	if e = tx.QueryRow(ctx, `SELECT budget_platform_instance::text FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&budgetInstance); e != nil {
		return false, e
	}
	outcome := p.Outcome
	if cancelled {
		outcome = "cancelled"
	}
	if budgetInstance != c.Instance && outcome == "preparation_failed" {
		outcome = "failure"
	}
	_, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET state='terminal',outcome=$2,closed_at=clock_timestamp(),stop_proof=$3,result_digest=decode($4,'hex') WHERE id=$1`, m.AttemptID, outcome, mustJSON(p), runs.Digest(p))
	if e != nil {
		return false, e
	}
	// Delete only the exact owning attempt; no time or worker-only release.
	if _, e = tx.Exec(ctx, `DELETE FROM cosmoformer.backend_reservations WHERE attempt_id=$1 AND target_id=(SELECT target_id FROM cosmoformer.attempts WHERE id=$1) AND stack_id=$2`, m.AttemptID, a.StackID); e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, `DELETE FROM cosmoformer.worker_reservations WHERE attempt_id=$1 AND worker_id=$2 AND generation=$3 AND agent_instance=$4`, m.AttemptID, m.WorkerID, m.Generation, m.Agent); e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='attempt' AND owner_id=$1 AND released_at IS NULL`, m.AttemptID); e != nil {
		return false, e
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='done',revision=revision+1 WHERE id=$1`, a.WorkItemID); e != nil {
		return false, e
	}
	reason := "EXECUTION_FAILED"
	if outcome == "success" {
		reason = "APPLY_SUCCEEDED"
		if a.Kind == "plan" {
			reason = "PLAN_READY"
			if !planning.Analysis.Changes.HasChanges() {
				reason = "NO_CHANGES"
			}
		}
	}
	if outcome == "preparation_failed" {
		reason = "PREPARATION_FAILED"
	}
	if outcome == "not_executed" {
		reason = "PERMISSION_NOT_EXECUTED"
	}
	if outcome == "cancelled" {
		reason = "CANCELLED"
	}
	if budgetInstance != c.Instance {
		reason = "ATTEMPT_BUDGET_INSTANCE_ENDED"
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET terminal_reason=$2 WHERE id=$1`, a.OperationID, reason); e != nil {
		return false, e
	}
	if outcome != "success" && outcome != "preparation_failed" {
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.plans SET status='unsupported',invalidation_reason=$2,revision=revision+1 WHERE attempt_id=$1 AND status='assembling'`, m.AttemptID, reason); e != nil {
			return false, e
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='plan' AND owner_id IN(SELECT id FROM cosmoformer.plans WHERE attempt_id=$1 AND status='unsupported') AND released_at IS NULL`, m.AttemptID); e != nil {
			return false, e
		}
	}
	out.AttemptState = "terminal"
	out.OperationStatus = "failed"
	if outcome == "not_executed" && a.Kind == "apply" {
		out.OperationStatus = "retired"
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET retired_at=clock_timestamp() WHERE id=$1`, a.OperationID); e != nil {
			return false, e
		}
	}
	if outcome == "success" {
		out.OperationStatus = "succeeded"
	}
	if cancelled || outcome == "cancelled" {
		out.OperationStatus = "cancelled"
	}
	return false, nil
}
func (c *Coordinator) executionLogFinal(ctx context.Context, tx pgx.Tx, attempt string, f *runs.LogFinal) error {
	if f != nil {
		if f.Stream != "console" || f.LastSequence < -1 || f.LastSequence > 9007199254740991 || f.Complete && f.Unknown {
			return runs.Fail("INVALID_REQUEST")
		}
		var last int64
		var gaps bool
		if e := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),-1),coalesce(bool_or(gap),false) FROM cosmoformer.log_segments WHERE attempt_id=$1 AND stream='console'`, attempt).Scan(&last, &gaps); e != nil {
			return e
		}
		if f.LastSequence != last || f.Complete && gaps {
			return runs.Fail("LOG_FINAL_MISMATCH")
		}
	}
	if f == nil || f.Unknown {
		if _, e := tx.Exec(ctx, `INSERT INTO cosmoformer.log_segments(org_id,attempt_id,stream,sequence,first_offset,last_offset,gap,gap_reason,unknown_extent) SELECT org_id,id,'console',coalesce((SELECT max(sequence)+1 FROM cosmoformer.log_segments WHERE attempt_id=$1),0),coalesce((SELECT max(last_offset) FROM cosmoformer.log_segments WHERE attempt_id=$1),0),coalesce((SELECT max(last_offset) FROM cosmoformer.log_segments WHERE attempt_id=$1),0),true,'worker_final_unknown',true FROM cosmoformer.attempts WHERE id=$1`, attempt); e != nil {
			return e
		}
	}
	if f != nil {
		_, e := tx.Exec(ctx, `UPDATE cosmoformer.attempts SET log_final=$2 WHERE id=$1`, attempt, mustJSON(f))
		return e
	}
	return nil
}

func (c *Coordinator) controlAck(ctx context.Context, tx pgx.Tx, m runs.Message, p runs.ControlAck) error {
	if p.State != "delivered" && p.State != "observed" || p.Ordinal < 0 || !catalog.UUID(p.ID) {
		return runs.Fail("INVALID_REQUEST")
	}
	var kind, state string
	var ordinal int64
	e := tx.QueryRow(ctx, `SELECT kind,state,coalesce(command_ordinal,0) FROM cosmoformer.control_requests WHERE id=$1 AND attempt_id=$2 AND worker_id=$3 AND generation=$4 AND agent_instance=$5`, p.ID, m.AttemptID, m.WorkerID, m.Generation, m.Agent).Scan(&kind, &state, &ordinal)
	if e != nil || ordinal != p.Ordinal {
		return runs.Fail("CONTROL_IDENTITY_MISMATCH")
	}
	if p.Signal != "" && (kind == "force_stop" && p.Signal != "SIGKILL" || kind != "force_stop" && p.Signal != "SIGINT") {
		return runs.Fail("CONTROL_IDENTITY_MISMATCH")
	}
	if state == "observed" && p.State != "observed" {
		return runs.Fail("CONTROL_ORDER")
	}
	var eventRowID string
	if p.StopEvent != "" {
		// The wire reference is the worker's message_id, not the private row ID.
		if !catalog.UUID(p.StopEvent) {
			return runs.Fail("STOP_EVIDENCE_REQUIRED")
		}
		var sequence int64
		var acceptedDigest, stopFacts []byte
		e = tx.QueryRow(ctx, `SELECT event.id::text,event.sequence,event.digest,permission.stop_facts
   FROM cosmoformer.attempt_messages event
   JOIN cosmoformer.start_permissions permission ON permission.attempt_id=event.attempt_id AND permission.ordinal=event.command_ordinal
   WHERE event.message_id=$1 AND event.attempt_id=$2 AND event.kind='command-events' AND event.command_ordinal=$3 AND permission.status='stopped'`, p.StopEvent, m.AttemptID, p.Ordinal).Scan(&eventRowID, &sequence, &acceptedDigest, &stopFacts)
		if errors.Is(e, pgx.ErrNoRows) {
			return runs.Fail("STOP_EVIDENCE_REQUIRED")
		}
		if e != nil {
			return e
		}
		var stopped runs.CommandEvent
		if runs.Decode(stopFacts, &stopped) != nil || stopped.Event != "stopped" || stopped.Ordinal != p.Ordinal {
			return runs.Fail("STOP_EVIDENCE_REQUIRED")
		}
		// Responses alone do not identify which transition accepted the stop.
		// Rebuild the named event's immutable envelope and compare its stored digest
		// with the exact accepted stop facts. A spawn message for a now-stopped
		// permission must never satisfy this check.
		original := m
		original.MessageID = p.StopEvent
		original.Seq = sequence
		if !identity.Equal(acceptedDigest, nativeMessageDigest("command-events", original, &stopped)) {
			return runs.Fail("STOP_EVIDENCE_REQUIRED")
		}
	}
	_, e = tx.Exec(ctx, `UPDATE cosmoformer.control_requests SET state=$2,observed_event_id=coalesce($3::uuid,observed_event_id),signal_sent=coalesce(signal_sent,$4::text),delivered_at=coalesce(delivered_at,clock_timestamp()),observed_at=CASE WHEN $2='observed' THEN coalesce(observed_at,clock_timestamp()) ELSE observed_at END,revision=revision+1 WHERE id=$1`, p.ID, p.State, nullable(eventRowID), nullable(p.Signal))
	return e
}
