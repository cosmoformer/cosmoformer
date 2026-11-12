package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type ApproveInput struct {
	OrgIDPath
	Body struct {
		Revision int64  `json:"expected_plan_revision" minimum:"1" maximum:"9007199254740991"`
		Manifest string `json:"manifest_digest" pattern:"^[0-9a-f]{64}$"`
	}
}
type WithdrawInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_plan_revision" minimum:"1" maximum:"9007199254740991"`
	}
}
type ApprovalSummary struct {
	CanApply         bool       `json:"can_apply,omitempty"`
	ApplyReasons     []string   `json:"apply_blocked_reasons,omitempty"`
	PlanID           string     `json:"plan_id" format:"uuid"`
	Revision         int64      `json:"revision" minimum:"1" maximum:"9007199254740991"`
	Manifest         string     `json:"manifest_digest"`
	Commit           string     `json:"source_commit" pattern:"^[0-9a-f]{40}$"`
	Minimum          int        `json:"minimum" minimum:"1" maximum:"100"`
	Count            int        `json:"counted_approvals" minimum:"0" maximum:"9007199254740991"`
	Eligible         bool       `json:"eligible"`
	OwnApproved      bool       `json:"own_approved"`
	OwnCounted       bool       `json:"own_counted"`
	CanApprove       bool       `json:"can_approve"`
	CanWithdraw      bool       `json:"can_withdraw"`
	Reasons          []string   `json:"blocked_reasons" nullable:"false"`
	SourceObservedAt *time.Time `json:"source_observed_at,omitempty"`
}
type ApprovalOutput struct{ Body ApprovalSummary }
type VoteOutput struct {
	Body struct {
		ApprovalSummary
		Event string `json:"vote_event_id" format:"uuid"`
	}
}
type approvalSubject struct {
	PlanStatus
	Project, Environment, Stack, Repo, Subject, Kind, Key, Commit, Provenance string
	ManifestID, PreID, LockID                                                 string
	Epoch                                                                     int64
	Floor                                                                     int
	Spec                                                                      runs.ExecutionSpec
	Artifacts                                                                 []string
}

