package coordinator

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

// ApplyEligibility grants no authority by itself. Call under the shared gate,
// alongside current requester authority and (for each new start) its own source
// observation. The immutable original result defines the exact artifact set.
func ApplyEligibility(ctx context.Context, tx pgx.Tx, stack string, s runs.ExecutionSpec) error {
	p := s.RetainedPlan
	if p == nil || p.Revision < 1 || !runs.ValidDigest(p.ManifestDigest) {
		return runs.Fail("PLAN_IDENTITY_CHANGED")
	}
	if !p.Valid() {
		return runs.Fail("EVIDENCE_UNAVAILABLE")
	}
	var attempt, status, provenance, digest string
	var revision int64
	var unexpired, pr bool
	var original runs.ExecutionSpec
	var ids []string
	e := tx.QueryRow(ctx, `SELECT p.attempt_id::text,p.status,p.provenance,p.revision,encode(p.manifest_digest,'hex'),p.expires_at>clock_timestamp(),ss.kind='pr',a.execution_spec,a.stop_proof->'artifact_ids' FROM cosmoformer.plans p JOIN cosmoformer.attempts a ON a.id=p.attempt_id JOIN cosmoformer.operations o ON o.id=p.operation_id JOIN cosmoformer.runs r ON r.id=o.run_id JOIN cosmoformer.source_subjects ss ON ss.id=r.source_subject_id WHERE p.id=$1 AND p.stack_id=$2`, p.ID, stack).Scan(&attempt, &status, &provenance, &revision, &digest, &unexpired, &pr, &original, &ids)
	if errors.Is(e, pgx.ErrNoRows) {
		return runs.Fail("PLAN_NOT_READY")
	}
	if e != nil {
		return e
	}
	if provenance != "normal" {
		return runs.Fail("RECOVERY_REQUIRED")
	}
	if status != "ready" {
		return runs.Fail("PLAN_INVALIDATED")
	}
	if !unexpired {
		return runs.Fail("PLAN_EXPIRED")
	}
	if revision != p.Revision || digest != p.ManifestDigest {
		return runs.Fail("PLAN_IDENTITY_CHANGED")
	}
	copy := s
	copy.RetainedPlan = nil
	if runs.Digest(copy) != runs.Digest(original) {
		return runs.Fail("INPUTS_CHANGED")
	}
	if pr && !s.Review.AllowPRApply {
		return runs.Fail("PR_APPLY_DISABLED")
	}
	if e = ApprovalInputs(ctx, tx, stack, attempt, original); e != nil {
		return e
	}
	var count int64
	if e = tx.QueryRow(ctx, `SELECT cosmoformer.count_plan_approvals($1)`, p.ID).Scan(&count); e != nil {
		return e
	}
	if count < int64(s.Review.Minimum) {
		return runs.Fail("APPROVALS_REQUIRED")
	}
	if runs.Digest(ids) != runs.Digest(p.Artifacts) || len(ids) < 7 || len(ids) > 8 || len(p.Records) != len(ids) {
		return runs.Fail("EVIDENCE_UNAVAILABLE")
	}
	kinds := map[string]bool{}
	for i, id := range ids {
		r := p.Records[i]
		if id != r.ID || r.Kind == "manifest" && r.Hash != p.ManifestDigest || kinds[r.Kind] || evidence.Limit(r.Kind) == 0 || r.Size < 0 || r.Size > evidence.Limit(r.Kind) || !runs.ValidDigest(r.Hash) || r.Version == "" {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
		kinds[r.Kind] = true
		var valid bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifacts a WHERE a.id=$1 AND a.attempt_id=$2 AND a.plan_id=$3 AND a.kind=$4 AND a.state='finalized' AND a.version_id=$5 AND encode(a.sha256,'hex')=$6 AND a.byte_count=$7 AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=a.id) AND EXISTS(SELECT FROM cosmoformer.artifact_holds WHERE artifact_id=a.id AND released_at IS NULL AND owner_kind='plan' AND owner_id=$3))`, id, attempt, p.ID, r.Kind, r.Version, r.Hash, r.Size).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
	}
	for _, kind := range []string{"pre_init_spec", "manifest", "binary_plan", "raw_plan_json", "provider_schema", "normalized_changes", "findings"} {
		if !kinds[kind] {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
	}
	return nil
}

func HoldApply(ctx context.Context, tx pgx.Tx, s runs.ExecutionSpec, ownerKind, owner string) error {
	for _, id := range s.RetainedPlan.Artifacts {
		tag, e := tx.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT org_id,id,$2,$3,'saved_plan_apply' FROM cosmoformer.artifacts a WHERE id=$1 AND state='finalized' AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=a.id) ON CONFLICT DO NOTHING`, id, ownerKind, owner)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
	}
	return nil
}

