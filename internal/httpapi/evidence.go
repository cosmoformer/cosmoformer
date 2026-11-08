package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

func evidenceError(ctx context.Context, e error) error {
	if errors.Is(e, evidence.ErrUploadIncomplete) {
		p := problem(ctx, 409, "Upload has no retained version; retry the same evidence with a new upload ID.")
		p.Code = "UPLOAD_INCOMPLETE"
		return p
	}
	if errors.Is(e, evidence.ErrInvalid) {
		return problem(ctx, 422, "Invalid evidence request.")
	}
	return workerError(ctx, e)
}
func (a *Server) evidenceTx(ctx context.Context, token string, id runs.Identity, attempt, kind string, read ...bool) evidence.Transaction {
	return func(fn func(pgx.Tx) error) error {
		with := a.Coordinator.WithEvidenceAttempt
		if len(read) > 0 {
			with = a.Coordinator.WithAttempt
		}
		return with(ctx, token, id, attempt, func(tx pgx.Tx, w coordinator.Worker, assignment runs.Assignment) error {
			// Preflight is source-only; its only retained output is redacted console logs.
			if assignment.Phase != "execution" && kind != "log_segment" {
				return store.ErrForbidden
			}
			return fn(tx)
		})
	}
}

type EvidenceIdentity struct {
	Authorization string `header:"Authorization"`
	Worker        string `header:"X-Worker-ID"`
	Generation    int64  `header:"X-Worker-Generation"`
	Agent         string `header:"X-Agent-Instance"`
	ID            string `path:"id" format:"uuid"`
}

func (i EvidenceIdentity) identity() runs.Identity {
	return runs.Identity{WorkerID: i.Worker, Generation: i.Generation, Agent: i.Agent}
}

type FinalizeInput struct {
	EvidenceIdentity
	Upload string `path:"upload" format:"uuid"`
	Body   struct {
		Request string `json:"request_id" format:"uuid"`
		Kind    string `json:"kind"`
		Hash    string `json:"sha256"`
		Size    int64  `json:"size"`
	}
}
type UploadReceipt struct {
	ID       string `json:"artifact_id"`
	Upload   string `json:"upload_id"`
	Received int64  `json:"received_bytes"`
	Hash     string `json:"sha256"`
}
type FinalizedReceipt struct {
	ID      string `json:"artifact_id"`
	Status  string `json:"status"`
	Version string `json:"version_id"`
	Hash    string `json:"sha256"`
	Size    int64  `json:"size"`
}
type ArtifactOutput struct{ Body FinalizedReceipt }
type WorkerReadInput struct {
	EvidenceIdentity
	Artifact string `path:"artifact" format:"uuid"`
}
type BrowserReadInput struct {
	Org      string `path:"org" format:"uuid"`
	Artifact string `path:"artifact" format:"uuid"`
}

