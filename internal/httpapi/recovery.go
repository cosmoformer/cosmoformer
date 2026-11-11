package httpapi

import (
	"context"
	"encoding/json"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type RecoveryInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_revision" minimum:"1" maximum:"9007199254740991"`
		runs.RecoveryEvidence
	}
}
type RecoveryRevisionInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_evidence_revision" minimum:"1" maximum:"9007199254740991"`
	}
}
type RecoveryPublic struct {
	ID               string                 `json:"id" format:"uuid"`
	Stack            string                 `json:"stack_id" format:"uuid"`
	Target           string                 `json:"target_id" format:"uuid"`
	Attempt          string                 `json:"origin_attempt_id"`
	Status           string                 `json:"status"`
	Revision         int64                  `json:"revision"`
	EvidenceRevision int64                  `json:"evidence_revision"`
	Stop             bool                   `json:"stop_verified"`
	Reconciled       bool                   `json:"reconciliation_complete"`
	Outcome          string                 `json:"infrastructure_outcome"`
	Authority        string                 `json:"stop_authority"`
	Evidence         *runs.RecoveryEvidence `json:"evidence,omitempty"`
	Count            int                    `json:"counted_approvals"`
	Minimum          int                    `json:"minimum"`
	OwnCounted       bool                   `json:"own_counted"`
	CanEvidence      bool                   `json:"can_submit_evidence"`
	CanApprove       bool                   `json:"can_approve"`
	CanClear         bool                   `json:"can_clear"`
	CanPlan          bool                   `json:"can_plan"`
	Event            string                 `json:"vote_event_id,omitempty"`
}
type RecoveryOutput struct{ Body RecoveryPublic }
type recoverySubject struct {
	RecoveryPublic
	Project, Environment string
}

