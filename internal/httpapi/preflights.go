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

type PreflightPublic struct {
	NativePlanning bool   `json:"native_planning"`
	ID             string `json:"id"`
	RunID          string `json:"run_id"`
	Stack          string `json:"stack"`
	Commit         string `json:"commit"`
	Purpose        string `json:"purpose"`
	Status         string `json:"status"`
	Priority       string `json:"priority"`
	Reason         string `json:"reason"`
	Revision       int64  `json:"revision"`
	CanRetry       bool   `json:"can_retry"`
	CanPrioritize  bool   `json:"can_prioritize"`
}
type PreflightsOutput struct {
	Body struct {
		Preflights []PreflightPublic `json:"preflights"`
	}
}
type OperationRetryInput struct {
	OrgIDPath
	Body struct {
		Revision          int64 `json:"expected_revision,omitempty" minimum:"1"`
		OperationRevision int64 `json:"expected_operation_revision,omitempty" minimum:"1"`
	}
}
type PriorityInput struct {
	OrgIDPath
	Body struct {
		Revision          int64  `json:"expected_revision,omitempty" minimum:"1"`
		OperationRevision int64  `json:"expected_operation_revision,omitempty" minimum:"1" maximum:"9007199254740991"`
		Priority          string `json:"priority" enum:"low,medium,high"`
	}
}