// Prior history is intentionally stricter than a terminal operation status.
// Only unclaimed retirement or a positively expired, never-used permission may
// produce another Apply operation. Preparation-only failure uses explicit Retry.
func ApplyHistory(ctx context.Context, tx pgx.Tx, plan string) error {
	var live, consumed bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.operations WHERE plan_id=$1 AND status IN ('queued','preparing','executing','finalizing','needs_attention')),EXISTS(SELECT FROM cosmoformer.operations o WHERE o.plan_id=$1 AND NOT (o.status='retired' AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=o.id AND (ever_permitted OR closed_at IS NULL OR outcome<>'preparation_failed')) OR o.status IN ('failed','retired') AND o.terminal_reason='PERMISSION_NOT_EXECUTED' AND EXISTS(SELECT FROM cosmoformer.attempts a WHERE a.operation_id=o.id AND a.outcome='not_executed' AND a.closed_at IS NOT NULL AND NOT EXISTS(SELECT FROM cosmoformer.start_permissions sp WHERE sp.attempt_id=a.id AND (sp.command='apply' AND sp.status<>'not_executed' OR sp.status NOT IN ('stopped','not_executed') OR sp.status='not_executed' AND sp.stop_facts->>'proof_reason'<>'local_deadline_expired_discarded')))))`, plan).Scan(&live, &consumed)
	if e != nil {
		return e
	}
	if live {
		return runs.Fail("APPLY_ALREADY_PENDING")
	}
	if consumed {
		return runs.Fail("NEW_PLAN_REQUIRED")
	}
	return nil
}

// Verify retained custody outside transactions before new native authority.
// The final gated ApplyEligibility repeats metadata/hold checks after this I/O.
func (c *Coordinator) verifyRetainedApply(ctx context.Context, token string, m runs.Message) error {
	var assigned runs.Assignment
	if e := c.WithAttempt(ctx, token, m.Identity, m.AttemptID, func(_ pgx.Tx, _ Worker, a runs.Assignment) error { assigned = a; return nil }); e != nil {
		return e
	}
	if assigned.Kind != "apply" {
		return nil
	}
	if c.Evidence == nil {
		return runs.Fail("EVIDENCE_UNAVAILABLE")
	}
	for _, r := range assigned.Execution.RetainedPlan.Records {
		auth := func(fn func(pgx.Tx) error) error {
			return c.WithAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, a runs.Assignment) error {
				var held bool
				if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifact_holds WHERE artifact_id=$1 AND owner_kind='operation' AND owner_id=$2 AND released_at IS NULL) AND EXISTS(SELECT FROM cosmoformer.artifact_holds WHERE artifact_id=$1 AND owner_kind='attempt' AND owner_id=$3 AND released_at IS NULL)`, r.ID, a.OperationID, m.AttemptID).Scan(&held); e != nil {
					return e
				}
				if !held {
					return runs.Fail("EVIDENCE_UNAVAILABLE")
				}
				return fn(tx)
			})
		}
		record, f, e := c.Evidence.Read(ctx, auth, r.ID)
		if e != nil {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
		if r.Kind == "manifest" {
			raw, err := io.ReadAll(io.LimitReader(f, evidence.PlanMax+1))
			var manifest runs.Manifest
			if err != nil || int64(len(raw)) > evidence.PlanMax || canonicalEvidence(raw, &manifest) != nil || runs.Digest(manifest) != assigned.Execution.RetainedPlan.ManifestDigest {
				e = runs.Fail("EVIDENCE_UNAVAILABLE")
			}
			var preID string
			if e == nil {
				e = c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, _ runs.Assignment) error {
					return tx.QueryRow(ctx, `SELECT pre_init_spec_artifact_id::text FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&preID)
				})
			}
			if e == nil {
				_, b, err := c.readExecutionArtifact(ctx, token, m, preID, "pre_init_spec")
				var pre runs.PreInitSpec
				if err != nil || canonicalEvidence(b, &pre) != nil || runs.Digest(pre.ReviewedInputs) != runs.Digest(manifest.ReviewedInputs) || pre.RetainedDigest != r.Hash || pre.LockHash != manifest.Lock.Hash || (pre.LockOrigin == "retained") != manifest.Lock.Present {
					e = runs.Fail("REPREPARE_REQUIRED")
				}
			}
		}
		f.Close()
		os.Remove(f.Name())
		if e != nil {
			return e
		}
		if record.Hash != r.Hash || record.Size != r.Size || record.Version != r.Version || record.Kind != r.Kind {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
	}
	return nil
}

func applyOperationHolds(ctx context.Context, tx pgx.Tx, s runs.ExecutionSpec, operation string) error {
	if s.RetainedPlan == nil || len(s.RetainedPlan.Artifacts) == 0 {
		return runs.Fail("PLAN_IDENTITY_CHANGED")
	}
	var count int
	if e := tx.QueryRow(ctx, `SELECT count(DISTINCT artifact_id) FROM cosmoformer.artifact_holds WHERE owner_kind='operation' AND owner_id=$1 AND released_at IS NULL AND artifact_id=ANY($2::uuid[])`, operation, s.RetainedPlan.Artifacts).Scan(&count); e != nil {
		return e
	}
	if count != len(s.RetainedPlan.Artifacts) {
		return runs.Fail("EVIDENCE_UNAVAILABLE")
	}
	return nil
}
