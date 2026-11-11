package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// orderedSender shares the immutable sequence between the assignment and its
// heartbeat controls. Only the independent control side channel can be drained
// before a different caller transition; launch callers must retry their exact
// message so its permission response cannot be consumed by another transition.
func (a *Agent) orderedSender(ctx context.Context, attempt string, last *runs.Transition) func(string, any) error {
	var mu sync.Mutex
	seq := int64(1)
	var pending *runs.Message
	var pendingRoute string
	ambiguous := false
	flush := func() error {
		var response runs.Transition
		var err error
		for retry := 0; retry < 2; retry++ {
			err = a.call(ctx, "/attempts/"+attempt+"/"+pendingRoute, *pending, &response)
			if err == nil {
				break
			}
			var rejected *Rejected
			if !errors.As(err, &rejected) || rejected.Status >= 500 {
				ambiguous = true
			}
			if ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			// A later rejection cannot disprove an earlier ambiguous commit.
			if !ambiguous {
				pending = nil
				pendingRoute = ""
			}
			return err
		}
		if !response.Accepted || response.NextSeq != seq+1 {
			ambiguous = true
			return ErrAgent
		}
		seq++
		if pendingRoute != "controls/ack" {
			*last = response
		}
		pending = nil
		pendingRoute = ""
		ambiguous = false
		return nil
	}
	return func(route string, payload any) error {
		mu.Lock()
		defer mu.Unlock()
		raw, e := json.Marshal(payload)
		if e != nil {
			return ErrAgent
		}
		if pending != nil && (pendingRoute != route || !bytes.Equal(pending.Payload, raw)) {
			if pendingRoute != "controls/ack" {
				return ErrAgent
			}
			if e = flush(); e != nil {
				return e
			}
		}
		if pending == nil {
			pending = &runs.Message{Protocol: 1, MessageID: runs.ID(), Identity: a.Identity, AttemptID: attempt, Seq: seq, Payload: raw}
			pendingRoute = route
			ambiguous = false
		}
		return flush()
	}
}
