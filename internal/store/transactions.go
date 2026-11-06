package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func classify(e error) error {
	if e == nil {
		return nil
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(e, &p) {
		switch p.Code {
		case "42501":
			return ErrForbidden
		case "23505", "23503", "23514", "22P02":
			return ErrConflict
		}
	}
	return e
}
func (s *Store) ActorTx(ctx context.Context, sessionHash []byte, org string, fn func(pgx.Tx, Actor) error) error {
	tx, e := s.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	// Account/grant changes and authenticated mutations share this brief gate.
	if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
		return e
	}
	actor, e := loadActor(ctx, tx, sessionHash)
	if e != nil {
		return e
	}
	if org != "" {
		if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, org); e != nil {
			return classify(e)
		}
	}
	if e = fn(tx, actor); e != nil {
		return classify(e)
	}
	return classify(tx.Commit(ctx))
}
func JSON[T any](ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string, args ...any) (T, error) {
	var out T
	var b []byte
	e := q.QueryRow(ctx, sql, args...).Scan(&b)
	if e != nil {
		return out, classify(e)
	}
	if len(b) == 0 || string(b) == "null" {
		return out, ErrNotFound
	}
	e = json.Unmarshal(b, &out)
	return out, e
}
func Audit(ctx context.Context, tx pgx.Tx, org, actor, action, subject string) error {
	_, e := tx.Exec(ctx, `INSERT INTO cosmoformer.audit_events(org_id,actor_id,action,subject_id) VALUES($1,$2,$3,$4)`, org, actor, action, subject)
	return e
}
