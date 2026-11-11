package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

// The database filters current action scopes before applying any page limit.
// Administrative visibility is deliberately excluded from execution reads.
func viewScopes(g []access.Grant, manage bool) []byte {
	scopes := []access.Grant{}
	for _, grant := range g {
		if access.Allows([]access.Grant{grant}, grant.ProjectID, grant.EnvironmentID, access.View) || manage && access.Allows([]access.Grant{grant}, grant.ProjectID, grant.EnvironmentID, access.Manage) {
			scopes = append(scopes, grant)
		}
	}
	b, _ := json.Marshal(scopes)
	return b
}

const visibleStack = `EXISTS(SELECT FROM jsonb_array_elements($1::jsonb) scope(value) WHERE scope.value->>'project_id'=s.project_id::text AND (coalesce(scope.value->>'environment_id','')='' OR scope.value->>'environment_id'=s.environment_id::text))`

// Reuse the exact list scope when resolving cursors. Hidden, revoked and filtered
// records must behave like nonexistent IDs, never as pagination boundaries.
const workspaceScope = visibleStack + ` AND ($2='' OR s.project_id::text=$2) AND ($3='' OR s.environment_id::text=$3) AND ($4='' OR s.id::text=$4)`

type WorkspaceListInput struct {
	OrgPath
	Project     string `query:"project_id" format:"uuid"`
	Environment string `query:"environment_id" format:"uuid"`
	Stack       string `query:"stack_id" format:"uuid"`
	Before      string `query:"before" format:"uuid"`
	Limit       int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}
