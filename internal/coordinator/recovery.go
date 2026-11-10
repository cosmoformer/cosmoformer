package coordinator

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

// ProtocolStopped accepts only positive facts already reconciled by this exact
// agent's ordered protocol. Issued/lost-response permissions remain unresolved.
func ProtocolStopped(ctx context.Context, tx pgx.Tx, attempt string) (bool, error) {
	var proven bool
	e := tx.QueryRow(ctx, `SELECT NOT stop_conflicted AND (closed_at IS NOT NULL AND stop_proof IS NOT NULL OR ever_permitted AND EXISTS(SELECT FROM cosmoformer.start_permissions WHERE attempt_id=a.id)) AND NOT EXISTS(SELECT FROM cosmoformer.start_permissions p WHERE p.attempt_id=a.id AND (p.status NOT IN ('stopped','not_executed') OR p.stop_facts IS NULL OR p.status='stopped' AND (p.stop_facts->>'event'<>'stopped' OR p.stop_facts->>'group_empty'<>'true' OR p.stop_facts->>'pipes' NOT IN ('drained','gapped')))) FROM cosmoformer.attempts a WHERE id=$1`, attempt).Scan(&proven)
	return proven, e
}

// StopRecovery must run under the caller's current Apply authorization and gate.
// Manual authority is recorded separately; it never fabricates native events.
func StopRecovery(ctx context.Context, tx pgx.Tx, attempt string, proof runs.StopProof) error {
	if proof.Method == "agent" {
		ok, e := ProtocolStopped(ctx, tx, attempt)
		if e != nil {
			return e
		}
		if !ok || proof.Reference != attempt {
			return runs.Fail("INDEPENDENT_STOP_PROOF_REQUIRED")
		}
	}
	if _, e := tx.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT ar.org_id,ar.id,'recovery',r.id,'execution_uncertain' FROM cosmoformer.artifacts ar JOIN cosmoformer.attempts a ON a.id=ar.attempt_id JOIN cosmoformer.recovery_cases r ON r.target_id=a.target_id AND r.status='open' WHERE r.target_id=(SELECT target_id FROM cosmoformer.attempts WHERE id=$1) AND ar.state IN ('uploading','finalized') AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=ar.id) ON CONFLICT DO NOTHING`, attempt); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `UPDATE cosmoformer.plans SET status='unsupported',invalidation_reason='RECOVERY_REQUIRED',revision=revision+1 WHERE attempt_id=$1 AND status='assembling'`, attempt); e != nil {
		return e
	}
	var needsGap bool
	if e := tx.QueryRow(ctx, `SELECT closed_at IS NULL AND log_final IS NULL FROM cosmoformer.attempts WHERE id=$1`, attempt).Scan(&needsGap); e != nil {
		return e
	}
	if needsGap {
		if e := (&Coordinator{}).executionLogFinal(ctx, tx, attempt, nil); e != nil {
			return e
		}
	}
	// Fence the old process before releasing its exact reservations. Pausing,
	// fencing and release follow proof; neither pause nor token expiry is proof.
	if _, e := tx.Exec(ctx, `UPDATE cosmoformer.workers w SET paused=true,generation=w.generation+1,agent_instance=NULL,revision=w.revision+1 FROM cosmoformer.attempts a WHERE a.id=$1 AND w.id=a.worker_id AND w.generation=a.generation AND w.agent_instance=a.agent_instance AND NOT EXISTS(SELECT FROM cosmoformer.worker_reservations other WHERE other.worker_id=w.id AND other.attempt_id<>a.id)`, attempt); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `UPDATE cosmoformer.attempts SET state='terminal',outcome='unknown',closed_at=clock_timestamp(),stop_proof=$2,revision=revision+1 WHERE id=$1 AND closed_at IS NULL`, attempt, mustJSON(proof)); e != nil {
		return e
	}
	// Issued permissions are accounted for by independent host/operator authority;
	// their original spawn/stop facts remain intact, and no permit is redelivered.
	if _, e := tx.Exec(ctx, `UPDATE cosmoformer.start_permissions SET status='stopped',stopped_at=coalesce(stopped_at,clock_timestamp()),revision=revision+1 WHERE attempt_id=$1 AND status IN ('issued','started')`, attempt); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `DELETE FROM cosmoformer.worker_reservations r USING cosmoformer.attempts a WHERE a.id=$1 AND r.attempt_id=a.id AND r.worker_id=a.worker_id AND r.generation=a.generation AND r.agent_instance=a.agent_instance`, attempt); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `DELETE FROM cosmoformer.backend_reservations r USING cosmoformer.attempts a WHERE a.id=$1 AND r.attempt_id=a.id AND r.target_id=a.target_id AND r.stack_id=a.stack_id`, attempt); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='done',revision=revision+1 WHERE id=(SELECT work_item_id FROM cosmoformer.attempts WHERE id=$1)`, attempt); e != nil {
		return e
	}
	_, e := tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='attempt' AND owner_id=$1 AND released_at IS NULL`, attempt)
	return e
}

func nullableRevision(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
