package worker

import (
	"context"
	"errors"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"io"
	"sync"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
)

// LogCapture drains each allowed pipe through its own overlap window into one
// bounded console queue. The credential registry is shared before helper reply.
type LogCapture struct {
	delivery sync.Mutex
	upload   string
	lastAck  int64
	gaps     bool
	Registry *evidence.Registry
	Queue    evidence.Queue
	mu       sync.Mutex
}

func NewLogCapture(registry *evidence.Registry) *LogCapture {
	return &LogCapture{Registry: registry, lastAck: -1}
}
func (c *LogCapture) Drain(stdout, stderr io.Reader, stdoutKind string) error {
	var wg sync.WaitGroup
	var first error
	var mu sync.Mutex
	drain := func(r io.Reader, console bool) {
		defer wg.Done()
		var e error
		if console {
			e = evidence.Drain(r, c.Registry.Pipe(), func(b []byte) { c.mu.Lock(); defer c.mu.Unlock(); c.Queue.Append(b) })
		} else {
			_, e = io.Copy(io.Discard, r)
		}
		if e != nil {
			mu.Lock()
			if first == nil {
				first = e
			}
			mu.Unlock()
		}
	}
	// Closed allowlist: show/schema JSON, helper credential stdout, and unknown
	// sources are drained without ever entering ordinary log buffers.
	wg.Add(2)
	go drain(stdout, stdoutKind == "console")
	go drain(stderr, stdoutKind != "credential-helper")
	wg.Wait()
	return first
}

// Logs shares the current attempt's registered credentials. A caller must finish
// its pipes before the attempt is closed and the registry is discarded.
func (a *Agent) Logs() *LogCapture {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.redactor == nil {
		a.redactor = &evidence.Registry{}
	}
	return NewLogCapture(a.redactor)
}

// Publish flushes at the fixed one-second cadence. Failed sends retain their
// segment and upload ID while output keeps draining into the bounded queue.
// On EOF the caller invokes Flush before submitting its final log receipt.
func (c *LogCapture) Publish(ctx context.Context, a *Agent, attempt string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if e := c.Flush(ctx, a, attempt); e != nil && ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
}
func (c *LogCapture) Flush(ctx context.Context, a *Agent, attempt string) error {
	c.delivery.Lock()
	defer c.delivery.Unlock()
	for {
		s, ok := c.Queue.Peek()
		if !ok {
			return nil
		}
		if c.upload == "" {
			c.upload = runs.ID()
		}
		if e := a.ArchiveSegment(ctx, attempt, c.upload, s); e != nil {
			if errors.Is(e, evidence.ErrUploadIncomplete) {
				c.upload = ""
			}
			return e
		}
		if e := c.Queue.Acknowledge(s.Sequence); e != nil {
			return e
		}
		c.lastAck = s.Sequence
		if s.Gap != nil {
			c.gaps = true
		}
		c.upload = ""
	}
}

func (c *LogCapture) Final() *runs.LogFinal {
	c.delivery.Lock()
	defer c.delivery.Unlock()
	return &runs.LogFinal{Stream: "console", LastSequence: c.lastAck, Complete: !c.gaps}
}
