package worker

import (
	"context"
	"sync"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

type supervisorKey struct{}

// processSupervisor is assignment-local. The same mutex covers launch, discard,
// exact reaping/group inspection and signals; a late ordinal never targets a
// replacement command. Cancellation latches across all subsequent commands.
type processSupervisor struct {
	mu                      sync.Mutex
	id                      runs.Identity
	attempt                 string
	ordinal                 int64
	cancelled               bool
	requested               bool
	signal                  func(string) bool
	interruptSent, killSent bool
	controls                map[string]runs.ControlAck
	requests                map[string]runs.Control
}

func newSupervisor(id runs.Identity, attempt string) *processSupervisor {
	return &processSupervisor{id: id, attempt: attempt, controls: map[string]runs.ControlAck{}, requests: map[string]runs.Control{}}
}
func withSupervisor(ctx context.Context, s *processSupervisor) context.Context {
	return context.WithValue(ctx, supervisorKey{}, s)
}
func supervisor(ctx context.Context) *processSupervisor {
	s, _ := ctx.Value(supervisorKey{}).(*processSupervisor)
	if s == nil {
		s = newSupervisor(runs.Identity{}, "")
	}
	return s
}
func (s *processSupervisor) isCancelled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.cancelled }
func (s *processSupervisor) start(ordinal int64, start func() error, signal func(string) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || s.signal != nil {
		return ErrAgent
	}
	if e := start(); e != nil {
		return e
	}
	s.ordinal = ordinal
	s.signal = signal
	s.interruptSent = false
	s.killSent = false
	return nil
}
func (s *processSupervisor) interruptLocked() {
	if s.signal != nil && !s.interruptSent {
		s.interruptSent = true
		s.signal("SIGINT")
	}
}
func (s *processSupervisor) interrupt() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = true
	s.interruptLocked()
}
func (s *processSupervisor) apply(c runs.Control) (runs.ControlAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Attempt != s.attempt || c.Worker != s.id.WorkerID || c.Generation != s.id.Generation || c.Agent != s.id.Agent || c.Ordinal < 0 || c.Kind != "cancel" && c.Kind != "deadline_cancel" && c.Kind != "force_stop" {
		return runs.ControlAck{}, ErrAgent
	}
	if old, ok := s.controls[c.ID]; ok {
		if s.requests[c.ID] != c {
			return runs.ControlAck{}, ErrAgent
		}
		return old, nil
	}
	s.cancelled = true
	s.requested = true
	ack := runs.ControlAck{ID: c.ID, State: "observed", Ordinal: c.Ordinal}
	if c.Ordinal == s.ordinal && s.signal != nil {
		if c.Kind == "force_stop" {
			if !s.killSent {
				s.killSent = true
				if s.signal("SIGKILL") {
					ack.Signal = "SIGKILL"
				}
			}
		} else if !s.interruptSent {
			s.interruptSent = true
			if s.signal("SIGINT") {
				ack.Signal = "SIGINT"
			}
		}
	}
	s.controls[c.ID] = ack
	s.requests[c.ID] = c
	return ack, nil
}

func (a *Agent) deliverControls(controls []runs.Control) {
	for _, c := range controls {
		a.mu.Lock()
		s, send := a.active, a.controlSend
		a.mu.Unlock()
		if s == nil || send == nil {
			continue
		}
		ack, e := s.apply(c)
		if e != nil {
			continue
		}
		delivered := ack
		delivered.State = "delivered"
		if send("controls/ack", delivered) != nil {
			continue
		}
		_ = send("controls/ack", ack)
	}
}

func (s *processSupervisor) wasRequested() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.requested }
