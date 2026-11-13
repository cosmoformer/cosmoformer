package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/jackc/pgx/v5"
)

type Server struct {
	Cleanup     evidence.CleanupSettings
	Evidence    *evidence.Custody
	Previews    *evidence.Previews
	Store       *store.Store
	Origin      string
	Hasher      *identity.Hasher
	Coordinator *coordinator.Coordinator
	Bitbucket   *bitbucket.Client
	Cipher      *cryptostore.Cipher
	Quota       cryptostore.Quota
}

func New(s *store.Store, origin string) (http.Handler, huma.API) {
	return NewWithOptions(s, origin, Options{})
}

type Options struct {
	Cleanup                                                   evidence.CleanupSettings
	Evidence                                                  *evidence.Custody
	Clock                                                     clock.Source
	Bitbucket                                                 *bitbucket.Client
	Cipher                                                    *cryptostore.Cipher
	Background                                                context.Context
	NativeEvidence                                            coordinator.NativeEvidenceVerifier
	PlanSeconds, ApplySeconds, PreflightSeconds, PlanTTLHours int64
}

func NewWithOptions(s *store.Store, origin string, options Options) (http.Handler, huma.API) {
	if options.Clock == nil {
		options.Clock = clock.New()
	}
	if options.Bitbucket == nil {
		options.Bitbucket = bitbucket.New(nil)
	}
	server := &Server{Store: s, Origin: origin, Coordinator: coordinator.New(s, options.Clock), Bitbucket: options.Bitbucket, Cipher: options.Cipher}
	server.Cleanup = options.Cleanup
	if server.Cleanup == (evidence.CleanupSettings{}) {
		server.Cleanup = evidence.DefaultCleanupSettings()
	}
	server.Evidence = options.Evidence
	server.Coordinator.Evidence = options.Evidence
	server.Coordinator.NativeEvidence = options.NativeEvidence
	if server.Coordinator.NativeEvidence == nil {
		server.Coordinator.NativeEvidence = plan.Verifier{}
	}
	if options.PlanSeconds != 0 {
		server.Coordinator.PlanSeconds = options.PlanSeconds
	}
	if options.ApplySeconds != 0 {
		server.Coordinator.ApplySeconds = options.ApplySeconds
	}
	if options.PreflightSeconds != 0 {
		server.Coordinator.PreflightSeconds = options.PreflightSeconds
	}
	if options.PlanTTLHours != 0 {
		server.Coordinator.PlanTTLHours = options.PlanTTLHours
	}
	server.Coordinator.ManagedSecrets = server.currentExecutionSecrets
	server.Coordinator.ObserveSource = server.workerSourceObservation
	server.Previews = evidence.NewPreviews(runs.ID)
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
	server.registerCleanup(api)
	server.registerConfiguration(api)
	server.registerModules(api)
	server.registerDependencies(api)
	server.registerPreflights(api)
	server.registerPlanningReads(api)
	server.registerApprovals(api)
	server.registerApply(api)
	server.registerControls(api)
	server.registerRecovery(api)
	server.registerWorkspace(api)
	server.registerWorker(api)
	server.registerEvidence(api)
	server.registerWebhook(api)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if s == nil || s.Ready(r.Context()) != nil {
			writeProblem(w, r, 503, "Not ready.")
			return
		}
		w.Write([]byte("ok\n"))
	})
	if options.Background != nil && s != nil {
		go server.sourceJobs(options.Background)
		if server.Cleanup.Enabled && server.Cleanup.Validate() == nil && server.Evidence != nil {
			go server.cleanupJobs(options.Background)
		}
	}
	return server.middleware(mux), api
}
func register[I, O any](api huma.API, method, path, id string, fn func(context.Context, *I) (*O, error)) {
	huma.Register(api, huma.Operation{OperationID: id, Method: method, Path: "/api/v1" + path, MaxBodyBytes: 64 * 1024, Errors: []int{401, 403, 404, 409, 422, 429, 503}}, fn)
}
func fail(ctx context.Context, e error) error {
	var p *Problem
	if errors.As(e, &p) {
		return p
	}
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

// Call only after current authorization. Source actions use this before external
// reads, and mutate checks again atomically before creating any new effects.
func browserReceipt[O any](ctx context.Context, tx pgx.Tx, actor store.Actor) (*O, bool, error) {
	d := request(ctx)
	type receipt struct {
		Digest string          `json:"digest"`
		Result json.RawMessage `json:"result"`
		Once   bool            `json:"once_visible"`
	}
	old, e := store.JSON[receipt](ctx, tx, `SELECT cosmoformer.browser_request($1,$2)`, actor.ID, d.Key)
	if errors.Is(e, store.ErrNotFound) {
		return nil, false, nil
	}
	if e != nil {
		return nil, false, e
	}
	if old.Once {
		p := problem(ctx, 409, "Query current metadata and explicitly confirm a new secret write.")
		p.Code = "RECONFIRM_SECRET_WRITE"
		return nil, true, p
	}
	if old.Digest != hex.EncodeToString(d.Digest) {
		return nil, true, store.ErrConflict
	}
	var out *O
	e = json.Unmarshal(old.Result, &out)
	return out, true, e
}

func mutate[O any](a *Server, ctx context.Context, org string, once bool, authorize func(pgx.Tx, store.Actor) error, fn func(pgx.Tx, store.Actor) (*O, error)) (*O, error) {
	var out *O
	d := request(ctx)
	e := a.tx(ctx, org, func(tx pgx.Tx, actor store.Actor) error {
		if e := authorize(tx, actor); e != nil {
			return e
		}
		var found bool
		var e error
		out, found, e = browserReceipt[O](ctx, tx, actor)
		if found || e != nil {
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
