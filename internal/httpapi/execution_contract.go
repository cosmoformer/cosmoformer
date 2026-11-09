package httpapi

import (
	"context"
	"encoding/json"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/danielgtaylor/huma/v2"
	"reflect"
)

type preparePayload json.RawMessage

func (p *preparePayload) UnmarshalJSON(b []byte) error { *p = append((*p)[:0], b...); return nil }
func (p preparePayload) MarshalJSON() ([]byte, error)  { return p, nil }
func (preparePayload) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{r.Schema(reflect.TypeFor[runs.Prepare](), true, "Prepare"), r.Schema(reflect.TypeFor[runs.ExecutionPrepare](), true, "ExecutionPrepare")}}
}

type resultPayload json.RawMessage

func (p *resultPayload) UnmarshalJSON(b []byte) error { *p = append((*p)[:0], b...); return nil }
func (p resultPayload) MarshalJSON() ([]byte, error)  { return p, nil }
func (resultPayload) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{r.Schema(reflect.TypeFor[runs.Result](), true, "Result"), r.Schema(reflect.TypeFor[runs.ExecutionResult](), true, "ExecutionResult")}}
}

type orderedMessage[P any] struct {
	Protocol  int    `json:"protocol_version" enum:"1"`
	MessageID string `json:"message_id" format:"uuid"`
	runs.Identity
	AttemptID string `json:"attempt_id" format:"uuid"`
	Seq       int64  `json:"seq" minimum:"1" maximum:"9007199254740991"`
	Payload   P      `json:"payload"`
}
type orderedInput[P any] struct {
	Authorization string `header:"Authorization"`
	ID            string `path:"id" format:"uuid"`
	Body          orderedMessage[P]
}

func registerOrdered[P any](a *Server, api huma.API, route, id string) {
	workerRegister(api, "/attempts/{id}/"+route, id, func(ctx context.Context, in *orderedInput[P]) (*TransitionOutput, error) {
		if in.ID != in.Body.AttemptID {
			return nil, problem(ctx, 422, "Attempt identity does not match.")
		}
		p, e := json.Marshal(in.Body.Payload)
		if e != nil {
			return nil, workerError(ctx, runs.Fail("INVALID_REQUEST"))
		}
		m := runs.Message{Protocol: in.Body.Protocol, MessageID: in.Body.MessageID, Identity: in.Body.Identity, AttemptID: in.Body.AttemptID, Seq: in.Body.Seq, Payload: p}
		out, e := a.Coordinator.Transition(ctx, bearer(in.Authorization), route, m)
		if e != nil {
			return nil, workerError(ctx, e)
		}
		return transitionResponse(*out), nil
	})
}

type ackPayload struct {
	Work string `json:"work_item_id" format:"uuid"`
}

func (a *Server) registerOrderedRoutes(api huma.API) {
	registerOrdered[ackPayload](a, api, "ack", "worker-ack")
	registerOrdered[preparePayload](a, api, "prepare-result", "worker-prepare-result")
	registerOrdered[runs.ManifestSeal](a, api, "manifest-seal", "worker-manifest-seal")
	registerOrdered[runs.CommandRequest](a, api, "start-permission", "worker-start-permission")
	registerOrdered[runs.CommandEvent](a, api, "command-events", "worker-command-events")
	registerOrdered[resultPayload](a, api, "result", "worker-result")
	registerOrdered[runs.ControlAck](a, api, "controls/ack", "worker-controls-ack")
}
