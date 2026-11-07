package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type integrationCredentials struct {
	ClientID      string           `json:"client_id"`
	ClientSecret  string           `json:"client_secret"`
	Tokens        bitbucket.Tokens `json:"tokens"`
	ExpiresAt     time.Time        `json:"expires_at"`
	WebhookSecret string           `json:"webhook_secret"`
}
type integrationRecord struct {
	Credentials        integrationCredentials
	Revision, Semantic int64
	Reconnect          bool
	Encrypted          []byte
}

func (a *Server) loadIntegration(ctx context.Context, tx pgx.Tx, org, id string) (integrationRecord, error) {
	var r integrationRecord
	e := tx.QueryRow(ctx, `SELECT encrypted_credentials,revision,semantic_revision,reconnect_required FROM cosmoformer.integrations WHERE id=$1 AND enabled`, id).Scan(&r.Encrypted, &r.Revision, &r.Semantic, &r.Reconnect)
	if e != nil {
		return r, store.ErrNotFound
	}
	if len(r.Encrypted) == 0 {
		return r, nil
	}
	if a.Cipher == nil {
		return r, store.ErrForbidden
	}
	raw, e := a.Cipher.Open(r.Encrypted, cryptostore.AAD(org, id, "integration", uint64(r.Semantic)))
	if e != nil {
		return r, e
	}
	if json.Unmarshal(raw, &r.Credentials) != nil {
		return r, store.ErrForbidden
	}
	return r, nil
}
func (a *Server) serviceTx(ctx context.Context, org string, fn func(pgx.Tx) error) error {
	tx, e := a.Store.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, org); e != nil {
		return e
	}
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit(ctx)
}

// apiToken single-flights rotating refresh credentials with a durable claim.
// A lost/failed refresh requires reconnection; no timeout guesses a safe retry.
func (a *Server) apiToken(ctx context.Context, org, id string) (string, error) {
	var record integrationRecord
	claim := ""
	e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		var e error
		record, e = a.loadIntegration(ctx, tx, org, id)
		if e != nil {
			return e
		}
		if record.Reconnect {
			return errors.New("source reconnect required")
		}
		if len(record.Encrypted) == 0 {
			return nil
		}
		if record.Credentials.Tokens.AccessToken != "" && time.Now().Add(time.Minute).Before(record.Credentials.ExpiresAt) {
			return nil
		}
		if record.Credentials.Tokens.RefreshToken == "" {
			return errors.New("source reconnect required")
		}
		claim = runs.ID()
		tag, e := tx.Exec(ctx, `UPDATE cosmoformer.integrations SET refresh_claim=$2,refresh_claimed_at=clock_timestamp() WHERE id=$1 AND refresh_claim IS NULL AND NOT reconnect_required`, id, claim)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return errors.New("source refresh in progress; reconnect if interrupted")
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	if claim == "" {
		return record.Credentials.Tokens.AccessToken, nil
	}
	tokens, e := a.Bitbucket.Refresh(ctx, record.Credentials.ClientID, record.Credentials.ClientSecret, record.Credentials.Tokens.RefreshToken)
	if e != nil || !bitbucket.Scopes(tokens.Scope) {
		a.serviceTx(ctx, org, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE cosmoformer.integrations SET reconnect_required=true,revision=revision+1 WHERE id=$1 AND refresh_claim=$2`, id, claim)
			return e
		})
		return "", bitbucket.ErrRemote
	}
	record.Credentials.Tokens = tokens
	record.Credentials.ExpiresAt = time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	sealed, e := a.sealRevision(ctx, org, id, "integration", record.Semantic, jsonBytes(record.Credentials))
	if e != nil {
		return "", e
	}
	e = a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE cosmoformer.integrations SET encrypted_credentials=$3,refresh_claim=NULL,refresh_claimed_at=NULL,revision=revision+1 WHERE id=$1 AND refresh_claim=$2 AND revision=$4 AND enabled AND NOT reconnect_required`, id, claim, sealed, record.Revision)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return store.ErrConflict
		}
		return nil
	})
	if e != nil {
		return "", e
	}
	return tokens.AccessToken, nil
}

type IntegrationCredentialInput struct {
	OrgIDPath
	Body struct {
		Revision      int64  `json:"expected_revision" minimum:"1"`
		ClientID      string `json:"client_id" minLength:"1" maxLength:"256"`
		ClientSecret  string `json:"client_secret" minLength:"1" maxLength:"16384"`
		WebhookSecret string `json:"webhook_secret" minLength:"32" maxLength:"256"`
		HookUUID      string `json:"hook_uuid" minLength:"1" maxLength:"128"`
	}
}
type ConnectOutput struct {
	Body struct {
		AuthorizationURL string    `json:"authorization_url"`
		ExpiresAt        time.Time `json:"expires_at"`
	}
}
type CallbackInput struct {
	State string `query:"state"`
	Code  string `query:"code"`
}
type CallbackOutput struct {
	Location string `header:"Location"`
	Body     struct {
		Connected bool `json:"connected"`
	}
}

