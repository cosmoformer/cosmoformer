package coordinator

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

type planningEvidence struct {
	Managed                        []runs.EnvSecret
	Records                        []evidence.Artifact
	Manifest                       runs.Manifest
	Analysis                       plan.Analysis
	Raw, Schema, Changes, Findings []byte
}

func (c *Coordinator) readPlanningEvidence(ctx context.Context, token string, m runs.Message, p runs.ExecutionResult) (*planningEvidence, error) {
	out := &planningEvidence{}
	kinds := map[string]evidence.Artifact{}
	bodies := map[string][]byte{}
	// Resolve kinds under current ownership, then verify every exact version
	// outside the gate. No path accepts a worker-supplied kind substitution.
	for _, id := range p.Artifacts {
		var kind string
		e := c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, a runs.Assignment) error {
			return tx.QueryRow(ctx, `SELECT kind FROM cosmoformer.artifacts WHERE id=$1 AND attempt_id=$2 AND plan_id=(SELECT id FROM cosmoformer.plans WHERE attempt_id=$2 AND status='assembling')`, id, m.AttemptID).Scan(&kind)
		})
		if e != nil {
			return nil, runs.Fail("EVIDENCE_REQUIRED")
		}
		if _, ok := kinds[kind]; ok {
			return nil, runs.Fail("EVIDENCE_AMBIGUOUS")
		}
		record, b, e := c.readExecutionArtifact(ctx, token, m, id, kind)
		if e != nil {
			return nil, e
		}
		kinds[kind] = record
		bodies[kind] = b
		out.Records = append(out.Records, record)
	}
	for _, kind := range []string{"pre_init_spec", "manifest", "binary_plan", "raw_plan_json", "provider_schema", "normalized_changes", "findings"} {
		if _, ok := kinds[kind]; !ok || len(bodies[kind]) == 0 {
			return nil, runs.Fail("EVIDENCE_REQUIRED")
		}
	}
	if canonicalEvidence(bodies["manifest"], &out.Manifest) != nil || runs.Digest(out.Manifest) != p.ManifestDigest {
		return nil, runs.Fail("MANIFEST_STAGE")
	}
	var pre runs.PreInitSpec
	if canonicalEvidence(bodies["pre_init_spec"], &pre) != nil || runs.Digest(pre) != p.PreInitDigest || runs.Digest(pre.ReviewedInputs) != runs.Digest(out.Manifest.ReviewedInputs) {
		return nil, runs.Fail("MANIFEST_STAGE")
	}
	if out.Manifest.Lock.Present {
		if _, ok := kinds["provider_lock"]; !ok {
			return nil, runs.Fail("PROVIDER_LOCK_REQUIRED")
		}
	}
	if e := runs.VerifyProviderLock(&out.Manifest, bodies["provider_lock"]); e != nil {
		return nil, e
	}
	if canonicalEvidence(bodies["normalized_changes"], &out.Analysis) != nil || out.Analysis.Changes == nil {
		return nil, runs.Fail("PLAN_ANALYSIS_INVALID")
	}
	var findings []plan.Finding
	if canonicalEvidence(bodies["findings"], &findings) != nil || runs.Digest(findings) != runs.Digest(out.Analysis.Changes.Findings) {
		return nil, runs.Fail("PLAN_ANALYSIS_INVALID")
	}
	for kind, ordinal := range map[string]int64{"pre_init_spec": 0, "manifest": 1, "provider_lock": 1, "provider_schema": 2, "binary_plan": 3, "raw_plan_json": 4, "normalized_changes": 4, "findings": 4} {
		if record, ok := kinds[kind]; ok {
			var actual int64
			e := c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, _ runs.Assignment) error {
				return tx.QueryRow(ctx, `SELECT command_ordinal FROM cosmoformer.artifacts WHERE id=$1`, record.ID).Scan(&actual)
			})
			if e != nil || actual != ordinal {
				return nil, runs.Fail("COMMAND_EVIDENCE_REQUIRED")
			}
		}
	}
	out.Raw = bodies["raw_plan_json"]
	out.Schema = bodies["provider_schema"]
	out.Changes = bodies["normalized_changes"]
	out.Findings = bodies["findings"]
	var assigned runs.Assignment
	var secrets []runs.EnvSecret
	if c.ManagedSecrets == nil {
		return nil, runs.Fail("SECRET_VALIDATION_UNAVAILABLE")
	}
	e := c.WithAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, w Worker, a runs.Assignment) error {
		assigned = a
		var e error
		secrets, e = c.ManagedSecrets(ctx, tx, w.OrgID, *a.Execution)
		return e
	})
	if e != nil {
		return nil, e
	}
	if plan.ValidateManagedSecrets(out.Changes, secrets) != nil || plan.ValidateManagedSecrets(out.Findings, secrets) != nil {
		return nil, runs.Fail("NORMALIZED_SECRET_DETECTED")
	}
	out.Managed = secrets
	registry := &evidence.Registry{}
	for _, v := range secrets {
		terms, e := evidence.SecretTerms(v.Value, len(v.Destination) > 7 && v.Destination[:7] == "TF_VAR_")
		if e != nil {
			return nil, runs.Fail("SECRET_VALUE_UNSUPPORTED")
		}
		if registry.Register(terms...) != nil {
			return nil, runs.Fail("SECRET_VALUE_UNSUPPORTED")
		}
	}
	options, _, e := plan.Allocate(out.Raw, plan.Options{Engine: assigned.Execution.Engine.Name, Version: assigned.Execution.Engine.Version, ProfileID: assigned.Execution.ProfileID, Providers: out.Manifest.Providers, Secrets: registry}, &out.Analysis.Identities, nil)
	if e != nil {
		return nil, runs.Fail("PLAN_ANALYSIS_INVALID")
	}
	// Full native validation against the exact retained JSON/schema, independently
	// of the sanitized worker artifact. Worker-only AWS/Git masks may be broader.
	verified, e := plan.Analyze(out.Raw, out.Schema, options)
	if e != nil {
		return nil, runs.Fail("PLAN_ANALYSIS_INVALID")
	}
	if plan.VerifySanitized(verified, out.Analysis.Changes) != nil {
		return nil, runs.Fail("PLAN_ANALYSIS_MISMATCH")
	}
	if out.Analysis.Changes.ProfileID != assigned.Execution.ProfileID || out.Analysis.Changes.RuleVersion != assigned.Execution.RuleVersion {
		return nil, runs.Fail("PLAN_ANALYSIS_MISMATCH")
	}
	return out, nil
}
func (c *Coordinator) finalizePlan(ctx context.Context, tx pgx.Tx, a runs.Assignment, m runs.Message, p runs.ExecutionResult, ev *planningEvidence) error {
	if ev == nil || a.Execution == nil || a.Kind != "plan" || len(p.Commands) != 4 {
		return runs.Fail("EVIDENCE_REQUIRED")
	}
	for i, r := range p.Commands {
		if r.Ordinal != int64(i+1) || r.Status != "stopped" || r.Exit == nil || (*r.Exit != 0 && (i != 2 || *r.Exit != 2)) {
			return runs.Fail("COMMAND_FAILED")
		}
	}
	if e := c.executionAuthority(ctx, tx, a); e != nil {
		return e
	}
	if e := c.canLaunch(ctx, tx, m.AttemptID, a.OperationID); e != nil {
		return e
	}
	if _, e := c.executionBudget(ctx, tx, m.AttemptID, true); e != nil {
		return e
	}
	if e := c.exactReservations(ctx, tx, m.AttemptID); e != nil {
		return e
	}
	if e := c.verifyExecutionEvidence(ctx, tx, m.AttemptID, ev.Records); e != nil {
		return e
	}
	var epoch int64
	var preID, manifestID, lockID string
	if e := tx.QueryRow(ctx, `SELECT cosmoformer.execution_target_epoch(id),pre_init_spec_artifact_id::text,sealed_manifest_artifact_id::text,coalesce(provider_lock_artifact_id::text,'') FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&epoch, &preID, &manifestID, &lockID); e != nil {
		return e
	}
	if e := ev.Manifest.ReviewedInputs.Validate(*a.Execution, a.StackID, epoch); e != nil {
		return e
	}
	ids := map[string]string{}
	for _, r := range ev.Records {
		ids[r.Kind] = r.ID
	}
	if ids["pre_init_spec"] != preID || ids["manifest"] != manifestID || ids["provider_lock"] != lockID {
		return runs.Fail("MANIFEST_STAGE")
	}
	// Independently decrypt current managed inputs under the final gate. Plaintext
	// never enters stored DTOs/replay, and cannot race a revision/grant revocation.
	if c.ManagedSecrets == nil {
		return runs.Fail("SECRET_VALIDATION_UNAVAILABLE")
	}
	secrets, e := c.ManagedSecrets(ctx, tx, "", *a.Execution)
	if e != nil {
		return e
	}
	if len(secrets) != len(ev.Managed) {
		return runs.Fail("INPUTS_CHANGED")
	}
	for i, value := range secrets {
		if value != ev.Managed[i] {
			return runs.Fail("INPUTS_CHANGED")
		}
	}

	// Holds must already protect each exact finalized version before usability.
	for _, r := range ev.Records {
		var held bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifact_holds h JOIN cosmoformer.plans p ON p.id=h.owner_id WHERE h.artifact_id=$1 AND h.owner_kind='plan' AND h.released_at IS NULL AND p.attempt_id=$2 AND p.status='assembling')`, r.ID, m.AttemptID).Scan(&held); e != nil {
			return e
		}
		if !held {
			return runs.Fail("EVIDENCE_HOLD_REQUIRED")
		}
	}
	ttl := c.PlanTTLHours
	if ttl < 1 || ttl > 720 {
		return runs.Fail("PLAN_TTL_INVALID")
	}
	tag, e := tx.Exec(ctx, `WITH ready AS (SELECT clock_timestamp() AS at) UPDATE cosmoformer.plans SET status=CASE WHEN provenance='recovery' THEN 'recovery_only' ELSE 'ready' END,manifest_artifact_id=$2,manifest_digest=decode($3,'hex'),ready_at=ready.at,expires_at=ready.at+$4::bigint*interval '1 hour',revision=revision+1 FROM ready WHERE attempt_id=$1 AND status='assembling' AND state_epoch=$5 AND source_facts_revision=$6`, m.AttemptID, manifestID, p.ManifestDigest, ttl, epoch, a.Source.FactsRevision)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return runs.Fail("PLAN_NOT_ASSEMBLING")
	}
	return nil
}
