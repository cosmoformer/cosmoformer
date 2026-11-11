package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestLostObservedControlResponseResumesOrderedReports(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "uncommitted"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
			attempt := runs.ID()
			next := int64(1)
			receipts := map[string]runs.Transition{}
			var observed []byte
			lost, delivered, observedEffects, results := 0, 0, 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var m runs.Message
				if e := json.NewDecoder(r.Body).Decode(&m); e != nil {
					t.Error(e)
					return
				}
				if m.Identity != id || m.AttemptID != attempt {
					t.Error("changed message identity")
				}
				var ack runs.ControlAck
				isControl := strings.HasSuffix(r.URL.Path, "/controls/ack")
				if isControl {
					json.Unmarshal(m.Payload, &ack)
				}
				isObserved := isControl && ack.State == "observed"
				if isObserved {
					raw, _ := json.Marshal(m)
					if observed == nil {
						observed = raw
					} else if !bytes.Equal(observed, raw) {
						t.Error("observed retry changed immutable message")
					}
					if lost < 2 {
						lost++
						if !committed || lost == 2 {
							w.WriteHeader(503)
							return
						}
					}
				}
				response, ok := receipts[m.MessageID]
				if !ok {
					if m.Seq != next {
						t.Errorf("sequence %d, want %d", m.Seq, next)
						w.WriteHeader(409)
						return
					}
					next++
					response = runs.Transition{Accepted: true, NextSeq: next}
					receipts[m.MessageID] = response
					if isControl {
						if ack.State == "delivered" {
							delivered++
						} else {
							observedEffects++
						}
					} else {
						results++
					}
				}
				if isObserved && lost == 1 {
					w.WriteHeader(503)
					return
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			a := &Agent{HTTP: server.Client(), Origin: server.URL, Identity: id}
			last := runs.Transition{Accepted: true, NextSeq: 99}
			send := a.orderedSender(context.Background(), attempt, &last)
			a.active = newSupervisor(id, attempt)
			a.controlSend = send
			c := runs.Control{ID: runs.ID(), Attempt: attempt, Worker: id.WorkerID, Generation: id.Generation, Agent: id.Agent, Kind: "cancel"}
			a.deliverControls([]runs.Control{c})
			if lost != 2 || last.NextSeq != 99 {
				t.Fatal("fixture did not leave an ambiguous observed receipt", lost, last)
			}
			// A committed observation disappears from subsequent heartbeats. Otherwise
			// the heartbeat repeats the control and must resume observed, not delivered.
			if !committed {
				a.deliverControls([]runs.Control{c})
			}
			if e := send("result", runs.Result{Outcome: "cancelled", NoStateChild: true, Reaped: true}); e != nil {
				t.Fatal("terminal report blocked by lost control reply", e)
			}
			if delivered != 1 || observedEffects != 1 || results != 1 || last.NextSeq != 4 {
				t.Fatal("duplicate effect or lost terminal report", delivered, observedEffects, results, last)
			}
		})
	}
}

func TestOrderedSenderRetainsAmbiguousLaunchForItsCaller(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(503) }))
	defer server.Close()
	a := &Agent{HTTP: server.Client(), Origin: server.URL}
	var last runs.Transition
	send := a.orderedSender(context.Background(), runs.ID(), &last)
	if send("start-permission", map[string]any{"ordinal": 1}) == nil {
		t.Fatal("lost launch reply accepted")
	}
	if send("result", runs.Result{}) == nil || calls != 2 {
		t.Fatal("another transition consumed an unresolved launch response", calls)
	}
}
