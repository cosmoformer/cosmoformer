package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type WebhookInput struct {
	Org       string `path:"org" format:"uuid"`
	ID        string `path:"id" format:"uuid"`
	Signature string `header:"X-Hub-Signature"`
	Hook      string `header:"X-Hook-UUID"`
	Delivery  string `header:"X-Request-UUID"`
	Event     string `header:"X-Event-Key"`
	RawBody   []byte
}

func (a *Server) registerWebhook(api huma.API) {
	huma.Register(api, huma.Operation{OperationID: "bitbucket-webhook", Method: "POST", Path: "/webhooks/bitbucket/{org}/{id}", MaxBodyBytes: 1048576, Errors: []int{403, 409, 422, 503}}, func(ctx context.Context, in *WebhookInput) (*OK, error) {
		if !catalog.UUID(strings.Trim(in.Delivery, "{}")) || len(in.RawBody) > 1048576 {
			return nil, problem(ctx, 422, "Invalid webhook delivery.")
		}
		switch in.Event {
		case "repo:push", "pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected":
		default:
			return nil, problem(ctx, 422, "Unsupported webhook event.")
		}
		var record integrationRecord
		var expectedHook string
		e := a.serviceTx(ctx, in.Org, func(tx pgx.Tx) error {
			var e error
			record, e = a.loadIntegration(ctx, tx, in.Org, in.ID)
			if e != nil {
				return e
			}
			return tx.QueryRow(ctx, `SELECT coalesce(hook_uuid,'') FROM cosmoformer.integrations WHERE id=$1`, in.ID).Scan(&expectedHook)
		})
		if e != nil || expectedHook == "" || expectedHook != in.Hook || !bitbucket.Signature(in.RawBody, in.Signature, []byte(record.Credentials.WebhookSecret)) {
			return nil, problem(ctx, 403, "Invalid webhook signature or hook identity.")
		}
		var body struct {
			Repository struct {
				UUID string `json:"uuid"`
			} `json:"repository"`
			PR struct {
				ID int `json:"id"`
			} `json:"pullrequest"`
		}
		if json.Unmarshal(in.RawBody, &body) != nil || body.Repository.UUID == "" {
			return nil, problem(ctx, 422, "Invalid webhook payload.")
		}
		if strings.HasPrefix(in.Event, "pullrequest:") && body.PR.ID < 1 {
			return nil, problem(ctx, 422, "Missing pull request identity.")
		}
		sum := sha256.Sum256(in.RawBody)
		e = a.serviceTx(ctx, in.Org, func(tx pgx.Tx) error {
			var valid bool
			if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.integrations WHERE id=$1 AND enabled AND semantic_revision=$2 AND hook_uuid=$3)`, in.ID, record.Semantic, in.Hook).Scan(&valid); e != nil {
				return e
			}
			if !valid {
				return store.ErrForbidden
			}
			var repo string
			if e := tx.QueryRow(ctx, `SELECT id::text FROM cosmoformer.repositories WHERE integration_id=$1 AND external_id=$2 AND enabled`, in.ID, body.Repository.UUID).Scan(&repo); e != nil {
				return store.ErrForbidden
			}
			var old []byte
			e := tx.QueryRow(ctx, `SELECT body_digest FROM cosmoformer.webhook_receipts WHERE integration_id=$1 AND delivery_id=$2`, in.ID, in.Delivery).Scan(&old)
			if e == nil {
				if !identity.Equal(old, sum[:]) {
					return store.ErrConflict
				}
				return nil
			}
			if e != pgx.ErrNoRows {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.webhook_receipts(org_id,integration_id,delivery_id,repository_id,body_digest,accepted_at) VALUES($1,$2,$3,$4,$5,clock_timestamp())`, in.Org, in.ID, in.Delivery, repo, sum[:]); e != nil {
				return e
			}
			kind := "branch"
			if strings.HasPrefix(in.Event, "pullrequest:") {
				kind = "pr"
			}
			_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.outbox(org_id,kind,subject_id,payload,status,next_attempt_at,delivery_key) VALUES($1,'source_reconcile',$2,$3,'pending',clock_timestamp(),$4) ON CONFLICT DO NOTHING`, in.Org, repo, jsonBytes(map[string]any{"source_kind": kind, "pr_number": body.PR.ID}), in.Delivery)
			return e
		})
		if e != nil {
			return nil, fail(ctx, e)
		}
		return ok(), nil
	})
}