func approvalSubjectFor(ctx context.Context, tx pgx.Tx, actor store.Actor, id string, positive bool) (approvalSubject, error) {
	var p approvalSubject
	e := tx.QueryRow(ctx, `SELECT `+planColumns+`,s.project_id::text,s.environment_id::text,s.id::text,s.repository_id::text,r.source_subject_id::text,ss.kind,ss.external_key,r.source_commit,p.provenance,coalesce(p.manifest_artifact_id::text,''),coalesce(t.pre_init_spec_artifact_id::text,''),coalesce(t.provider_lock_artifact_id::text,''),p.state_epoch,pj.minimum_approvals,t.execution_spec,coalesce(t.stop_proof->'artifact_ids','[]') FROM cosmoformer.plans p JOIN cosmoformer.operations o ON o.id=p.operation_id JOIN cosmoformer.runs r ON r.id=o.run_id JOIN cosmoformer.stacks s ON s.id=p.stack_id JOIN cosmoformer.projects pj ON pj.id=s.project_id JOIN cosmoformer.source_subjects ss ON ss.id=r.source_subject_id JOIN cosmoformer.attempts t ON t.id=p.attempt_id WHERE p.id=$1`, id).Scan(&p.ID, &p.RunID, &p.OperationID, &p.AttemptID, &p.Status, &p.Reason, &p.Revision, &p.Manifest, &p.Profile, &p.ReadyAt, &p.ExpiresAt, &p.Project, &p.Environment, &p.Stack, &p.Repo, &p.Subject, &p.Kind, &p.Key, &p.Commit, &p.Provenance, &p.ManifestID, &p.PreID, &p.LockID, &p.Epoch, &p.Floor, newJSON(&p.Spec), newJSON(&p.Artifacts))
	if errors.Is(e, pgx.ErrNoRows) {
		return p, store.ErrNotFound
	}
	if e != nil {
		return p, e
	}
	grants, e := store.GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return p, e
	}
	if !access.Allows(grants, p.Project, p.Environment, access.View) || positive && !access.Allows(grants, p.Project, p.Environment, access.Approve) {
		return p, store.ErrForbidden
	}
	return p, nil
}
func approvalProblem(ctx context.Context, code string) error {
	messages := map[string]string{
		"PLAN_NOT_READY":            "This plan is not ready for approval.",
		"PLAN_EXPIRED":              "This plan has expired. Request a fresh plan.",
		"PLAN_INVALIDATED":          "This plan was invalidated. Request a fresh plan and new approvals.",
		"PLAN_IDENTITY_CHANGED":     "The exact plan identity does not match. Refresh this run.",
		"RECOVERY_REQUIRED":         "Recovery-only evidence or an open recovery case cannot receive normal plan approval.",
		"INPUTS_CHANGED":            "Reviewed inputs or policy changed. Request a fresh plan and new approvals.",
		"STATE_OR_RECOVERY_CHANGED": "State or recovery context changed. Request a fresh normal plan after resolving recovery.",
		"PROFILE_UNSUPPORTED":       "This plan's mandatory runtime or analysis profile is unavailable.",
		"EVIDENCE_UNAVAILABLE":      "Exact retained plan evidence is unavailable.",
		"SOURCE_CHANGED":            "Source changed. Request a fresh plan and new approvals.",
		"SOURCE_VERIFICATION_STALE": "Source verification became stale or was superseded. Refresh and explicitly approve again only if this plan remains eligible.",
		"SOURCE_UNAVAILABLE":        "Source verification is unavailable. No approval was recorded.",
		"CLOCK_UNRELIABLE":          "Trusted server time is unavailable. Approval is blocked.",
		"MAINTENANCE":               "Approval is blocked during maintenance.",
	}
	message, ok := messages[code]
	if !ok {
		return fail(ctx, errors.New(code))
	}
	status := 409
	if code == "SOURCE_UNAVAILABLE" || code == "CLOCK_UNRELIABLE" {
		status = 503
	}
	p := problem(ctx, status, message)
	p.Code = code
	return p
}
func (p approvalSubject) subjectReason() string {
	if p.Provenance != "normal" || p.Status == "recovery_only" {
		return "RECOVERY_REQUIRED"
	}
	switch p.Status {
	case "expired":
		return "PLAN_EXPIRED"
	case "invalidated":
		return "PLAN_INVALIDATED"
	case "unsupported":
		return "PROFILE_UNSUPPORTED"
	case "ready":
		if p.ReadyAt != nil && p.ExpiresAt != nil && p.Manifest != "" {
			return ""
		}
	}
	return "PLAN_NOT_READY"
}
func (a *Server) approvalEligibility(ctx context.Context, tx pgx.Tx, p approvalSubject) (string, error) {
	if reason := p.subjectReason(); reason != "" {
		return reason, nil
	}
	if a.Evidence == nil || a.Evidence.Storage == nil {
		return "EVIDENCE_UNAVAILABLE", nil
	}
	var maintenance, unreliable bool
	if e := tx.QueryRow(ctx, `SELECT maintenance,clock_unreliable FROM cosmoformer.installation`).Scan(&maintenance, &unreliable); e != nil {
		return "", e
	}
	if maintenance {
		return "MAINTENANCE", nil
	}
	if unreliable {
		return "CLOCK_UNRELIABLE", nil
	}
	if e := coordinator.ApprovalInputs(ctx, tx, p.Stack, p.AttemptID, p.Spec); e != nil {
		if code := runs.Code(e); code != "" {
			return code, nil
		}
		return "", e
	}
	if _, e := approvalArtifacts(ctx, tx, p); e != nil {
		if errors.Is(e, evidence.ErrUnavailable) {
			return "EVIDENCE_UNAVAILABLE", nil
		}
		return "", e
	}
	return "", nil
}