type RunPublic struct {
	ID              string      `json:"id" format:"uuid"`
	StackID         string      `json:"stack_id" format:"uuid"`
	StackName       string      `json:"stack_name"`
	ProjectID       string      `json:"project_id" format:"uuid"`
	ProjectName     string      `json:"project_name"`
	EnvironmentID   string      `json:"environment_id" format:"uuid"`
	EnvironmentName string      `json:"environment_name"`
	Commit          string      `json:"commit"`
	SourceKind      string      `json:"source_kind" enum:"branch,pr"`
	SourceKey       string      `json:"source_key"`
	SourceURL       string      `json:"source_url"`
	SourceRoot      string      `json:"source_root"`
	Purpose         string      `json:"purpose"`
	OperationID     string      `json:"operation_id" format:"uuid"`
	NativePlanning  bool        `json:"native_planning"`
	Status          string      `json:"status"`
	Reason          string      `json:"reason"`
	QueueReason     string      `json:"queue_reason"`
	Priority        string      `json:"priority" enum:"low,medium,high"`
	CreatedAt       time.Time   `json:"created_at"`
	AcceptedAt      time.Time   `json:"accepted_at"`
	Plan            *PlanStatus `json:"plan,omitempty"`
}
type RunsOutput struct {
	Body struct {
		Runs []RunPublic `json:"runs" nullable:"false"`
		Next string      `json:"next_cursor,omitempty"`
	}
}
type AttemptPublic struct {
	Revision       int64           `json:"revision,omitempty"`
	Ordinal        int64           `json:"command_ordinal,omitempty"`
	CanControl     bool            `json:"can_control,omitempty"`
	ContactWarning bool            `json:"contact_warning,omitempty"`
	Controls       []ControlPublic `json:"controls,omitempty"`

	ID        string     `json:"id" format:"uuid"`
	Phase     string     `json:"phase" enum:"preflight,execution"`
	State     string     `json:"state"`
	Outcome   string     `json:"outcome"`
	CreatedAt time.Time  `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at,omitempty"`
}
type ReviewArtifact struct {
	ID     string `json:"id" format:"uuid"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}
type RunReadInput struct {
	OrgIDPath
	AttemptBefore  string `query:"attempt_before" format:"uuid"`
	ArtifactBefore string `query:"artifact_before" format:"uuid"`
}
type RunOutput struct {
	Body struct {
		Recovery      *RecoveryPublic  `json:"recovery,omitempty"`
		Run           RunPublic        `json:"run"`
		AttemptsNext  string           `json:"attempts_next,omitempty"`
		ArtifactsNext string           `json:"raw_artifacts_next,omitempty"`
		Attempts      []AttemptPublic  `json:"attempts" nullable:"false"`
		CanReadRaw    bool             `json:"can_read_raw"`
		Artifacts     []ReviewArtifact `json:"raw_artifacts" nullable:"false"`
	}
}

const runColumns = `r.id::text,s.id::text,s.display_name,s.project_id::text,pj.name,s.environment_id::text,en.name,r.source_commit,ss.kind,ss.external_key,coalesce(w.assignment_spec->'source'->>'url',''),coalesce(w.assignment_spec->'source'->>'root',''),r.purpose,o.id::text,o.native_planning,o.status,coalesce(o.terminal_reason,''),o.priority,r.created_at,o.accepted_at,
 CASE WHEN o.status<>'queued' THEN ''
 WHEN EXISTS(SELECT FROM cosmoformer.recovery_cases rc WHERE rc.stack_id=s.id AND rc.status='open') THEN 'RECOVERY_REQUIRED'
 WHEN EXISTS(SELECT FROM cosmoformer.attempts a WHERE a.stack_id=s.id AND a.closed_at IS NULL) THEN 'STACK_BUSY'
 WHEN NOT EXISTS(SELECT FROM cosmoformer.workers cw WHERE cw.pool_id::text=w.assignment_spec->'pool'->>'id' AND cw.runtime_id::text=w.assignment_spec->'engine'->>'runtime_id' AND NOT cw.paused AND EXISTS(SELECT FROM cosmoformer.worker_pools wp WHERE wp.id=cw.pool_id AND wp.enabled) AND cw.last_contact_at>clock_timestamp()-interval '30 seconds' AND cw.capabilities->'supported_work_phases' ? w.phase AND cw.tags @> ARRAY(SELECT jsonb_array_elements_text(coalesce(w.assignment_spec->'pool'->'tags','[]'::jsonb)))) THEN 'NO_COMPATIBLE_WORKER'
 ELSE 'WAITING_CAPACITY' END`
const runTables = ` FROM cosmoformer.runs r JOIN cosmoformer.stacks s ON s.id=r.stack_id JOIN cosmoformer.projects pj ON pj.id=s.project_id JOIN cosmoformer.environments en ON en.id=s.environment_id JOIN cosmoformer.source_subjects ss ON ss.id=r.source_subject_id JOIN cosmoformer.operations o ON o.run_id=r.id AND o.kind='plan' LEFT JOIN LATERAL (SELECT assignment_spec,phase FROM cosmoformer.work_items WHERE operation_id=o.id ORDER BY CASE phase WHEN 'execution' THEN 0 ELSE 1 END LIMIT 1) w ON true `

func scanRun(row pgx.Row, r *RunPublic) error {
	return row.Scan(&r.ID, &r.StackID, &r.StackName, &r.ProjectID, &r.ProjectName, &r.EnvironmentID, &r.EnvironmentName, &r.Commit, &r.SourceKind, &r.SourceKey, &r.SourceURL, &r.SourceRoot, &r.Purpose, &r.OperationID, &r.NativePlanning, &r.Status, &r.Reason, &r.Priority, &r.CreatedAt, &r.AcceptedAt, &r.QueueReason)
}

// The accepted, immutable result names the exact evidence used for readiness.
// This also survives plan-hold release on expiry/invalidation. No latest lookup.
const retainedChangesID = `coalesce((SELECT CASE WHEN count(*)=1 THEN min(a.id::text) ELSE '' END FROM cosmoformer.artifacts a JOIN cosmoformer.attempts t ON t.id=a.attempt_id WHERE a.plan_id=p.id AND a.attempt_id=p.attempt_id AND a.kind='normalized_changes' AND a.id::text IN (SELECT jsonb_array_elements_text(coalesce(t.stop_proof->'artifact_ids','[]'::jsonb)))),'')`
const planColumns = `p.id::text,o.run_id::text,p.operation_id::text,p.attempt_id::text,CASE WHEN p.status='ready' AND p.expires_at<=clock_timestamp() THEN 'expired' ELSE p.status END,coalesce(p.invalidation_reason,o.terminal_reason,''),p.revision,coalesce(encode(p.manifest_digest,'hex'),''),p.profile_id,p.ready_at,p.expires_at`

func readRunPlan(ctx context.Context, tx pgx.Tx, r *RunPublic) error {
	p := &PlanStatus{}
	err := tx.QueryRow(ctx, `SELECT `+planColumns+` FROM cosmoformer.plans p JOIN cosmoformer.operations o ON o.id=p.operation_id WHERE p.operation_id=$1`, r.OperationID).Scan(&p.ID, &p.RunID, &p.OperationID, &p.AttemptID, &p.Status, &p.Reason, &p.Revision, &p.Manifest, &p.Profile, &p.ReadyAt, &p.ExpiresAt)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if err = planEvidenceStatus(ctx, tx, p); err != nil {
		return err
	}
	r.Plan = p
	return nil
}
func planEvidenceStatus(ctx context.Context, tx pgx.Tx, p *PlanStatus) error {
	p.Evidence = "not_finalized"
	if p.ReadyAt == nil {
		return nil
	}
	p.Evidence = "unavailable"
	var id string
	if err := tx.QueryRow(ctx, `SELECT `+retainedChangesID+` FROM cosmoformer.plans p WHERE p.id=$1`, p.ID).Scan(&id); err != nil {
		return err
	}
	var available bool
	if id != "" {
		if err := tx.QueryRow(ctx, `SELECT state='finalized' AND version_id IS NOT NULL AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=a.id) FROM cosmoformer.artifacts a WHERE a.id=$1`, id).Scan(&available); err != nil {
			return err
		}
	}
	if available {
		p.Evidence = "retained"
	}
	return nil
}
func (a *Server) registerWorkspace(api huma.API) {
	a.registerStackReads(api)
	register(api, "GET", "/organizations/{org}/runs", "runs", func(ctx context.Context, in *WorkspaceListInput) (*RunsOutput, error) {
		out := &RunsOutput{}
		out.Body.Runs = []RunPublic{}
		err := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			grants, err := store.GrantsFor(ctx, tx, actor.ID)
			if err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT `+runColumns+runTables+` WHERE `+workspaceScope+` AND ($5='' OR (r.created_at,r.id)<(SELECT r.created_at,r.id`+runTables+` WHERE `+workspaceScope+` AND r.id::text=$5)) ORDER BY r.created_at DESC,r.id DESC LIMIT $6`, viewScopes(grants, false), in.Project, in.Environment, in.Stack, in.Before, in.Limit+1)
			if err != nil {
				return err
			}
			for rows.Next() {
				var r RunPublic
				if err = scanRun(rows, &r); err != nil {
					rows.Close()
					return err
				}
				out.Body.Runs = append(out.Body.Runs, r)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(out.Body.Runs) > in.Limit {
				out.Body.Runs = out.Body.Runs[:in.Limit]
				out.Body.Next = out.Body.Runs[in.Limit-1].ID
			}
			for i := range out.Body.Runs {
				if err = readRunPlan(ctx, tx, &out.Body.Runs[i]); err != nil {
					return err
				}
			}
			return nil
		})
		return out, err
	})
	register(api, "GET", "/organizations/{org}/runs/{id}", "run", func(ctx context.Context, in *RunReadInput) (*RunOutput, error) {
		out := &RunOutput{}
		out.Body.Attempts = []AttemptPublic{}
		out.Body.Artifacts = []ReviewArtifact{}
		err := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			grants, err := store.GrantsFor(ctx, tx, actor.ID)
			if err != nil {
				return err
			}
			if err = scanRun(tx.QueryRow(ctx, `SELECT `+runColumns+runTables+` WHERE r.id=$2 AND `+visibleStack, viewScopes(grants, false), in.ID), &out.Body.Run); err != nil {
				if err == pgx.ErrNoRows {
					return store.ErrNotFound
				}
				return err
			}
			if err = readRunPlan(ctx, tx, &out.Body.Run); err != nil {
				return err
			}
			out.Body.CanReadRaw = access.Allows(grants, out.Body.Run.ProjectID, out.Body.Run.EnvironmentID, access.RawEvidenceRead)
			rows, err := tx.Query(ctx, `SELECT a.id::text,w.phase,a.state,coalesce(a.outcome,''),a.created_at,a.closed_at FROM cosmoformer.attempts a JOIN cosmoformer.work_items w ON w.id=a.work_item_id WHERE a.operation_id=$1 AND ($2='' OR (a.created_at,a.id)<(SELECT created_at,id FROM cosmoformer.attempts WHERE id::text=$2 AND operation_id=$1)) ORDER BY a.created_at DESC,a.id DESC LIMIT 101`, out.Body.Run.OperationID, in.AttemptBefore)
			if err != nil {
				return err
			}
			for rows.Next() {
				var p AttemptPublic
				if err = rows.Scan(&p.ID, &p.Phase, &p.State, &p.Outcome, &p.CreatedAt, &p.ClosedAt); err != nil {
					rows.Close()
					return err
				}
				out.Body.Attempts = append(out.Body.Attempts, p)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(out.Body.Attempts) > 100 {
				out.Body.Attempts = out.Body.Attempts[:100]
				out.Body.AttemptsNext = out.Body.Attempts[99].ID
			}
			slices.Reverse(out.Body.Attempts)
			for i := range out.Body.Attempts {
				v := &out.Body.Attempts[i]
				if err = tx.QueryRow(ctx, `SELECT a.revision,a.command_ordinal,a.closed_at IS NULL,coalesce(w.last_contact_at<clock_timestamp()-interval '30 seconds',true) AND a.closed_at IS NULL FROM cosmoformer.attempts a JOIN cosmoformer.workers w ON w.id=a.worker_id WHERE a.id=$1`, v.ID).Scan(&v.Revision, &v.Ordinal, &v.CanControl, &v.ContactWarning); err != nil {
					return err
				}
				v.CanControl = v.CanControl && access.Allows(grants, out.Body.Run.ProjectID, out.Body.Run.EnvironmentID, access.Apply)
				v.Controls, err = readControls(ctx, tx, v.ID)
				if err != nil {
					return err
				}
			}
			var recoveryID string
			err = tx.QueryRow(ctx, `SELECT r.id::text FROM cosmoformer.recovery_cases r WHERE r.stack_id=$1 AND (r.status='open' OR r.origin_attempt_id IN(SELECT id FROM cosmoformer.attempts WHERE operation_id=$2) OR r.id=(SELECT recovery_case_id FROM cosmoformer.operations WHERE id=$2)) ORDER BY r.opened_at DESC LIMIT 1`, out.Body.Run.StackID, out.Body.Run.OperationID).Scan(&recoveryID)
			if err != nil && err != pgx.ErrNoRows {
				return err
			}
			if recoveryID != "" {
				rc, e := recoveryFor(ctx, tx, actor, recoveryID, access.View)
				if e != nil {
					return e
				}
				out.Body.Recovery = &rc.RecoveryPublic
			}

			if !out.Body.CanReadRaw {
				return nil
			}
			rows, err = tx.Query(ctx, `SELECT a.id::text,a.kind,a.state FROM cosmoformer.artifacts a JOIN cosmoformer.attempts t ON t.id=a.attempt_id WHERE t.operation_id=$1 AND a.kind IN ('binary_plan','raw_plan_json','manifest','pre_init_spec','provider_schema','provider_lock') AND ($2='' OR (a.created_at,a.id)<(SELECT created_at,id FROM cosmoformer.artifacts WHERE id::text=$2 AND attempt_id IN(SELECT id FROM cosmoformer.attempts WHERE operation_id=$1))) ORDER BY a.created_at DESC,a.id DESC LIMIT 101`, out.Body.Run.OperationID, in.ArtifactBefore)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p ReviewArtifact
				if err = rows.Scan(&p.ID, &p.Kind, &p.Status); err != nil {
					return err
				}
				if evidence.Raw(p.Kind) {
					out.Body.Artifacts = append(out.Body.Artifacts, p)
				}
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if len(out.Body.Artifacts) > 100 {
				out.Body.Artifacts = out.Body.Artifacts[:100]
				out.Body.ArtifactsNext = out.Body.Artifacts[99].ID
			}
			return nil
		})
		return out, err
	})
}

