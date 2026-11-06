package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5"
)

type Server struct {
	Store  *store.Store
	Origin string
	Hasher *identity.Hasher
}

func New(s *store.Store, origin string) (http.Handler, huma.API) {
	server := &Server{Store: s, Origin: origin}
	if s != nil {
		server.Hasher = identity.NewHasher()
	}
	mux := http.NewServeMux()
	cfg := huma.DefaultConfig("Cosmoformer", "0.1.0-dev")
	cfg.OpenAPIPath = ""
	cfg.DocsPath = ""
	cfg.SchemasPath = ""
	api := humago.New(mux, cfg)
	server.registerBrowser(api)
	server.registerAdmin(api)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if s == nil || s.Ready(r.Context()) != nil {
			writeProblem(w, r, 503, "Not ready.")
			return
		}
		w.Write([]byte("ok\n"))
	})
	return server.middleware(mux), api
}
func register[I, O any](api huma.API, method, path, id string, fn func(context.Context, *I) (*O, error)) {
	huma.Register(api, huma.Operation{OperationID: id, Method: method, Path: "/api/v1" + path, MaxBodyBytes: 64 * 1024, Errors: []int{401, 403, 404, 409, 422, 429, 503}}, fn)
}
func fail(ctx context.Context, e error) error {
	switch {
	case errors.Is(e, store.ErrUnauthenticated):
		return problem(ctx, 401, "Sign in to continue.")
	case errors.Is(e, store.ErrForbidden):
		return problem(ctx, 403, "You don’t have permission to do that.")
	case errors.Is(e, store.ErrNotFound):
		return problem(ctx, 404, "This item is unavailable.")
	case errors.Is(e, store.ErrConflict):
		return problem(ctx, 409, "The record changed or conflicts with an existing record. Refresh and try again.")
	default:
		return problem(ctx, 503, "The service is temporarily unavailable.")
	}
}
func (a *Server) tx(ctx context.Context, org string, fn func(pgx.Tx, store.Actor) error) error {
	e := a.Store.ActorTx(ctx, request(ctx).Hash, org, fn)
	if e != nil {
		return fail(ctx, e)
	}
	return nil
}

type Empty struct{}
type OK struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func ok() *OK { o := &OK{}; o.Body.OK = true; return o }

type IDResult struct {
	Body struct {
		ID string `json:"id" format:"uuid"`
	}
}

func idResult(id string) *IDResult { o := &IDResult{}; o.Body.ID = id; return o }

func mutate[O any](a *Server, ctx context.Context, org string, once bool, authorize func(pgx.Tx, store.Actor) error, fn func(pgx.Tx, store.Actor) (*O, error)) (*O, error) {
	var out *O
	d := request(ctx)
	e := a.tx(ctx, org, func(tx pgx.Tx, actor store.Actor) error {
		if e := authorize(tx, actor); e != nil {
			return e
		}
		type receipt struct {
			Digest string          `json:"digest"`
			Result json.RawMessage `json:"result"`
			Once   bool            `json:"once_visible"`
		}
		old, e := store.JSON[receipt](ctx, tx, `SELECT cosmoformer.browser_request($1,$2)`, actor.ID, d.Key)
		if e == nil {
			if old.Digest != hex.EncodeToString(d.Digest) || old.Once {
				return store.ErrConflict
			}
			return json.Unmarshal(old.Result, &out)
		}
		if !errors.Is(e, store.ErrNotFound) {
			return e
		}
		out, e = fn(tx, actor)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(out)
		if e != nil {
			return e
		}
		if once {
			raw = []byte("{}")
		}
		_, e = tx.Exec(ctx, `SELECT cosmoformer.save_browser_request($1,$2,$3,$4::jsonb,$5)`, actor.ID, d.Key, d.Digest, string(raw), once)
		return e
	})
	return out, e
}