func (a *Server) registerPreflights(api huma.API) {
	register(api, "GET", "/organizations/{org}/preflights", "preflights", func(ctx context.Context, in *OrgPath) (*PreflightsOutput, error) {
		out := &PreflightsOutput{}
		out.Body.Preflights = []PreflightPublic{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			g, e := store.GrantsFor(ctx, tx, actor.ID)
			if e != nil {
				return e
			}
			visible := []access.Grant{}
			for _, grant := range g {
				one := []access.Grant{grant}
				if access.Allows(one, grant.ProjectID, grant.EnvironmentID, access.View) || access.Allows(one, grant.ProjectID, grant.EnvironmentID, access.Manage) {
					visible = append(visible, grant)
				}
			}
			scopes, _ := json.Marshal(visible)
			rows, e := tx.Query(ctx, `SELECT o.id::text,o.run_id::text,s.display_name,r.source_commit,r.purpose,o.status,o.priority,coalesce(o.terminal_reason,''),o.revision,o.native_planning,s.project_id::text,s.environment_id::text,EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=o.id AND outcome='preparation_failed') AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=o.id AND (ever_permitted OR closed_at IS NULL OR outcome NOT IN ('success','preparation_failed'))) FROM cosmoformer.operations o JOIN cosmoformer.runs r ON r.id=o.run_id JOIN cosmoformer.stacks s ON s.id=o.stack_id WHERE o.kind='plan' AND EXISTS(SELECT FROM jsonb_array_elements($1::jsonb) AS scope(value) WHERE scope.value->>'project_id'=s.project_id::text AND (NOT o.native_planning OR scope.value->>'role'<>'administrator') AND (coalesce(scope.value->>'environment_id','')='' OR scope.value->>'environment_id'=s.environment_id::text)) ORDER BY o.created_at DESC,o.id LIMIT 100`, scopes)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var p PreflightPublic
				var project, env string
				var retry bool
				if e = rows.Scan(&p.ID, &p.RunID, &p.Stack, &p.Commit, &p.Purpose, &p.Status, &p.Priority, &p.Reason, &p.Revision, &p.NativePlanning, &project, &env, &retry); e != nil {
					return e
				}
				if !access.Allows(g, project, env, access.View) && !access.Allows(g, project, env, access.Manage) {
					continue
				}
				p.CanRetry = retry && p.Status == "failed" && access.Allows(g, project, env, access.Plan)
				p.CanPrioritize = p.Status == "queued" && (access.Allows(g, project, env, access.Plan) || access.Allows(g, project, env, access.Manage))
				out.Body.Preflights = append(out.Body.Preflights, p)
			}
			return rows.Err()
		})
		return out, e
	})
	authorize := func(ctx context.Context, tx pgx.Tx, actor store.Actor, id string, manage bool) error {
		var project, env, kind string
		if e := tx.QueryRow(ctx, `SELECT s.project_id::text,s.environment_id::text,o.kind FROM cosmoformer.operations o JOIN cosmoformer.stacks s ON s.id=o.stack_id WHERE o.id=$1`, id).Scan(&project, &env, &kind); e != nil {
			return store.ErrNotFound
		}
		g, e := store.GrantsFor(ctx, tx, actor.ID)
		if e != nil {
			return e
		}
		action := access.Plan
		if kind == "apply" {
			action = access.Apply
		}
		if access.Allows(g, project, env, action) || manage && access.Allows(g, project, env, access.Manage) {
			return nil
		}
		return store.ErrForbidden
	}
	priority := func(ctx context.Context, in *PriorityInput) (*OK, error) {
		revision := in.Body.Revision
		if in.Body.OperationRevision > 0 {
			revision = in.Body.OperationRevision
		}
		if revision < 1 || in.Body.Revision > 0 && in.Body.OperationRevision > 0 {
			return nil, problem(ctx, 422, "One expected operation revision is required.")
		}
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return authorize(ctx, tx, actor, in.ID, true) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.operations SET priority=$3,revision=revision+1 WHERE id=$1 AND revision=$2 AND status='queued' AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=$1 AND closed_at IS NULL)`, in.ID, revision, in.Body.Priority)
			if e != nil {
				return nil, e
			}
			if tag.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "preflight.priority", in.ID)
		})
	}
	register(api, "PATCH", "/organizations/{org}/operations/{id}/priority", "preflight-priority", priority)
	register(api, "POST", "/organizations/{org}/operations/{id}/priority", "operation-priority", priority)
	register(api, "POST", "/organizations/{org}/operations/{id}/retry", "retry-preflight", func(ctx context.Context, in *OperationRetryInput) (*OK, error) {
		revision := in.Body.Revision
		if in.Body.OperationRevision > 0 {
			revision = in.Body.OperationRevision
		}
		if revision < 1 || in.Body.Revision > 0 && in.Body.OperationRevision > 0 {
			return nil, problem(ctx, 422, "One expected operation revision is required.")
		}
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return authorize(ctx, tx, actor, in.ID, false) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			var kind, stack string
			var spec runs.ExecutionSpec
			if e := tx.QueryRow(ctx, `SELECT o.kind,o.stack_id::text,w.assignment_spec FROM cosmoformer.operations o JOIN cosmoformer.work_items w ON w.operation_id=o.id WHERE o.id=$1 ORDER BY CASE w.phase WHEN 'execution' THEN 0 ELSE 1 END LIMIT 1`, in.ID).Scan(&kind, &stack, newJSON(&spec)); e != nil {
				return nil, e
			}
			if kind == "apply" {
				if e := coordinator.ApplyEligibility(ctx, tx, stack, spec); e != nil {
					return nil, applyProblem(ctx, e)
				}
				if e := coordinator.HoldApply(ctx, tx, spec, "operation", in.ID); e != nil {
					return nil, applyProblem(ctx, e)
				}
			}
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='queued',terminal_reason=NULL,revision=revision+1 WHERE id=$1 AND revision=$2 AND status='failed' AND EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=$1 AND outcome='preparation_failed') AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=$1 AND (ever_permitted OR closed_at IS NULL OR outcome NOT IN ('success','preparation_failed')))`, in.ID, revision)
			if e != nil {
				return nil, e
			}
			if tag.RowsAffected() != 1 {
				return nil, applyProblem(ctx, runs.Fail("NEW_PLAN_REQUIRED"))
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='queued',resolved_preflight=NULL,revision=revision+1 WHERE id=(SELECT work_item_id FROM cosmoformer.attempts WHERE operation_id=$1 AND outcome='preparation_failed' ORDER BY created_at DESC,id DESC LIMIT 1)`, in.ID); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "preflight.retry", in.ID)
		})
	})
}
