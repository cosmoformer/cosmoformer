package httpapi

import (
	"context"
	"encoding/json"
	"os"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type PreviewInput struct {
	EvidenceIdentity
	Body struct {
		Stream   string        `json:"stream"`
		Epoch    string        `json:"epoch"`
		Sequence int64         `json:"sequence"`
		Start    int64         `json:"byte_start"`
		Text     string        `json:"text"`
		Gap      *evidence.Gap `json:"gap,omitempty"`
	}
}
type PreviewOutput struct {
	Body struct {
		Cursor string `json:"cursor"`
	}
}
type LogsInput struct {
	Org     string `path:"org" format:"uuid"`
	Run     string `path:"run" format:"uuid"`
	Attempt string `query:"attempt_id" format:"uuid" required:"true"`
	Cursor  string `query:"cursor" maxLength:"1024"`
	Limit   int    `query:"limit_bytes" minimum:"1" maximum:"262144" default:"262144"`
}
type LogBatch struct {
	Cursor   string             `json:"cursor"`
	Reset    bool               `json:"reset"`
	Reason   string             `json:"reset_reason,omitempty"`
	Segments []evidence.Segment `json:"segments" nullable:"false"`
	Complete bool               `json:"archive_complete"`
	More     bool               `json:"has_more"`
}
type LogsOutput struct{ Body LogBatch }

func (a *Server) registerLogs(api huma.API) {
	workerRegister(api, "/attempts/{id}/log-preview", "worker-log-preview", func(ctx context.Context, in *PreviewInput) (*PreviewOutput, error) {
		if in.Body.Stream != "console" {
			return nil, problem(ctx, 422, "Preview text is required; only console is supported.")
		}
		s := evidence.Segment{Sequence: in.Body.Sequence, Start: in.Body.Start, End: in.Body.Start + int64(len(in.Body.Text)), Text: &in.Body.Text}
		if in.Body.Gap != nil {
			if in.Body.Text != "" {
				return nil, problem(ctx, 422, "Text and gap are exclusive.")
			}
			s.Text = nil
			s.Gap = in.Body.Gap
		}
		var epoch string
		e := a.evidenceTx(ctx, bearer(in.Authorization), in.identity(), in.ID, "log_segment")(func(tx pgx.Tx) error { var e error; epoch, e = a.Previews.Append(in.ID, in.Body.Epoch, s); return e })
		out := &PreviewOutput{}
		out.Body.Cursor = evidence.Cursor{Attempt: in.ID, Stream: "console", Epoch: epoch, Next: s.End}.Encode()
		return out, evidenceError(ctx, e)
	})
	register(api, "GET", "/organizations/{org}/runs/{run}/logs", "run-logs", func(ctx context.Context, in *LogsInput) (*LogsOutput, error) {
		cursor := evidence.Cursor{Attempt: in.Attempt, Stream: "console"}
		var e error
		if in.Cursor != "" {
			cursor, e = evidence.DecodeCursor(in.Cursor, in.Attempt)
			if e != nil {
				return nil, problem(ctx, 422, "Invalid log cursor.")
			}
		}
		authorize := func(fn func(pgx.Tx) error) error {
			return a.Store.ActorTx(ctx, request(ctx).Hash, in.Org, func(tx pgx.Tx, actor store.Actor) error {
				var project, env string
				e := tx.QueryRow(ctx, `SELECT s.project_id::text,s.environment_id::text FROM cosmoformer.attempts t JOIN cosmoformer.operations o ON o.id=t.operation_id JOIN cosmoformer.stacks s ON s.id=t.stack_id WHERE t.id=$1 AND o.run_id=$2`, in.Attempt, in.Run).Scan(&project, &env)
				if e != nil {
					return store.ErrNotFound
				}
				g, e := store.GrantsFor(ctx, tx, actor.ID)
				if e != nil {
					return e
				}
				if !access.Allows(g, project, env, access.View) {
					return store.ErrForbidden
				}
				return fn(tx)
			})
		}
		type row struct {
			id string
			s  evidence.Segment
		}
		rows := []row{}
		var lastSequence, lastOffset int64
		e = authorize(func(tx pgx.Tx) error {
			if e := tx.QueryRow(ctx, `SELECT coalesce(max(sequence),-1),coalesce(max(last_offset),0) FROM cosmoformer.log_segments WHERE attempt_id=$1 AND stream='console'`, in.Attempt).Scan(&lastSequence, &lastOffset); e != nil {
				return e
			}
			r, e := tx.Query(ctx, `SELECT coalesce(artifact_id::text,''),sequence,first_offset,last_offset,gap_reason,unknown_extent FROM cosmoformer.log_segments WHERE attempt_id=$1 AND stream='console' AND sequence >= $2 ORDER BY sequence LIMIT 101`, in.Attempt, cursor.Archive)
			if e != nil {
				return e
			}
			defer r.Close()
			for r.Next() {
				var v row
				var reason *string
				var unknown bool
				if e = r.Scan(&v.id, &v.s.Sequence, &v.s.Start, &v.s.End, &reason, &unknown); e != nil {
					return e
				}
				if reason != nil {
					v.s.Gap = &evidence.Gap{Reason: *reason, Unknown: unknown}
				}
				rows = append(rows, v)
			}
			return r.Err()
		})
		if e != nil {
			return nil, fail(ctx, e)
		}
		epoch, preview := a.Previews.Snapshot(in.Attempt)
		unavailable := false
		out := &LogsOutput{Body: LogBatch{Segments: []evidence.Segment{}}}
		if cursor.Epoch != "" && cursor.Epoch != epoch {
			out.Body.Reset = true
			out.Body.Reason = "epoch_changed"
			cursor.Next = 0
		}
		maxOffset := lastOffset
		for _, s := range preview {
			maxOffset = max(maxOffset, s.End)
		}
		if cursor.Archive > lastSequence+1 || !out.Body.Reset && cursor.Next > maxOffset {
			return nil, problem(ctx, 422, "Log cursor exceeds retained bounds.")
		}
		cursor.Epoch = epoch
		left := in.Limit
	archiveRows:
		for i, v := range rows {
			if i == 100 || left <= 0 {
				out.Body.More = true
				break
			}
			s := v.s
			if v.id != "" {
				_, f, e := a.Evidence.Read(ctx, authorize, v.id)
				if e != nil {
					unavailable = true
					s.Gap = &evidence.Gap{Reason: "archive_unavailable"}
				} else {
					var payload evidence.Segment
					err := json.NewDecoder(f).Decode(&payload)
					f.Close()
					os.Remove(f.Name())
					if err != nil || payload.Validate() != nil || payload.Text == nil || payload.Sequence != s.Sequence || payload.Start != s.Start || payload.End != s.End {
						unavailable = true
						s.Gap = &evidence.Gap{Reason: "archive_invalid"}
					} else {
						offset := max(int64(0), cursor.Next-s.Start)
						if offset >= s.End-s.Start {
							cursor.Archive = s.Sequence + 1
							continue
						}
						text := (*payload.Text)[offset:]
						text, err = evidence.TextPrefix(text, left)
						if err != nil {
							if left < in.Limit {
								out.Body.More = true
								break archiveRows
							}
							return nil, problem(ctx, 422, "Increase the log byte limit or reset the cursor.")
						}
						s.Start += offset
						s.End = s.Start + int64(len(text))
						s.Text = &text
						left -= len(text)
					}
				}
			}
			out.Body.Segments = append(out.Body.Segments, s)
			cursor.Next = s.End
			if s.End == v.s.End {
				cursor.Archive = s.Sequence + 1
			} else {
				out.Body.More = true
				break
			}
		}
		if out.Body.Reset {
			out.Body.Segments = append(out.Body.Segments, evidence.Segment{Start: cursor.Next, End: cursor.Next, Gap: &evidence.Gap{Reason: "preview_reset", Unknown: true}})
		}
		if !out.Body.More {
			for _, s := range preview {
				if s.End <= cursor.Next {
					continue
				}
				if left <= 0 {
					out.Body.More = true
					break
				}
				if s.Start < cursor.Next {
					if s.Text != nil {
						v := (*s.Text)[cursor.Next-s.Start:]
						s.Text = &v
					}
					s.Start = cursor.Next
				}
				if s.Text != nil {
					v := *s.Text
					if len(v) > left {
						var e error
						v, e = evidence.TextPrefix(v, left)
						if e != nil {
							if left < in.Limit {
								out.Body.More = true
								break
							}
							return nil, problem(ctx, 422, "Increase the log byte limit or reset the cursor.")
						}
						out.Body.More = true
					}
					s.Text = &v
					s.End = s.Start + int64(len(v))
					left -= len(v)
				}
				out.Body.Segments = append(out.Body.Segments, s)
				cursor.Next = s.End
				if left == 0 {
					out.Body.More = false
					for _, pending := range preview {
						if pending.End > cursor.Next {
							out.Body.More = true
							break
						}
					}
					break
				}
			}
		}
		e = authorize(func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT closed_at IS NOT NULL AND coalesce(log_final->>'stream'='console' AND log_final->>'complete'='true' AND log_final->>'unknown_loss'='false',false) AND NOT EXISTS(SELECT FROM cosmoformer.log_segments WHERE attempt_id=$1 AND (gap OR (artifact_id IS NOT NULL AND NOT EXISTS(SELECT FROM cosmoformer.artifacts WHERE id=artifact_id AND state='finalized')))) FROM cosmoformer.attempts WHERE id=$1`, in.Attempt).Scan(&out.Body.Complete)
		})
		if e != nil {
			return nil, fail(ctx, e)
		}

		if unavailable {
			out.Body.Complete = false
		}
		out.Body.Cursor = cursor.Encode()
		return out, nil
	})
}
