package httpapi

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type CancelInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_attempt_revision" minimum:"1" maximum:"9007199254740991"`
	}
}
type ForceStopInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_attempt_revision" minimum:"1" maximum:"9007199254740991"`
		Context  struct {
			Stack       string `json:"stack_id" format:"uuid"`
			Environment string `json:"environment_id" format:"uuid"`
			Attempt     string `json:"attempt_id" format:"uuid"`
		} `json:"confirmed_context"`
		Confirm bool `json:"confirm"`
	}
}
type ControlOutput struct {
	Body struct {
		ID    string `json:"control_id" format:"uuid"`
		Kind  string `json:"kind"`
		State string `json:"state"`
	}
	Status int
}

func scopeAttempt(ctx context.Context, tx pgx.Tx, actor store.Actor, id string) error {
	var project, env string
	if e := tx.QueryRow(ctx, `SELECT s.project_id::text,s.environment_id::text FROM cosmoformer.attempts a JOIN cosmoformer.stacks s ON s.id=a.stack_id WHERE a.id=$1`, id).Scan(&project, &env); e != nil {
		return store.ErrNotFound
	}
	g, e := store.GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return e
	}
	if !access.Allows(g, project, env, access.Apply) {
		return store.ErrForbidden
	}
	return nil
}
func (a *Server) control(ctx context.Context, org, id string, revision int64, kind, stack, env, confirmedAttempt string, confirmed bool) (*ControlOutput, error) {
	return mutate(a, ctx, org, false, func(tx pgx.Tx, actor store.Actor) error { return scopeAttempt(ctx, tx, actor, id) }, func(tx pgx.Tx, actor store.Actor) (*ControlOutput, error) {
		var current int64
		var closed bool
		var actualStack, actualEnv string
		e := tx.QueryRow(ctx, `SELECT a.revision,a.closed_at IS NOT NULL,a.stack_id::text,s.environment_id::text FROM cosmoformer.attempts a JOIN cosmoformer.stacks s ON s.id=a.stack_id WHERE a.id=$1`, id).Scan(&current, &closed, &actualStack, &actualEnv)
		if e != nil {
			return nil, e
		}
		if revision != current || closed {
			return nil, store.ErrConflict
		}
		if kind == "force_stop" && (!confirmed || stack != actualStack || env != actualEnv || confirmedAttempt != id) {
			return nil, problem(ctx, 422, "Confirm the exact stack, environment and attempt before Force Stop.")
		}
		out := &ControlOutput{Status: 202}
		e = tx.QueryRow(ctx, `INSERT INTO cosmoformer.control_requests(org_id,attempt_id,actor_id,kind,command_ordinal,confirmed,state,requested_at,worker_id,generation,agent_instance) SELECT org_id,id,$2,$3,command_ordinal,$4,'requested',clock_timestamp(),worker_id,generation,agent_instance FROM cosmoformer.attempts WHERE id=$1 RETURNING id::text,kind,state`, id, actor.ID, kind, confirmed).Scan(&out.Body.ID, &out.Body.Kind, &out.Body.State)
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET cancel_requested=true,revision=revision+1 WHERE id=(SELECT operation_id FROM cosmoformer.attempts WHERE id=$1)`, id); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET revision=revision+1 WHERE id=$1`, id); e != nil {
			return nil, e
		}
		return out, store.Audit(ctx, tx, org, actor.ID, "attempt."+kind, id)
	})
}
func (a *Server) registerControls(api huma.API) {
	register(api, "POST", "/organizations/{org}/attempts/{id}/cancel", "cancel-attempt", func(ctx context.Context, in *CancelInput) (*ControlOutput, error) {
		return a.control(ctx, in.Org, in.ID, in.Body.Revision, "cancel", "", "", "", false)
	})
	register(api, "POST", "/organizations/{org}/attempts/{id}/force-stop", "force-stop-attempt", func(ctx context.Context, in *ForceStopInput) (*ControlOutput, error) {
		return a.control(ctx, in.Org, in.ID, in.Body.Revision, "force_stop", in.Body.Context.Stack, in.Body.Context.Environment, in.Body.Context.Attempt, in.Body.Confirm)
	})
}

type ControlPublic struct {
	ID      string `json:"control_id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Ordinal int64  `json:"command_ordinal"`
	Signal  string `json:"signal_sent"`
}

func readControls(ctx context.Context, tx pgx.Tx, attempt string) ([]ControlPublic, error) {
	rows, e := tx.Query(ctx, `SELECT id::text,kind,state,command_ordinal,coalesce(signal_sent,'') FROM cosmoformer.control_requests WHERE attempt_id=$1 ORDER BY requested_at DESC,id DESC LIMIT 100`, attempt)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []ControlPublic{}
	for rows.Next() {
		var c ControlPublic
		if e = rows.Scan(&c.ID, &c.Kind, &c.State, &c.Ordinal, &c.Signal); e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
