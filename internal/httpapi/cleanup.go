package httpapi

import (
	"context"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type OperationalSettings struct {
	Cleanup           evidence.CleanupSettings `json:"cleanup"`
	PlanSeconds       int64                    `json:"plan_seconds"`
	ApplySeconds      int64                    `json:"apply_seconds"`
	PreflightSeconds  int64                    `json:"preflight_seconds"`
	PlanTTLHours      int64                    `json:"plan_ttl_hours"`
	Maintenance       bool                     `json:"maintenance"`
	Offline           bool                     `json:"offline"`
	StorageConfigured bool                     `json:"storage_configured"`
}
type SettingsOutput struct{ Body OperationalSettings }
type CleanupPreviewInput struct {
	Org     string `query:"org_id" format:"uuid" required:"true"`
	Cursor  string `query:"cursor" format:"uuid"`
	Outcome string `query:"outcome" enum:"eligible,protected,unresolved,pending,failed,completed,attention"`
	Limit   int    `query:"limit" minimum:"1" maximum:"100" default:"50"`
}
type CleanupPreviewOutput struct{ Body evidence.CleanupPage }
type CleanupInput struct {
	Body struct {
		Org   string `json:"org_id" format:"uuid"`
		Limit int    `json:"limit" minimum:"1" maximum:"100"`
	}
}
type CleanupAccepted struct {
	Body struct {
		Claims []string `json:"claim_ids" nullable:"false"`
	}
}

func cleanupAdmin(actor store.Actor) error {
	if !actor.InstallationAdmin {
		return store.ErrForbidden
	}
	return nil
}
func (a *Server) registerCleanup(api huma.API) {
	register(api, "GET", "/admin/settings", "operational-settings", func(ctx context.Context, _ *Empty) (*SettingsOutput, error) {
		out := &SettingsOutput{}
		e := a.tx(ctx, "", func(tx pgx.Tx, actor store.Actor) error {
			if e := cleanupAdmin(actor); e != nil {
				return e
			}
			out.Body = OperationalSettings{Cleanup: a.Cleanup, PlanSeconds: a.Coordinator.PlanSeconds, ApplySeconds: a.Coordinator.ApplySeconds, PreflightSeconds: a.Coordinator.PreflightSeconds, PlanTTLHours: a.Coordinator.PlanTTLHours, StorageConfigured: a.Evidence != nil && a.Evidence.Storage != nil}
			return tx.QueryRow(ctx, `SELECT maintenance,offline FROM cosmoformer.installation`).Scan(&out.Body.Maintenance, &out.Body.Offline)
		})
		return out, e
	})
	register(api, "GET", "/admin/cleanup-preview", "cleanup-preview", func(ctx context.Context, in *CleanupPreviewInput) (*CleanupPreviewOutput, error) {
		out := &CleanupPreviewOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := cleanupAdmin(actor); e != nil {
				return e
			}
			var e error
			out.Body, e = evidence.PreviewCleanup(ctx, tx, a.Cleanup.RetentionDays, in.Cursor, in.Outcome, in.Limit)
			return e
		})
		return out, e
	})
	register(api, "POST", "/admin/cleanup", "cleanup", func(ctx context.Context, in *CleanupInput) (*CleanupAccepted, error) {
		ctx, cancelRun := context.WithTimeout(ctx, evidence.CleanupRunTimeout)
		defer cancelRun()
		// Current authority is checked before reading the mutation receipt. The
		// receipt fixes this action's claim set; replay cannot select another batch.
		out, e := mutate(a, ctx, in.Body.Org, false, func(_ pgx.Tx, actor store.Actor) error { return cleanupAdmin(actor) }, func(tx pgx.Tx, actor store.Actor) (*CleanupAccepted, error) {
			if in.Body.Limit > a.Cleanup.Batch {
				return nil, problem(ctx, 422, "Requested batch exceeds the configured cleanup bound.")
			}
			if a.Evidence == nil || a.Evidence.Storage == nil {
				return nil, problem(ctx, 503, "Artifact storage is unavailable.")
			}
			result := &CleanupAccepted{}
			var err error
			result.Body.Claims, err = evidence.ClaimCleanup(ctx, tx, a.Cleanup, in.Body.Limit)
			if err != nil {
				return nil, err
			}
			if err = store.Audit(ctx, tx, in.Body.Org, actor.ID, "cleanup.requested", in.Body.Org); err != nil {
				return nil, err
			}
			return result, nil
		})
		if e != nil {
			return nil, e
		}
		bounded, cancel := context.WithTimeout(ctx, evidence.CleanupRunTimeout)
		defer cancel()
		var pending []string
		if e = a.tx(bounded, in.Body.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := cleanupAdmin(actor); e != nil {
				return e
			}
			var e error
			pending, e = evidence.PendingCleanup(bounded, tx, out.Body.Claims, min(in.Body.Limit, a.Cleanup.Batch))
			return e
		}); e != nil {
			return nil, e
		}
		for _, id := range pending {
			if bounded.Err() != nil {
				break
			}
			// Authorization is rechecked before every storage admission and write.
			transaction := func(fn func(pgx.Tx) error) error {
				return a.Store.ActorTx(bounded, request(ctx).Hash, in.Body.Org, func(tx pgx.Tx, actor store.Actor) error {
					if e := cleanupAdmin(actor); e != nil {
						return e
					}
					return fn(tx)
				})
			}
			_ = a.Evidence.DeleteClaim(bounded, transaction, id)
		}
		return out, nil
	})
}

// At most one sequential batch per interval, at most 100 tenant visits, and a
// fixed wall-clock budget. Cancellation leaves exact claims for a later action.
func (a *Server) cleanupJobs(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(a.Cleanup.IntervalSeconds) * time.Second)
	defer ticker.Stop()
	cursor := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		bounded, cancel := context.WithTimeout(ctx, evidence.CleanupRunTimeout)
		cursor = a.cleanupTick(bounded, cursor)
		cancel()
	}
}
func (a *Server) cleanupTick(ctx context.Context, after string) string {
	rows, e := a.Store.Pool.Query(ctx, `SELECT cosmoformer.cleanup_tenants(NULLIF($1,'')::uuid)::text`, after)
	if e != nil {
		return after
	}
	tenants := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return after
		}
		tenants = append(tenants, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return after
	}
	left := a.Cleanup.Batch
	cursor := after
	for _, org := range tenants {
		if ctx.Err() != nil || left == 0 {
			break
		}
		claims := []string{}
		e = a.serviceTx(ctx, org, func(tx pgx.Tx) error {
			var e error
			claims, e = evidence.ClaimCleanup(ctx, tx, a.Cleanup, left)
			return e
		})
		cursor = org
		if e != nil {
			continue
		}
		left -= len(claims)
		for _, id := range claims {
			if ctx.Err() != nil {
				break
			}
			_ = a.Evidence.DeleteClaim(ctx, func(fn func(pgx.Tx) error) error { return a.serviceTx(ctx, org, fn) }, id)
		}
	}
	if len(tenants) < 100 && (len(tenants) == 0 || cursor == tenants[len(tenants)-1]) {
		return ""
	}
	return cursor
}
