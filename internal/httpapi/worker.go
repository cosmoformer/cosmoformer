package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

func workerRegister[I, O any](api huma.API, path, id string, fn func(context.Context, *I) (*O, error)) {
	huma.Register(api, huma.Operation{OperationID: id, Method: "POST", Path: "/worker/v1" + path, MaxBodyBytes: 1048576, Errors: []int{401, 403, 404, 409, 422, 503}}, fn)
}
func bearer(header string) string {
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(header, "Bearer ")
}
func workerError(ctx context.Context, e error) error {
	if e == nil {
		return nil
	}
	var pe *runs.Error
	if errors.As(e, &pe) {
		status := 409
		if pe.Code == "UNAVAILABLE" || pe.Code == "CLOCK_UNAVAILABLE" {
			status = 503
		}
		if pe.Code == "INVALID_REQUEST" {
			status = 422
		}
		p := problem(ctx, status, "The worker request cannot proceed.")
		p.Code = pe.Code
		p.ExpectedSeq = pe.ExpectedSeq
		return p
	}
	return fail(ctx, e)
}

type AdmitInput struct {
	Authorization string `header:"Authorization"`
	Body          runs.Admission
}
type AdmitOutput struct {
	Body struct {
		Generation int64 `json:"generation"`
		IdleLease  int   `json:"idle_lease_seconds"`
		Heartbeat  int   `json:"heartbeat_seconds"`
	}
}
type PollInput struct {
	Authorization string `header:"Authorization"`
	Body          runs.Poll
}
type PollOutput struct {
	Body struct {
		Assignment *runs.Assignment `json:"assignment"`
	}
}
type HeartbeatInput struct {
	Authorization string `header:"Authorization"`
	Body          struct {
		runs.Identity
		RequestID string `json:"request_id"`
		AttemptID string `json:"attempt_id,omitempty"`
		Ordinal   int    `json:"current_command_ordinal,omitempty"`
		Tick      int64  `json:"agent_tick"`
		Phase     string `json:"phase"`
	}
}
type HeartbeatOutput struct {
	Body struct {
		Contact  time.Time      `json:"contact_received_at"`
		Controls []runs.Control `json:"controls"`
		Paused   bool           `json:"paused"`
	}
}
type TransitionInput struct {
	Authorization string `header:"Authorization"`
	ID            string `path:"id" format:"uuid"`
	Body          runs.Message
}

// Explicit optional fields survive Huma's schema-link response transformer,
// which rebuilds top-level structs and does not preserve MarshalJSON methods.
type transitionBody struct {
	Accepted           bool       `json:"accepted"`
	NextSeq            int64      `json:"next_seq"`
	AttemptState       string     `json:"attempt_state"`
	OperationStatus    string     `json:"operation_status"`
	Revision           int64      `json:"revision"`
	Ready              bool       `json:"ready"`
	NextCommandOrdinal int        `json:"next_command_ordinal"`
	PermissionID       string     `json:"permission_id,omitempty"`
	Ordinal            int64      `json:"ordinal,omitempty"`
	Digest             string     `json:"command_spec_digest,omitempty"`
	Platform           string     `json:"platform_instance,omitempty"`
	RemainingMS        int64      `json:"remaining_ms,omitempty"`
	Expires            *time.Time `json:"display_expires_at,omitempty"`
}
type TransitionOutput struct{ Body transitionBody }

func transitionResponse(t runs.Transition) *TransitionOutput {
	out := &TransitionOutput{Body: transitionBody{Accepted: t.Accepted, NextSeq: t.NextSeq, AttemptState: t.AttemptState, OperationStatus: t.OperationStatus, Revision: t.Revision, Ready: t.Ready, NextCommandOrdinal: t.NextCommandOrdinal}}
	if p := t.Permission; p != nil {
		out.Body.PermissionID = p.ID
		out.Body.Ordinal = p.Ordinal
		out.Body.Digest = p.Digest
		out.Body.Platform = p.Platform
		out.Body.RemainingMS = p.RemainingMS
		out.Body.Expires = &p.Expires
	}
	return out
}