func fileResponse(f *os.File, size int64) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(ctx huma.Context) {
		defer f.Close()
		defer os.Remove(f.Name())
		ctx.SetHeader("Content-Type", "application/octet-stream")
		ctx.SetHeader("Content-Disposition", `attachment; filename="evidence.bin"`)
		ctx.SetHeader("Content-Length", strconv.FormatInt(size, 10))
		ctx.SetStatus(200)
		io.Copy(ctx.BodyWriter(), f)
	}}
}
func (a *Server) browserEvidenceTx(ctx context.Context, org, id string) evidence.Transaction {
	return func(fn func(pgx.Tx) error) error {
		return a.Store.ActorTx(ctx, request(ctx).Hash, org, func(tx pgx.Tx, actor store.Actor) error {
			var project, env, kind string
			e := tx.QueryRow(ctx, `SELECT s.project_id::text,s.environment_id::text,a.kind FROM cosmoformer.artifacts a JOIN cosmoformer.attempts t ON t.id=a.attempt_id JOIN cosmoformer.stacks s ON s.id=t.stack_id WHERE a.id=$1`, id).Scan(&project, &env, &kind)
			if e != nil {
				return store.ErrNotFound
			}
			grants, e := store.GrantsFor(ctx, tx, actor.ID)
			if e != nil {
				return e
			}
			if !access.Allows(grants, project, env, access.View) || evidence.Raw(kind) && !access.Allows(grants, project, env, access.RawEvidenceRead) {
				return store.ErrForbidden
			}
			return fn(tx)
		})
	}
}
func (a *Server) registerEvidence(api huma.API) {
	// Adapter handler streams rather than allowing the control DTO decoder to
	// allocate an artifact-sized request body. The same functional route is exported.
	op := &huma.Operation{OperationID: "worker-artifact-upload", Method: "PUT", Path: "/worker/v1/attempts/{id}/artifacts/{upload}", Summary: "Stream bounded exact-identity evidence", Responses: map[string]*huma.Response{"200": {Description: "Upload receipt"}}}
	for _, v := range []struct{ name, where, typ string }{{"id", "path", "string"}, {"upload", "path", "string"}, {"Authorization", "header", "string"}, {"X-Worker-ID", "header", "string"}, {"X-Worker-Generation", "header", "integer"}, {"X-Agent-Instance", "header", "string"}, {"Artifact-Kind", "header", "string"}, {"Content-Length", "header", "integer"}, {"Expected-SHA256", "header", "string"}} {
		op.Parameters = append(op.Parameters, &huma.Param{Name: v.name, In: v.where, Required: true, Schema: &huma.Schema{Type: v.typ}})
	}
	op.RequestBody = &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/octet-stream": {Schema: &huma.Schema{Type: "string", Format: "binary"}}}}
	op.Responses["200"].Content = map[string]*huma.MediaType{"application/json": {Schema: api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(UploadReceipt{}), true, "Artifact")}}
	api.OpenAPI().AddOperation(op)
	api.Adapter().Handle(op, func(h huma.Context) {
		ctx := h.Context()
		generation, _ := strconv.ParseInt(h.Header("X-Worker-Generation"), 10, 64)
		id := runs.Identity{WorkerID: h.Header("X-Worker-ID"), Generation: generation, Agent: h.Header("X-Agent-Instance")}
		size, e := strconv.ParseInt(h.Header("Content-Length"), 10, 64)
		if e != nil || h.Header("Content-Type") != "application/octet-stream" || !catalog.UUID(h.Param("id")) || !catalog.UUID(h.Param("upload")) {
			huma.WriteErr(api, h, 422, "Invalid upload identity.")
			return
		}
		kind := h.Header("Artifact-Kind")
		v, e := a.Evidence.Upload(ctx, a.evidenceTx(ctx, bearer(h.Header("Authorization")), id, h.Param("id"), kind), h.Param("id"), h.Param("upload"), kind, h.Header("Expected-SHA256"), size, h.BodyReader())
		if e != nil {
			p := evidenceError(ctx, e).(huma.StatusError)
			h.SetHeader("Content-Type", "application/json")
			h.SetStatus(p.GetStatus())
			json.NewEncoder(h.BodyWriter()).Encode(p)
			return
		}
		h.SetHeader("Content-Type", "application/json")
		h.SetStatus(200)
		json.NewEncoder(h.BodyWriter()).Encode(UploadReceipt{ID: v.ID, Upload: v.Upload, Received: v.Size, Hash: v.Hash})
	})
	workerRegister(api, "/attempts/{id}/artifacts/{upload}/finalize", "worker-artifact-finalize", func(ctx context.Context, in *FinalizeInput) (*ArtifactOutput, error) {
		if !catalog.UUID(in.Body.Request) {
			return nil, problem(ctx, 422, "A request ID is required.")
		}
		v, e := a.Evidence.Finalize(ctx, a.evidenceTx(ctx, bearer(in.Authorization), in.identity(), in.ID, in.Body.Kind), in.ID, in.Upload, in.Body.Kind, in.Body.Hash, in.Body.Size)
		return &ArtifactOutput{FinalizedReceipt{ID: v.ID, Status: v.State, Version: v.Version, Hash: v.Hash, Size: v.Size}}, evidenceError(ctx, e)
	})
	huma.Register(api, huma.Operation{OperationID: "worker-artifact-read", Method: "GET", Path: "/worker/v1/attempts/{id}/artifacts/{artifact}"}, func(ctx context.Context, in *WorkerReadInput) (*huma.StreamResponse, error) {
		tx := a.evidenceTx(ctx, bearer(in.Authorization), in.identity(), in.ID, "log_segment", true)
		restricted := func(fn func(pgx.Tx) error) error {
			return tx(func(q pgx.Tx) error {
				var attempt, kind string
				e := q.QueryRow(ctx, `SELECT attempt_id::text,kind FROM cosmoformer.artifacts WHERE id=$1`, in.Artifact).Scan(&attempt, &kind)
				if e != nil {
					return e
				}
				if attempt != in.ID {
					var allowed bool
					e = q.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifacts a JOIN cosmoformer.operations o ON o.plan_id=a.plan_id JOIN cosmoformer.attempts t ON t.operation_id=o.id JOIN cosmoformer.work_items w ON w.id=t.work_item_id WHERE a.id=$1 AND t.id=$2 AND o.kind='apply' AND w.phase='execution' AND t.state IN ('claimed','preparing','ready') AND a.state='finalized' AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=a.id))`, in.Artifact, in.ID).Scan(&allowed)
					if e != nil {
						return e
					}
					if !allowed {
						return store.ErrForbidden
					}
					if _, e = q.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT org_id,id,'attempt',$2,'retained_preparation' FROM cosmoformer.artifacts WHERE id=$1 ON CONFLICT DO NOTHING`, in.Artifact, in.ID); e != nil {
						return e
					}
				}
				if kind != "log_segment" {
					var phase string
					if e = q.QueryRow(ctx, `SELECT phase FROM cosmoformer.work_items WHERE id=(SELECT work_item_id FROM cosmoformer.attempts WHERE id=$1)`, in.ID).Scan(&phase); e != nil {
						return e
					}
					if phase != "execution" {
						return store.ErrForbidden
					}
				}
				return fn(q)
			})
		}
		v, f, e := a.Evidence.Read(ctx, restricted, in.Artifact)
		if e != nil {
			return nil, evidenceError(ctx, e)
		}
		return fileResponse(f, v.Size), nil
	})
	register(api, http.MethodGet, "/organizations/{org}/artifacts/{artifact}/content", "artifact-content", func(ctx context.Context, in *BrowserReadInput) (*huma.StreamResponse, error) {
		v, f, e := a.Evidence.Read(ctx, a.browserEvidenceTx(ctx, in.Org, in.Artifact), in.Artifact)
		if e != nil {
			return nil, evidenceError(ctx, e)
		}
		return fileResponse(f, v.Size), nil
	})
	a.registerLogs(api)
}
