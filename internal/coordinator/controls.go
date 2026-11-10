package coordinator

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

func (c *Coordinator) Controls(ctx context.Context, token string, id runs.Identity) ([]runs.Control, bool, error) {
	out := []runs.Control{}
	var paused bool
	e := c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, id, false); e != nil {
			return e
		}
		if e := tx.QueryRow(ctx, `UPDATE cosmoformer.workers SET last_contact_at=clock_timestamp() WHERE id=$1 RETURNING paused`, w.ID).Scan(&paused); e != nil {
			return e
		}
		rows, e := tx.Query(ctx, `SELECT c.id::text,c.attempt_id::text,c.worker_id::text,c.generation,c.agent_instance::text,c.kind,c.command_ordinal FROM cosmoformer.control_requests c JOIN cosmoformer.attempts a ON a.id=c.attempt_id WHERE c.worker_id=$1 AND c.generation=$2 AND c.agent_instance=$3 AND a.closed_at IS NULL AND c.state<>'observed' ORDER BY c.requested_at,c.id LIMIT 100`, w.ID, id.Generation, id.Agent)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var v runs.Control
			if e = rows.Scan(&v.ID, &v.Attempt, &v.Worker, &v.Generation, &v.Agent, &v.Kind, &v.Ordinal); e != nil {
				return e
			}
			out = append(out, v)
		}
		return rows.Err()
	})
	return out, paused, e
}
