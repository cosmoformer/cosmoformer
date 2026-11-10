package httpapi

import (
	"bytes"
	"context"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"io"
	"os"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type PlanStatus struct {
	ReadyAt     *time.Time `json:"ready_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	OperationID string     `json:"operation_id"`
	AttemptID   string     `json:"attempt_id"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	Revision    int64      `json:"revision"`
	Manifest    string     `json:"manifest_digest"`
	Profile     string     `json:"profile_id"`
}
type PlansOutput struct {
	Body struct {
		Plans []PlanStatus `json:"plans"`
	}
}
type ChangesInput struct {
	OrgIDPath
	Plan     string `query:"plan_id" format:"uuid" required:"true"`
	Revision int64  `query:"revision" minimum:"1" required:"true"`
}
type SanitizedChanges plan.Result

type ChangesOutput struct {
	Body struct {
		PlanID   string            `json:"plan_id"`
		Revision int64             `json:"revision"`
		Manifest string            `json:"manifest_digest"`
		Changes  *SanitizedChanges `json:"changes"`
	}
}

func (a *Server) registerPlanningReads(api huma.API) {
	register(api, "GET", "/organizations/{org}/plans", "plans", func(ctx context.Context, in *OrgPath) (*PlansOutput, error) {
		out := &PlansOutput{}
		out.Body.Plans = []PlanStatus{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			grants, e := store.GrantsFor(ctx, tx, actor.ID)
			if e != nil {
				return e
			}
			rows, e := tx.Query(ctx, `SELECT p.id::text,o.run_id::text,p.operation_id::text,p.attempt_id::text,CASE WHEN p.status='ready' AND p.expires_at<=clock_timestamp() THEN 'expired' ELSE p.status END,coalesce(p.invalidation_reason,o.terminal_reason,''),p.revision,coalesce(encode(p.manifest_digest,'hex'),''),p.profile_id,p.ready_at,p.expires_at,s.project_id::text,s.environment_id::text FROM cosmoformer.plans p JOIN cosmoformer.operations o ON o.id=p.operation_id JOIN cosmoformer.stacks s ON s.id=p.stack_id ORDER BY p.created_at DESC,p.id LIMIT 100`)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var p PlanStatus
				var project, env string
				if e = rows.Scan(&p.ID, &p.RunID, &p.OperationID, &p.AttemptID, &p.Status, &p.Reason, &p.Revision, &p.Manifest, &p.Profile, &p.ReadyAt, &p.ExpiresAt, &project, &env); e != nil {
					return e
				}
				if access.Allows(grants, project, env, access.View) {
					out.Body.Plans = append(out.Body.Plans, p)
				}
			}
			return rows.Err()
		})
		return out, e
	})
	register(api, "GET", "/organizations/{org}/runs/{id}/changes", "run-changes", func(ctx context.Context, in *ChangesInput) (*ChangesOutput, error) {
		var artifact string
		out := &ChangesOutput{}
		authorize := func(fn func(pgx.Tx) error) error {
			return a.Store.ActorTx(ctx, request(ctx).Hash, in.Org, func(tx pgx.Tx, actor store.Actor) error {
				var project, env string
				var available bool
				if e := tx.QueryRow(ctx, `SELECT s.project_id::text,s.environment_id::text,p.status='ready' AND p.expires_at>clock_timestamp() AND p.revision=$3,p.revision,coalesce(encode(p.manifest_digest,'hex'),''),coalesce((SELECT id::text FROM cosmoformer.artifacts WHERE plan_id=p.id AND attempt_id=p.attempt_id AND kind='normalized_changes' AND state='finalized' ORDER BY created_at DESC,id DESC LIMIT 1),'') FROM cosmoformer.plans p JOIN cosmoformer.operations o ON o.id=p.operation_id JOIN cosmoformer.stacks s ON s.id=p.stack_id WHERE p.id=$1 AND o.run_id=$2`, in.Plan, in.ID, in.Revision).Scan(&project, &env, &available, &out.Body.Revision, &out.Body.Manifest, &artifact); e != nil {
					return store.ErrNotFound
				}
				grants, e := store.GrantsFor(ctx, tx, actor.ID)
				if e != nil {
					return e
				}
				if !access.Allows(grants, project, env, access.View) {
					return store.ErrForbidden
				}
				if !available {
					return store.ErrConflict
				}
				return fn(tx)
			})
		}
		if e := authorize(func(pgx.Tx) error { return nil }); e != nil {
			return nil, fail(ctx, e)
		}
		if a.Evidence == nil {
			return nil, problem(ctx, 503, "Plan evidence is unavailable.")
		}
		_, f, e := a.Evidence.Read(ctx, authorize, artifact)
		if e != nil {
			return nil, evidenceError(ctx, e)
		}
		defer f.Close()
		defer os.Remove(f.Name())
		raw, e := io.ReadAll(io.LimitReader(f, plan.MaxBytes+1))
		if e != nil || len(raw) > plan.MaxBytes {
			return nil, problem(ctx, 409, "Plan evidence is unavailable.")
		}
		var analysis plan.Analysis
		if jsonErr := decodeAnalysis(raw, &analysis); jsonErr != nil {
			return nil, problem(ctx, 409, "Plan evidence is unavailable.")
		}
		// Verify authorization again after external custody I/O; a concurrent access
		// revocation never borrows the earlier permission to expose sanitized data.
		if e = authorize(func(pgx.Tx) error { return nil }); e != nil {
			return nil, fail(ctx, e)
		}
		out.Body.PlanID = in.Plan
		out.Body.Changes = (*SanitizedChanges)(analysis.Changes)
		return out, nil
	})
}
func decodeAnalysis(raw []byte, out *plan.Analysis) error {
	if e := runs.Decode(raw, out); e != nil {
		return e
	}
	canonical, e := catalog.Canonical(out)
	if e != nil || !bytes.Equal(raw, canonical) || out.Changes == nil {
		return store.ErrConflict
	}
	return nil
}