// Exact artifact IDs come only from the immutable successful terminal result.
func approvalArtifacts(ctx context.Context, tx pgx.Tx, p approvalSubject) ([]evidence.Artifact, error) {
	if len(p.Artifacts) < 7 || len(p.Artifacts) > 8 {
		return nil, evidence.ErrUnavailable
	}
	rows, e := tx.Query(ctx, `SELECT a.id::text,a.upload_id::text,a.attempt_id::text,a.kind,a.bucket,a.object_key,a.version_id,encode(a.sha256,'hex'),a.byte_count,a.state FROM cosmoformer.artifacts a WHERE a.id=ANY($1::uuid[]) AND a.plan_id=$2 AND a.attempt_id=$3 AND a.state='finalized' AND a.version_id IS NOT NULL AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims c WHERE c.artifact_id=a.id) AND EXISTS(SELECT FROM cosmoformer.artifact_holds h WHERE h.artifact_id=a.id AND h.owner_kind='plan' AND h.owner_id=$2 AND h.released_at IS NULL) ORDER BY a.kind`, p.Artifacts, p.ID, p.AttemptID)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []evidence.Artifact{}
	kinds := map[string]string{}
	for rows.Next() {
		var r evidence.Artifact
		if e = rows.Scan(&r.ID, &r.Upload, &r.Attempt, &r.Kind, &r.Bucket, &r.Key, &r.Version, &r.Hash, &r.Size, &r.State); e != nil {
			return nil, e
		}
		if kinds[r.Kind] != "" {
			return nil, evidence.ErrUnavailable
		}
		if r.Kind == "manifest" && r.Hash != p.Manifest {
			return nil, evidence.ErrUnavailable
		}
		kinds[r.Kind] = r.ID
		out = append(out, r)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if len(out) != len(p.Artifacts) || kinds["manifest"] != p.ManifestID || kinds["pre_init_spec"] != p.PreID || kinds["provider_lock"] != p.LockID {
		return nil, evidence.ErrUnavailable
	}
	for _, kind := range []string{"manifest", "pre_init_spec", "binary_plan", "raw_plan_json", "provider_schema", "normalized_changes", "findings"} {
		if kinds[kind] == "" {
			return nil, evidence.ErrUnavailable
		}
	}
	return out, nil
}
func (a *Server) approvalSummary(ctx context.Context, tx pgx.Tx, actor store.Actor, p approvalSubject) (ApprovalSummary, error) {
	out := ApprovalSummary{PlanID: p.ID, Revision: p.Revision, Manifest: p.Manifest, Commit: p.Commit, Minimum: max(1, p.Floor, p.Spec.Review.Minimum), Reasons: []string{}}
	reason, e := a.approvalEligibility(ctx, tx, p)
	if e != nil {
		return out, e
	}
	out.Eligible = reason == ""
	if reason != "" {
		out.Reasons = append(out.Reasons, reason)
	}
	grants, e := store.GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return out, e
	}
	authority := access.Allows(grants, p.Project, p.Environment, access.Approve)
	if !authority {
		out.Reasons = append(out.Reasons, "APPROVAL_AUTHORITY_REQUIRED")
	}
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.plan_votes v WHERE v.plan_id=$1 AND v.account_id=$2 AND v.approved),count(*)::int,coalesce(bool_or(v.account_id=$2),false) FROM cosmoformer.plan_votes v JOIN cosmoformer.approvals e ON e.id=v.last_event_id WHERE v.plan_id=$1 AND v.approved AND v.subject_revision=$3 AND e.manifest_digest=decode($4,'hex') AND e.source_commit=$5 AND cosmoformer.requester_enabled(v.account_id) AND EXISTS(SELECT FROM cosmoformer.memberships m WHERE m.account_id=v.account_id AND m.org_id=v.org_id) AND EXISTS(SELECT FROM cosmoformer.team_members tm JOIN cosmoformer.grants g ON (g.org_id,g.team_id)=(tm.org_id,tm.team_id) WHERE tm.account_id=v.account_id AND g.org_id=v.org_id AND g.project_id=$6 AND (g.environment_id IS NULL OR g.environment_id=$7) AND g.role='approver' AND g.revoked_at IS NULL)`, p.ID, actor.ID, p.Revision, p.Manifest, p.Commit, p.Project, p.Environment).Scan(&out.OwnApproved, &out.Count, &out.OwnCounted)
	if e != nil {
		return out, e
	}
	if !out.Eligible {
		out.Count = 0
		out.OwnCounted = false
	}
	out.CanApprove = authority && out.Eligible
	out.CanWithdraw = out.OwnApproved
	out.ApplyReasons = []string{}
	if !access.Allows(grants, p.Project, p.Environment, access.Apply) {
		out.ApplyReasons = append(out.ApplyReasons, "APPLY_AUTHORITY_REQUIRED")
	} else if reason != "" {
		out.ApplyReasons = append(out.ApplyReasons, reason)
	} else {
		records, err := approvalArtifacts(ctx, tx, p)
		if err != nil {
			out.ApplyReasons = append(out.ApplyReasons, "EVIDENCE_UNAVAILABLE")
		} else {
			err = coordinator.ApplyEligibility(ctx, tx, p.Stack, retainedSpec(p, records))
			if err == nil {
				err = coordinator.ApplyHistory(ctx, tx, p.ID)
			}
			if err != nil {
				if code := runs.Code(err); code != "" {
					out.ApplyReasons = append(out.ApplyReasons, code)
				} else {
					return out, err
				}
			}
		}
	}
	out.CanApply = len(out.ApplyReasons) == 0
	e = tx.QueryRow(ctx, `SELECT max(observed_at) FROM cosmoformer.source_observations WHERE subject_id=$1`, p.Subject).Scan(&out.SourceObservedAt)
	return out, e
}
func (a *Server) registerApprovals(api huma.API) {
	register(api, "GET", "/organizations/{org}/plans/{id}/approvals", "plan-approvals", func(ctx context.Context, in *OrgIDPath) (*ApprovalOutput, error) {
		out := &ApprovalOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			p, e := approvalSubjectFor(ctx, tx, actor, in.ID, false)
			if e != nil {
				return e
			}
			out.Body, e = a.approvalSummary(ctx, tx, actor, p)
			return e
		})
		return out, e
	})
	register(api, "POST", "/organizations/{org}/plans/{id}/approvals", "approve-plan", func(ctx context.Context, in *ApproveInput) (*VoteOutput, error) {
		return a.vote(ctx, in.Org, in.ID, in.Body.Revision, in.Body.Manifest, true)
	})
	register(api, "DELETE", "/organizations/{org}/plans/{id}/approvals/me", "withdraw-plan-approval", func(ctx context.Context, in *WithdrawInput) (*VoteOutput, error) {
		return a.vote(ctx, in.Org, in.ID, in.Body.Revision, "", false)
	})
}
func (a *Server) vote(ctx context.Context, org, id string, revision int64, manifest string, positive bool) (*VoteOutput, error) {
	var subject approvalSubject
	var observation coordinator.Observation
	var repo RepoFacts
	var replay *VoteOutput
	var found bool
	var initialDenied error
	// Authorization precedes replay, including when an account loses approve but
	// retains visibility. Withdrawal intentionally requires visibility alone.
	e := a.tx(ctx, org, func(tx pgx.Tx, actor store.Actor) error {
		var e error
		subject, e = approvalSubjectFor(ctx, tx, actor, id, positive)
		if e != nil {
			return e
		}
		replay, found, e = browserReceipt[VoteOutput](ctx, tx, actor)
		if e != nil {
			return e
		}
		if found {
			replay.Body.ApprovalSummary, e = a.approvalSummary(ctx, tx, actor, subject)
			return e
		}
		if subject.Revision != revision || positive && subject.Manifest != manifest {
			return approvalProblem(ctx, "PLAN_IDENTITY_CHANGED")
		}
		if !positive {
			return nil
		}
		reason, e := a.approvalEligibility(ctx, tx, subject)
		if e != nil {
			return e
		}
		if reason != "" {
			return approvalProblem(ctx, reason)
		}
		tick, e := a.Coordinator.Clock.Now()
		if e != nil {
			return approvalProblem(ctx, "CLOCK_UNRELIABLE")
		}
		var good bool
		if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_clock_observe($1,$2)`, a.Coordinator.Instance, int64(tick)).Scan(&good); e != nil {
			return e
		}
		if !good {
			initialDenied = approvalProblem(ctx, "CLOCK_UNRELIABLE")
			return nil
		}
		repo, e = repoFacts(ctx, tx, subject.Repo)
		if e != nil {
			return e
		}
		observation, e = a.Coordinator.BeginObservation(ctx, tx, org, subject.Repo, subject.Kind, subject.Key)
		return e
	})
	if e != nil {
		return nil, e
	}
	if found {
		return replay, nil
	}
	if initialDenied != nil {
		return nil, initialDenied
	}
	var records []evidence.Artifact
	var reviewed runs.Manifest
	if positive {
		// Independent per-action live read, bounded even for synthetic HTTP clients.
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		token, err := a.apiToken(readCtx, org, repo.IntegrationID)
		if err != nil {
			cancel()
			return nil, approvalProblem(ctx, "SOURCE_UNAVAILABLE")
		}
		pr := 0
		if subject.Kind == "pr" {
			pr, err = strconv.Atoi(subject.Key)
			if err != nil {
				cancel()
				return nil, approvalProblem(ctx, "SOURCE_CHANGED")
			}
		}
		facts, err := a.Bitbucket.Read(readCtx, repo.URL, repo.ExternalID, repo.Branch, pr, token)
		cancel()
		contextChanged := errors.Is(err, bitbucket.ErrContextChanged)
		if err != nil && !contextChanged {
			return nil, approvalProblem(ctx, "SOURCE_UNAVAILABLE")
		}
		facts.InventoryRevision = repo.Revision
		sourceRevision, err := a.acceptSourceFacts(ctx, org, subject.Repo, repo, observation, facts)
		if err != nil {
			return nil, approvalProblem(ctx, "SOURCE_VERIFICATION_STALE")
		}
		if contextChanged || sourceRevision != subject.Spec.Source.FactsRevision || facts.Commit != subject.Commit {
			return nil, approvalProblem(ctx, "SOURCE_CHANGED")
		}
		// Verify every accepted exact version outside transactions, not a latest
		// artifact of the same kind. Raw bytes never leave this internal path.
		authorize := func(fn func(pgx.Tx) error) error {
			return a.Store.ActorTx(ctx, request(ctx).Hash, org, func(tx pgx.Tx, actor store.Actor) error {
				p, e := approvalSubjectFor(ctx, tx, actor, id, true)
				if e != nil {
					return e
				}
				if p.Revision != revision || p.Manifest != manifest {
					return approvalProblem(ctx, "PLAN_IDENTITY_CHANGED")
				}
				return fn(tx)
			})
		}
		e = authorize(func(tx pgx.Tx) error { var e error; records, e = approvalArtifacts(ctx, tx, subject); return e })
		if e != nil {
			return nil, approvalFailure(ctx, e)
		}
		for _, r := range records {
			_, f, err := a.Evidence.Read(ctx, authorize, r.ID)
			if err != nil {
				return nil, approvalFailure(ctx, err)
			}
			if r.Kind == "manifest" {
				raw, readErr := io.ReadAll(io.LimitReader(f, evidence.PlanMax+1))
				if readErr != nil || int64(len(raw)) > evidence.PlanMax || runs.Decode(raw, &reviewed) != nil || runs.Digest(reviewed) != manifest {
					err = evidence.ErrUnavailable
				}
			}
			f.Close()
			os.Remove(f.Name())
			if err != nil {
				return nil, approvalFailure(ctx, err)
			}
		}
		if reviewed.ReviewedInputs.Validate(subject.Spec, subject.Stack, subject.Epoch) != nil || reviewed.ValidateLock() != nil {
			return nil, approvalProblem(ctx, "EVIDENCE_UNAVAILABLE")
		}
	}
	out := &VoteOutput{}
	var denied error
	// Commit the clock-unreliable latch even when it denies the vote. The vote,
	// projection, audit and replay receipt otherwise commit in this same gate.
	e = a.tx(ctx, org, func(tx pgx.Tx, actor store.Actor) error {
		p, e := approvalSubjectFor(ctx, tx, actor, id, positive)
		if e != nil {
			return e
		}
		old, exists, e := browserReceipt[VoteOutput](ctx, tx, actor)
		if e != nil {
			return e
		}
		if exists {
			out = old
			out.Body.ApprovalSummary, e = a.approvalSummary(ctx, tx, actor, p)
			return e
		}
		if p.Revision != revision || positive && p.Manifest != manifest {
			return approvalProblem(ctx, "PLAN_IDENTITY_CHANGED")
		}
		if positive {
			tick, e := a.Coordinator.Clock.Now()
			if e != nil {
				return approvalProblem(ctx, "CLOCK_UNRELIABLE")
			}
			var good bool
			if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_clock_observe($1,$2)`, a.Coordinator.Instance, int64(tick)).Scan(&good); e != nil {
				return e
			}
			if !good {
				denied = approvalProblem(ctx, "CLOCK_UNRELIABLE")
				return nil
			}
			if e = a.Coordinator.RequireObservation(ctx, tx, observation, p.Spec.Source.FactsRevision); e != nil {
				return approvalProblem(ctx, "SOURCE_VERIFICATION_STALE")
			}
			reason, e := a.approvalEligibility(ctx, tx, p)
			if e != nil {
				return e
			}
			if reason != "" {
				return approvalProblem(ctx, reason)
			}
			// Re-read current DB time after lock acquisition and all reference checks.
			var unexpired bool
			if e = tx.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM cosmoformer.plans WHERE id=$1`, id).Scan(&unexpired); e != nil {
				return e
			}
			if !unexpired {
				return approvalProblem(ctx, "PLAN_EXPIRED")
			}
			current, e := approvalArtifacts(ctx, tx, p)
			if e != nil {
				return approvalProblem(ctx, "EVIDENCE_UNAVAILABLE")
			}
			if runs.Digest(current) != runs.Digest(records) {
				return approvalProblem(ctx, "EVIDENCE_UNAVAILABLE")
			}
		}
		// Time-based session expiry can cross while current references are checked,
		// even though account/grant mutations are excluded by the gate.
		var authenticated bool
		if e = tx.QueryRow(ctx, `SELECT cosmoformer.session_identity($1) IS NOT NULL`, request(ctx).Hash).Scan(&authenticated); e != nil {
			return e
		}
		if !authenticated {
			return store.ErrUnauthenticated
		}
		event := runs.ID()
		action := "revoke"
		var observationID any
		var digest, commit any
		if positive {
			action = "approve"
			digest = manifest
			commit = p.Commit
			var v string
			if e = tx.QueryRow(ctx, `SELECT id::text FROM cosmoformer.source_observations WHERE subject_id=$1 AND ticket=$2`, observation.SubjectID, observation.Ticket).Scan(&v); e != nil {
				return e
			}
			observationID = v
			// Reference and session checks can consume the remaining source age.
			// Fence again at the last decision point before appending the event.
			if e = a.Coordinator.RequireObservation(ctx, tx, observation, p.Spec.Source.FactsRevision); e != nil {
				return approvalProblem(ctx, "SOURCE_VERIFICATION_STALE")
			}
		}
		if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.approvals(id,org_id,subject_kind,subject_id,subject_revision,account_id,action,source_observation_id,accepted_at,manifest_digest,source_commit) VALUES($1,$2,'plan',$3,$4,$5,$6,$7,clock_timestamp(),decode($8::text,'hex'),$9)`, event, org, id, revision, actor.ID, action, observationID, digest, commit); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.plan_votes(org_id,plan_id,account_id,last_event_id,approved,subject_revision) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(org_id,plan_id,account_id) DO UPDATE SET last_event_id=excluded.last_event_id,approved=excluded.approved,subject_revision=excluded.subject_revision`, org, id, actor.ID, event, positive, revision); e != nil {
			return e
		}
		if e = store.Audit(ctx, tx, org, actor.ID, "plan."+action, id); e != nil {
			return e
		}
		out.Body.ApprovalSummary, e = a.approvalSummary(ctx, tx, actor, p)
		if e != nil {
			return e
		}
		out.Body.Event = event
		raw, e := json.Marshal(out)
		if e != nil {
			return e
		}
		d := request(ctx)
		_, e = tx.Exec(ctx, `SELECT cosmoformer.save_browser_request($1,$2,$3,$4::jsonb,false)`, actor.ID, d.Key, d.Digest, string(raw))
		return e
	})
	if e != nil {
		return nil, e
	}
	if denied != nil {
		return nil, denied
	}
	return out, nil
}
func approvalFailure(ctx context.Context, e error) error {
	if errors.Is(e, store.ErrUnauthenticated) || errors.Is(e, store.ErrForbidden) || errors.Is(e, store.ErrNotFound) {
		return fail(ctx, e)
	}
	var p *Problem
	if errors.As(e, &p) {
		return p
	}
	return approvalProblem(ctx, "EVIDENCE_UNAVAILABLE")
}