func (a *Server) registerStackReads(api huma.API) {
	register(api, "GET", "/organizations/{org}/stacks", "stacks", func(ctx context.Context, in *WorkspaceListInput) (*StacksOutput, error) {
		out := &StacksOutput{}
		out.Body.Stacks = []StackPublic{}
		err := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			grants, err := store.GrantsFor(ctx, tx, actor.ID)
			if err != nil {
				return err
			}
			rows, err := tx.Query(ctx, `SELECT s.id::text,s.display_name,s.project_id::text,s.environment_id::text,coalesce(sr.commit,''),s.archived,pj.name,en.name,s.repository_id::text,r.url,r.tracked_branch,coalesce(sr.parsed_config->>'root',''),s.assessment FROM cosmoformer.stacks s JOIN cosmoformer.projects pj ON pj.id=s.project_id JOIN cosmoformer.environments en ON en.id=s.environment_id JOIN cosmoformer.repositories r ON r.id=s.repository_id LEFT JOIN cosmoformer.stack_revisions sr ON sr.id=s.effective_revision_id WHERE `+workspaceScope+` AND ($5='' OR (s.display_name,s.id)>(SELECT s.display_name,s.id FROM cosmoformer.stacks s WHERE `+workspaceScope+` AND s.id::text=$5)) ORDER BY s.display_name,s.id LIMIT $6`, viewScopes(grants, true), in.Project, in.Environment, in.Stack, in.Before, in.Limit+1)
			if err != nil {
				return err
			}
			for rows.Next() {
				var s StackPublic
				if err = rows.Scan(&s.ID, &s.Name, &s.ProjectID, &s.EnvironmentID, &s.Commit, &s.Archived, &s.ProjectName, &s.EnvironmentName, &s.Repository, &s.SourceURL, &s.Branch, &s.Root, &s.Assessment); err != nil {
					rows.Close()
					return err
				}
				out.Body.Stacks = append(out.Body.Stacks, s)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(out.Body.Stacks) > in.Limit {
				out.Body.Stacks = out.Body.Stacks[:in.Limit]
				out.Body.Next = out.Body.Stacks[in.Limit-1].ID
			}
			for i := range out.Body.Stacks {
				s := &out.Body.Stacks[i]
				s.CanReview = access.Allows(grants, s.ProjectID, s.EnvironmentID, access.View)
				s.CanPlan = access.Allows(grants, s.ProjectID, s.EnvironmentID, access.Plan) && !s.Archived
				s.CheckReason = "Planning permission is required."
				s.PlanReason = s.CheckReason
				if s.Archived {
					s.CheckReason = "This stack is archived."
					s.PlanReason = s.CheckReason
					continue
				}
				if !s.CanPlan {
					continue
				}
				var declaration catalog.Stack
				if err = tx.QueryRow(ctx, `SELECT parsed_config FROM cosmoformer.stack_revisions WHERE id=(SELECT effective_revision_id FROM cosmoformer.stacks WHERE id=$1)`, s.ID).Scan(newJSON(&declaration)); err != nil {
					return err
				}
				eff, buildErr := coordinator.BuildSpec(ctx, tx, declaration, s.Repository)
				if buildErr != nil {
					s.CanPlan = false
					s.CheckReason = "Current source, runtime or resource permissions need attention."
					s.PlanReason = s.CheckReason
					continue
				}
				s.CheckReason = ""
				s.PlanReason = "Execution compatibility has not been assessed for this stack."
				if s.Assessment != "compatible" {
					continue
				}
				s.PlanReason = "This runtime has no supported planning profile."
				if !plan.SupportedRuntime(eff.Runtime.Name, eff.Runtime.Version) {
					continue
				}
				s.PlanReason = "Protected planning evidence storage is unavailable."
				if a.Evidence == nil {
					continue
				}
				var fixed bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.aws_connections WHERE id=$1 AND revision=$2 AND expected_bootstrap_principal IS NOT NULL AND session_seconds=3600 AND enabled)`, eff.AWS.ID, eff.AWS.Revision).Scan(&fixed); err != nil {
					return err
				}
				s.PlanReason = "The fixed execution session metadata is incomplete."
				if !fixed {
					continue
				}
				s.CanNativePlan = true
				s.PlanReason = ""
			}
			return nil
		})
		return out, err
	})
}