type SourceCredentialInput struct {
	Authorization string `header:"Authorization"`
	ID            string `path:"id" format:"uuid"`
	Body          struct {
		runs.Identity
		RequestID    string `json:"request_id"`
		RepositoryID string `json:"repository_id"`
		URL          string `json:"canonical_https_url"`
	}
}
type SourceCredentialOutput struct {
	Body struct {
		Username string `json:"username"`
		Token    string `json:"token"`
		Revision int64  `json:"credential_revision"`
	}
}

func (a *Server) registerWorker(api huma.API) {
	a.registerExecutionInputs(api)
	workerRegister(api, "/attempts/{id}/source-credential", "worker-source-credential", func(ctx context.Context, in *SourceCredentialInput) (*SourceCredentialOutput, error) {
		out := &SourceCredentialOutput{}
		e := a.Coordinator.WithAttempt(ctx, bearer(in.Authorization), in.Body.Identity, in.ID, func(tx pgx.Tx, w coordinator.Worker, assignment runs.Assignment) error {
			allowed := false
			for _, g := range assignment.Spec.Grants {
				if g.Kind == "repository" && g.ResourceID == in.Body.RepositoryID {
					allowed = true
				}
			}
			if !allowed {
				return store.ErrForbidden
			}
			var exists bool
			e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.repositories WHERE id=$1 AND url=$2 AND enabled)`, in.Body.RepositoryID, in.Body.URL).Scan(&exists)
			if e != nil {
				return e
			}
			if !exists {
				return store.ErrForbidden
			}
			for _, ref := range assignment.Spec.SourceCredentials {
				if ref.RepositoryID != in.Body.RepositoryID {
					continue
				}
				var ciphertext []byte
				e = tx.QueryRow(ctx, `SELECT encrypted_value FROM cosmoformer.source_credentials WHERE id=$1 AND repository_id=$2 AND semantic_revision=$3 AND enabled`, ref.ID, ref.RepositoryID, ref.Revision).Scan(&ciphertext)
				if e != nil {
					return store.ErrForbidden
				}
				if a.Cipher == nil {
					return store.ErrForbidden
				}
				plain, e := a.Cipher.Open(ciphertext, cryptostore.AAD(w.OrgID, ref.ID, "source_credential", uint64(ref.Revision)))
				if e != nil {
					return e
				}
				if json.Unmarshal(plain, &out.Body) != nil {
					return store.ErrForbidden
				}
				out.Body.Revision = ref.Revision
				return nil
			}
			return nil
		})
		return out, workerError(ctx, e)
	})
	workerRegister(api, "/admit", "worker-admit", func(ctx context.Context, in *AdmitInput) (*AdmitOutput, error) {
		out := &AdmitOutput{}
		n, e := a.Coordinator.Admit(ctx, bearer(in.Authorization), in.Body)
		out.Body.Generation = n
		out.Body.IdleLease = 30
		out.Body.Heartbeat = 5
		return out, workerError(ctx, e)
	})
	workerRegister(api, "/poll", "worker-poll", func(ctx context.Context, in *PollInput) (*PollOutput, error) {
		out := &PollOutput{}
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		for {
			v, e := a.Coordinator.Poll(ctx, bearer(in.Authorization), in.Body)
			if e != nil {
				return nil, workerError(ctx, e)
			}
			if v != nil {
				out.Body.Assignment = v
				return out, nil
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fail(ctx, store.ErrConflict)
			case <-deadline.C:
				timer.Stop()
				return out, nil
			case <-timer.C:
			}
		}
	})
	workerRegister(api, "/heartbeat", "worker-heartbeat", func(ctx context.Context, in *HeartbeatInput) (*HeartbeatOutput, error) {
		if in.Body.RequestID == "" {
			return nil, problem(ctx, 422, "A request ID is required.")
		}
		controls, paused, e := a.Coordinator.Controls(ctx, bearer(in.Authorization), in.Body.Identity)
		out := &HeartbeatOutput{}
		out.Body.Contact = time.Now().UTC()
		out.Body.Controls = controls
		out.Body.Paused = paused
		return out, workerError(ctx, e)
	})
	a.registerOrderedRoutes(api)
}