func recoveryFor(ctx context.Context, tx pgx.Tx, actor store.Actor, id string, action access.Action) (recoverySubject, error) {
	var r recoverySubject
	e := tx.QueryRow(ctx, `SELECT r.id::text,r.stack_id::text,r.target_id::text,coalesce(r.origin_attempt_id::text,''),r.status,r.revision,r.evidence_revision,r.stop_verified,r.reconciliation_complete,r.evidence,s.project_id::text,s.environment_id::text,greatest(p.minimum_approvals,coalesce((sr.effective_config->'declaration'->'approvals'->>'minimum')::int,1),coalesce((a.execution_spec->'review_policy'->>'effective_minimum')::int,(a.execution_spec->'review_policy'->>'minimum')::int,1)) FROM cosmoformer.recovery_cases r JOIN cosmoformer.stacks s ON s.id=r.stack_id JOIN cosmoformer.projects p ON p.id=s.project_id LEFT JOIN cosmoformer.attempts a ON a.id=r.origin_attempt_id LEFT JOIN cosmoformer.stack_revisions sr ON sr.id=s.effective_revision_id WHERE r.id=$1`, id).Scan(&r.ID, &r.Stack, &r.Target, &r.Attempt, &r.Status, &r.Revision, &r.EvidenceRevision, &r.Stop, &r.Reconciled, newJSON(&r.Evidence), &r.Project, &r.Environment, &r.Minimum)
	if e == pgx.ErrNoRows {
		return r, store.ErrNotFound
	}
	if e != nil {
		return r, e
	}
	g, e := store.GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return r, e
	}
	// A separate approver can inspect the evidence subject without borrowing raw
	// evidence permission or a deployment grant.
	if !access.Allows(g, r.Project, r.Environment, action) && !(action == access.View && access.Allows(g, r.Project, r.Environment, access.Approve)) {
		return r, store.ErrForbidden
	}
	if r.Evidence != nil && r.Evidence.Stop.Method == "" {
		r.Evidence = nil
	}
	r.Outcome = "unknown"
	r.Authority = "unverified"
	if r.Evidence != nil && r.Evidence.Stop.Method != "" {
		r.Authority = "agent_protocol"
		if r.Evidence.Stop.Method != "agent" {
			r.Authority = "manual_" + r.Evidence.Stop.Method
		}
	}
	r.CanEvidence = r.Status == "open" && access.Allows(g, r.Project, r.Environment, access.Apply)
	r.CanApprove = r.Status == "open" && r.Stop && r.Reconciled && access.Allows(g, r.Project, r.Environment, access.Approve)
	r.CanPlan = r.Status == "open" && r.Stop && (access.Allows(g, r.Project, r.Environment, access.Plan) || access.Allows(g, r.Project, r.Environment, access.Apply))
	e = tx.QueryRow(ctx, `SELECT count(*)::int,coalesce(bool_or(v.account_id=$2),false) FROM cosmoformer.recovery_votes v WHERE v.case_id=$1 AND v.approved AND v.evidence_revision=$3 AND cosmoformer.requester_enabled(v.account_id) AND EXISTS(SELECT FROM cosmoformer.memberships m WHERE m.org_id=v.org_id AND m.account_id=v.account_id) AND EXISTS(SELECT FROM cosmoformer.team_members tm JOIN cosmoformer.grants g ON (g.org_id,g.team_id)=(tm.org_id,tm.team_id) WHERE tm.account_id=v.account_id AND g.org_id=v.org_id AND g.project_id=$4 AND (g.environment_id IS NULL OR g.environment_id=$5) AND g.role='approver' AND g.revoked_at IS NULL)`, r.ID, actor.ID, r.EvidenceRevision, r.Project, r.Environment).Scan(&r.Count, &r.OwnCounted)
	if e != nil {
		return r, e
	}
	var unresolved bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.attempts WHERE target_id=$1 AND closed_at IS NULL) OR EXISTS(SELECT FROM cosmoformer.operations WHERE recovery_case_id=$2 AND status IN ('queued','preparing','executing','finalizing','needs_attention'))`, r.Target, r.ID).Scan(&unresolved); e != nil {
		return r, e
	}
	r.CanClear = r.CanEvidence && r.Stop && r.Reconciled && r.Count >= r.Minimum && !unresolved
	var maintenance, unreliable bool
	if e = tx.QueryRow(ctx, `SELECT maintenance,clock_unreliable FROM cosmoformer.installation`).Scan(&maintenance, &unreliable); e != nil {
		return r, e
	}
	if maintenance || unreliable {
		r.CanApprove, r.CanClear, r.CanPlan = false, false, false
	}
	if r.Evidence != nil && !access.Allows(g, r.Project, r.Environment, access.RawEvidenceRead) {
		r.Evidence.Stop.Reference = ""
		r.Evidence.Reconciliation.References = []string{}
	}
	return r, nil
}

// Recovery receipts retain only immutable event identity. Replays project the
// subject again under current grants, evidence and policy, never a cached DTO.
func mutateRecovery(a *Server, ctx context.Context, org, id string, action access.Action, fn func(pgx.Tx, store.Actor) (*RecoveryOutput, error)) (*RecoveryOutput, error) {
	var out *RecoveryOutput
	e := a.tx(ctx, org, func(tx pgx.Tx, actor store.Actor) error {
		r, e := recoveryFor(ctx, tx, actor, id, action)
		if e != nil {
			return e
		}
		receipt, found, e := browserReceipt[RecoveryOutput](ctx, tx, actor)
		if e != nil {
			return e
		}
		if found {
			out = &RecoveryOutput{Body: r.RecoveryPublic}
			if receipt != nil {
				out.Body.Event = receipt.Body.Event
			}
			return nil
		}
		out, e = fn(tx, actor)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(&RecoveryOutput{Body: RecoveryPublic{Event: out.Body.Event}})
		if e != nil {
			return e
		}
		d := request(ctx)
		_, e = tx.Exec(ctx, `SELECT cosmoformer.save_browser_request($1,$2,$3,$4::jsonb,false)`, actor.ID, d.Key, d.Digest, string(raw))
		return e
	})
	return out, e
}

func (a *Server) registerRecovery(api huma.API) {
	a.registerAttemptStop(api)
	register(api, "GET", "/organizations/{org}/recoveries/{id}", "read-recovery", func(ctx context.Context, in *OrgIDPath) (*RecoveryOutput, error) {
		out := &RecoveryOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			r, e := recoveryFor(ctx, tx, actor, in.ID, access.View)
			out.Body = r.RecoveryPublic
			return e
		})
		return out, e
	})
	register(api, "POST", "/organizations/{org}/attempts/{id}/recovery", "escalate-attempt", func(ctx context.Context, in *CancelInput) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return scopeAttempt(ctx, tx, actor, in.ID) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			var revision int64
			if e := tx.QueryRow(ctx, `SELECT revision FROM cosmoformer.attempts WHERE id=$1`, in.ID).Scan(&revision); e != nil {
				return nil, e
			}
			if revision != in.Body.Revision {
				return nil, store.ErrConflict
			}
			if _, e := tx.Exec(ctx, `SELECT cosmoformer.execution_attention($1)`, in.ID); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "attempt.recovery", in.ID)
		})
	})
	register(api, "PATCH", "/organizations/{org}/recoveries/{id}/evidence", "recovery-evidence", func(ctx context.Context, in *RecoveryInput) (*RecoveryOutput, error) {
		return mutateRecovery(a, ctx, in.Org, in.ID, access.Apply, func(tx pgx.Tx, actor store.Actor) (*RecoveryOutput, error) {
			r, e := recoveryFor(ctx, tx, actor, in.ID, access.Apply)
			if e != nil {
				return nil, e
			}
			if r.Status != "open" || r.Revision != in.Body.Revision {
				return nil, store.ErrConflict
			}
			ev := in.Body.RecoveryEvidence
			if e = ev.Validate(); e != nil {
				return nil, problem(ctx, 422, "Supply bounded, non-secret stop and reconciliation statements and protected references.")
			}
			if e = coordinator.StopRecovery(ctx, tx, r.Attempt, ev.Stop); e != nil {
				return nil, workerError(ctx, e)
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.recovery_cases SET evidence=$2,stop_verified=true,reconciliation_complete=$3,revision=revision+1 WHERE id=$1`, r.ID, jsonBytes(ev), ev.Reconciliation.Complete); e != nil {
				return nil, e
			}
			var version int64
			if e = tx.QueryRow(ctx, `SELECT evidence_revision FROM cosmoformer.recovery_cases WHERE id=$1`, r.ID).Scan(&version); e != nil {
				return nil, e
			}
			if version != r.EvidenceRevision {
				if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations o SET status='retired',terminal_reason='RECOVERY_EVIDENCE_CHANGED',revision=revision+1 WHERE o.recovery_case_id=$1 AND o.recovery_evidence_revision<>$2 AND o.status='queued' AND NOT EXISTS(SELECT FROM cosmoformer.attempts a WHERE a.operation_id=o.id AND a.closed_at IS NULL)`, r.ID, version); e != nil {
					return nil, e
				}
				if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='retired',revision=revision+1 WHERE status='queued' AND operation_id IN(SELECT id FROM cosmoformer.operations WHERE recovery_case_id=$1 AND status='retired')`, r.ID); e != nil {
					return nil, e
				}
			}
			authority := "manual_" + ev.Stop.Method
			if ev.Stop.Method == "agent" {
				authority = "agent_protocol"
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.recovery_evidence_versions(org_id,case_id,evidence_revision,submitted_by,evidence,authority) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, in.Org, r.ID, version, actor.ID, jsonBytes(ev), authority); e != nil {
				return nil, e
			}
			if e = store.Audit(ctx, tx, in.Org, actor.ID, "recovery.evidence", r.ID); e != nil {
				return nil, e
			}
			r, e = recoveryFor(ctx, tx, actor, r.ID, access.Apply)
			return &RecoveryOutput{Body: r.RecoveryPublic}, e
		})
	})
	register(api, "POST", "/organizations/{org}/recoveries/{id}/approvals", "approve-recovery", func(ctx context.Context, in *RecoveryRevisionInput) (*RecoveryOutput, error) {
		return mutateRecovery(a, ctx, in.Org, in.ID, access.Approve, func(tx pgx.Tx, actor store.Actor) (*RecoveryOutput, error) {
			r, e := recoveryFor(ctx, tx, actor, in.ID, access.Approve)
			if e != nil {
				return nil, e
			}
			if !r.CanApprove || r.EvidenceRevision != in.Body.Revision {
				return nil, store.ErrConflict
			}
			event := runs.ID()
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.approvals(id,org_id,subject_kind,subject_id,subject_revision,account_id,action,accepted_at) VALUES($1,$2,'recovery',$3,$4,$5,'approve',clock_timestamp())`, event, in.Org, r.ID, r.EvidenceRevision, actor.ID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.recovery_votes(org_id,case_id,account_id,last_event_id,approved,evidence_revision) VALUES($1,$2,$3,$4,true,$5) ON CONFLICT(org_id,case_id,account_id) DO UPDATE SET last_event_id=excluded.last_event_id,approved=true,evidence_revision=excluded.evidence_revision`, in.Org, r.ID, actor.ID, event, r.EvidenceRevision); e != nil {
				return nil, e
			}
			if e = store.Audit(ctx, tx, in.Org, actor.ID, "recovery.approve", r.ID); e != nil {
				return nil, e
			}
			r, e = recoveryFor(ctx, tx, actor, r.ID, access.Approve)
			r.Event = event
			return &RecoveryOutput{Body: r.RecoveryPublic}, e
		})
	})
	register(api, "POST", "/organizations/{org}/recoveries/{id}/clear", "clear-recovery", func(ctx context.Context, in *RecoveryRevisionInput) (*RecoveryOutput, error) {
		return mutateRecovery(a, ctx, in.Org, in.ID, access.Apply, func(tx pgx.Tx, actor store.Actor) (*RecoveryOutput, error) {
			r, e := recoveryFor(ctx, tx, actor, in.ID, access.Apply)
			if e != nil {
				return nil, e
			}
			if !r.CanClear || r.EvidenceRevision != in.Body.Revision {
				return nil, store.ErrConflict
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.recovery_cases SET status='cleared',cleared_at=clock_timestamp(),cleared_by=$2,revision=revision+1 WHERE id=$1`, r.ID, actor.ID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='failed',terminal_reason='recovery_completed',revision=revision+1 WHERE status='needs_attention' AND id IN(SELECT operation_id FROM cosmoformer.attempts WHERE target_id=$2 AND closed_at IS NOT NULL) AND EXISTS(SELECT FROM cosmoformer.recovery_cases rc WHERE rc.origin_attempt_id=$1 AND rc.target_id=$2 AND rc.status='cleared')`, r.Attempt, r.Target); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='recovery' AND owner_id=$1 AND released_at IS NULL`, r.ID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='operation' AND owner_id IN(SELECT a.operation_id FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id WHERE a.target_id=$2 AND a.closed_at IS NOT NULL AND o.status IN ('failed','succeeded','cancelled','retired')) AND released_at IS NULL AND EXISTS(SELECT FROM cosmoformer.recovery_cases WHERE origin_attempt_id=$1 AND target_id=$2 AND status='cleared')`, r.Attempt, r.Target); e != nil {
				return nil, e
			}
			if e = store.Audit(ctx, tx, in.Org, actor.ID, "recovery.clear", r.ID); e != nil {
				return nil, e
			}
			r, e = recoveryFor(ctx, tx, actor, r.ID, access.Apply)
			return &RecoveryOutput{Body: r.RecoveryPublic}, e
		})
	})
	// Source-only preparation has no backend case. Manual stop closes only this
	// exact worker ownership and requires an entirely new plan request.
	register(api, "PATCH", "/organizations/{org}/attempts/{id}/source-stop", "source-stop-proof", func(ctx context.Context, in *RecoveryInput) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return scopeAttempt(ctx, tx, actor, in.ID) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			var valid bool
			e := tx.QueryRow(ctx, `SELECT target_id IS NULL AND state='uncertain' AND closed_at IS NULL AND revision=$2 FROM cosmoformer.attempts WHERE id=$1`, in.ID, in.Body.Revision).Scan(&valid)
			if e != nil {
				return nil, e
			}
			if !valid {
				return nil, store.ErrConflict
			}
			if in.Body.RecoveryEvidence.Validate() != nil || in.Body.Stop.Method == "agent" {
				return nil, problem(ctx, 422, "Independent host or operator stop attestation is required.")
			}
			if e = coordinator.StopRecovery(ctx, tx, in.ID, in.Body.Stop); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='failed',terminal_reason='SOURCE_PREPARATION_UNKNOWN',revision=revision+1 WHERE id=(SELECT operation_id FROM cosmoformer.attempts WHERE id=$1)`, in.ID); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "attempt.source_stop", in.ID)
		})
	})
}

// Independent stop can also account for a diagnostic attempt which became
// uncertain while its originating case remains open. It releases only that
// attempt; the case's stop/reconciliation evidence and votes remain separate.
func (a *Server) registerAttemptStop(api huma.API) {
	register(api, "PATCH", "/organizations/{org}/attempts/{id}/stop-proof", "attempt-stop-proof", func(ctx context.Context, in *RecoveryInput) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return scopeAttempt(ctx, tx, actor, in.ID) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			var valid bool
			e := tx.QueryRow(ctx, `SELECT a.state='uncertain' AND a.closed_at IS NULL AND a.revision=$2 AND EXISTS(SELECT FROM cosmoformer.recovery_cases r WHERE r.target_id=a.target_id AND r.stack_id=a.stack_id AND r.status='open') FROM cosmoformer.attempts a WHERE a.id=$1`, in.ID, in.Body.Revision).Scan(&valid)
			if e != nil {
				return nil, e
			}
			if !valid {
				return nil, store.ErrConflict
			}
			if e = in.Body.RecoveryEvidence.Validate(); e != nil {
				return nil, problem(ctx, 422, "Supply bounded independent stop attestation and protected references.")
			}
			if e = coordinator.StopRecovery(ctx, tx, in.ID, in.Body.Stop); e != nil {
				return nil, workerError(ctx, e)
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='failed',terminal_reason=CASE WHEN recovery_case_id IS NOT NULL THEN 'RECOVERY_DIAGNOSTIC_STOPPED' ELSE 'RECOVERY_EXECUTION_STOPPED' END,revision=revision+1 WHERE id=(SELECT operation_id FROM cosmoformer.attempts WHERE id=$1) AND NOT EXISTS(SELECT FROM cosmoformer.recovery_cases WHERE origin_attempt_id=$1 AND status='open')`, in.ID); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "attempt.stop_proof", in.ID)
		})
	})
}