func (a *Server) registerIntegration(api huma.API) {
	register(api, "POST", "/organizations/{org}/integrations/{id}/credentials", "configure-integration", func(ctx context.Context, in *IntegrationCredentialInput) (*OK, error) {
		var old integrationRecord
		if e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return e
			}
			var e error
			old, e = a.loadIntegration(ctx, tx, in.Org, in.ID)
			return e
		}); e != nil {
			return nil, e
		}
		if old.Revision != in.Body.Revision {
			return nil, problem(ctx, 409, "Integration settings changed.")
		}
		credentials := integrationCredentials{ClientID: in.Body.ClientID, ClientSecret: in.Body.ClientSecret, WebhookSecret: in.Body.WebhookSecret}
		sealed, e := a.sealRevision(ctx, in.Org, in.ID, "integration", old.Semantic+1, jsonBytes(credentials))
		if e != nil {
			return nil, fail(ctx, e)
		}
		return mutate(a, ctx, in.Org, true, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.integrations SET encrypted_credentials=$3,semantic_revision=semantic_revision+1,revision=revision+1,reconnect_required=false,refresh_claim=NULL,refresh_claimed_at=NULL,hook_uuid=$4 WHERE id=$1 AND revision=$2`, in.ID, in.Body.Revision, sealed, in.Body.HookUUID)
			if e != nil {
				return nil, e
			}
			if tag.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "integration.configure", in.ID)
		})
	})
	register(api, "POST", "/organizations/{org}/integrations/{id}/connect", "connect-integration", func(ctx context.Context, in *OrgIDPath) (*ConnectOutput, error) {
		out := &ConnectOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return e
			}
			record, e := a.loadIntegration(ctx, tx, in.Org, in.ID)
			if e != nil {
				return e
			}
			if record.Credentials.ClientID == "" {
				return store.ErrConflict
			}
			state := identity.Token()
			out.Body.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
			_, e = tx.Exec(ctx, `DELETE FROM cosmoformer.oauth_states WHERE expires_at<clock_timestamp() OR (actor_id=$1 AND integration_id=$2)`, actor.ID, in.ID)
			if e != nil {
				return e
			}
			_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.oauth_states(org_id,integration_id,actor_id,verifier,session_verifier,expires_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '10 minutes')`, in.Org, in.ID, actor.ID, identity.Digest(state), request(ctx).Hash)
			if e != nil {
				return e
			}
			q := url.Values{"client_id": {record.Credentials.ClientID}, "response_type": {"code"}, "state": {state}, "scope": {"repository pullrequest:write webhook"}, "redirect_uri": {a.Origin + "/oauth/bitbucket/callback"}}
			out.Body.AuthorizationURL = "https://bitbucket.org/site/oauth2/authorize?" + q.Encode()
			return nil
		})
		return out, e
	})
	huma.Register(api, huma.Operation{OperationID: "bitbucket-oauth-callback", Method: "GET", Path: "/oauth/bitbucket/callback", DefaultStatus: 303, Errors: []int{401, 403, 409, 422, 503}}, func(ctx context.Context, in *CallbackInput) (*CallbackOutput, error) {
		if !identity.ValidToken(in.State) || in.Code == "" || len(in.Code) > 4096 {
			return nil, problem(ctx, 422, "Invalid integration callback.")
		}
		var binding struct {
			Org string `json:"org_id"`
			ID  string `json:"integration_id"`
		}
		e := a.Store.Pool.QueryRow(ctx, `SELECT cosmoformer.consume_oauth_state($1,$2)`, identity.Digest(in.State), request(ctx).Hash).Scan(newJSON(&binding))
		if e != nil || binding.ID == "" {
			return nil, problem(ctx, 403, "The connection request is expired or belongs to another session.")
		}
		var old integrationRecord
		if e = a.tx(ctx, binding.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, binding.Org); e != nil {
				return e
			}
			var e error
			old, e = a.loadIntegration(ctx, tx, binding.Org, binding.ID)
			return e
		}); e != nil {
			return nil, e
		}
		tokens, e := a.Bitbucket.Exchange(ctx, old.Credentials.ClientID, old.Credentials.ClientSecret, in.Code, a.Origin+"/oauth/bitbucket/callback")
		if e != nil || !bitbucket.Scopes(tokens.Scope) {
			return nil, problem(ctx, 409, "Reconnect using only the required repository, pull request and webhook scopes.")
		}
		old.Credentials.Tokens = tokens
		old.Credentials.ExpiresAt = time.Now().UTC().Add(time.Duration(tokens.ExpiresIn) * time.Second)
		sealed, e := a.sealRevision(ctx, binding.Org, binding.ID, "integration", old.Semantic, jsonBytes(old.Credentials))
		if e != nil {
			return nil, fail(ctx, e)
		}
		if e = a.tx(ctx, binding.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, binding.Org); e != nil {
				return e
			}
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.integrations SET encrypted_credentials=$3,revision=revision+1,reconnect_required=false,refresh_claim=NULL WHERE id=$1 AND revision=$2`, binding.ID, old.Revision, sealed)
			if e != nil {
				return e
			}
			if tag.RowsAffected() != 1 {
				return store.ErrConflict
			}
			return store.Audit(ctx, tx, binding.Org, actor.ID, "integration.connect", binding.ID)
		}); e != nil {
			return nil, e
		}
		out := &CallbackOutput{Location: a.Origin + "/organizations?org=" + url.QueryEscape(binding.Org) + "&page=settings"}
		out.Body.Connected = true
		return out, nil
	})
}
