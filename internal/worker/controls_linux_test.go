//go:build linux

package worker

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func TestControlledProcessFixture(t *testing.T) {
	if os.Getenv("COSMOFORMER_CONTROL_CHILD") != "1" {
		return
	}
	events := make(chan os.Signal, 8)
	signal.Notify(events, os.Interrupt)
	file := os.Getenv("COSMOFORMER_CONTROL_FILE")
	if os.WriteFile(file, []byte("ready\n"), 0600) != nil {
		os.Exit(3)
	}
	for range events {
		f, e := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			os.Exit(4)
		}
		f.WriteString("interrupt\n")
		f.Close()
	}
	os.Exit(0)
}
func TestRealControlsInterruptOnceForceExactGroupAndRetainDuringOutage(t *testing.T) {
	id := runs.Identity{WorkerID: runs.ID(), Generation: 3, Agent: runs.ID()}
	attempt := runs.ID()
	s := newSupervisor(id, attempt)
	ctx := withSupervisor(context.Background(), s)
	file := filepath.Join(t.TempDir(), "events")
	exe, _ := os.Executable()
	spawned := make(chan runs.CommandEvent, 1)
	type result struct {
		stop runs.CommandEvent
		err  error
	}
	done := make(chan result, 1)
	go func() {
		stop, e := runNative(ctx, NativeCommand{Start: func(fn func() error) error { return fn() }, Program: exe, Args: []string{"-test.run=^TestControlledProcessFixture$"}, Env: []string{"COSMOFORMER_CONTROL_CHILD=1", "COSMOFORMER_CONTROL_FILE=" + file}, Permission: runs.Permission{ID: runs.ID(), Ordinal: 2}, Logs: NewLogCapture(&evidence.Registry{})}, func(e runs.CommandEvent) error { spawned <- e; return ErrAgent })
		done <- result{stop, e}
	}()
	event := <-spawned
	t.Cleanup(func() { syscall.Kill(-int(*event.PGID), syscall.SIGKILL) })
	until := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(file); e == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child not ready")
		}
		time.Sleep(time.Millisecond)
	}
	// Reporting/contact failure alone must not interrupt an authorized child.
	time.Sleep(30 * time.Millisecond)
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "interrupt") {
		t.Fatal("contact loss signalled child")
	}
	control := runs.Control{ID: runs.ID(), Attempt: attempt, Worker: id.WorkerID, Generation: id.Generation, Agent: id.Agent, Ordinal: 2, Kind: "cancel"}
	ack, e := s.apply(control)
	if e != nil || ack.Signal != "SIGINT" {
		t.Fatal(ack, e)
	}
	replay, e := s.apply(control)
	if e != nil || replay != ack {
		t.Fatal("replayed signal changed", replay, e)
	}
	control.ID = runs.ID()
	again, _ := s.apply(control)
	if again.Signal != "" {
		t.Fatal("second Cancel sent another interrupt")
	}
	until = time.Now().Add(time.Second)
	for {
		raw, _ = os.ReadFile(file)
		if strings.Contains(string(raw), "interrupt") {
			break
		}
		if time.Now().After(until) {
			t.Fatal("interrupt not delivered")
		}
		time.Sleep(time.Millisecond)
	}
	if strings.Count(string(raw), "interrupt") != 1 {
		t.Fatal("repeated SIGINT", string(raw))
	}
	control.ID = runs.ID()
	control.Ordinal = 1
	control.Kind = "force_stop"
	late, _ := s.apply(control)
	if late.Signal != "" || syscall.Kill(int(*event.PID), 0) != nil {
		t.Fatal("late ordinal signalled current child")
	}
	control.ID = runs.ID()
	control.Ordinal = 2
	forced, e := s.apply(control)
	if e != nil || forced.Signal != "SIGKILL" {
		t.Fatal(forced, e)
	}
	select {
	case r := <-done:
		if r.stop.Event != "stopped" || r.stop.Signal == nil || *r.stop.Signal != int(syscall.SIGKILL) || r.stop.GroupEmpty == nil || !*r.stop.GroupEmpty {
			t.Fatal("signal claimed stop without exact proof", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exact killed child not reaped")
	}
	if syscall.Kill(int(*event.PID), 0) != syscall.ESRCH {
		t.Fatal("child not reaped")
	}
	started := false
	if s.start(3, func() error { started = true; return nil }, func(string) bool { return true }) == nil || started {
		t.Fatal("cancellation allowed subsequent launch")
	}
	control.ID = runs.ID()
	last, _ := s.apply(control)
	if last.Signal != "" {
		t.Fatal("completed child signalled")
	}
}
func TestSupervisorLaunchCancelCompletionRaces(t *testing.T) {
	for range 100 {
		id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
		attempt := runs.ID()
		s := newSupervisor(id, attempt)
		launched := make(chan struct{})
		done := make(chan struct{})
		go func() {
			s.start(1, func() error { close(launched); return nil }, func(string) bool { return true })
			close(done)
		}()
		c := runs.Control{ID: runs.ID(), Attempt: attempt, Worker: id.WorkerID, Generation: 1, Agent: id.Agent, Ordinal: 1, Kind: "cancel"}
		s.apply(c)
		<-done
		s.mu.Lock()
		s.signal = nil
		s.mu.Unlock()
		if ack, _ := s.apply(runs.Control{ID: runs.ID(), Attempt: attempt, Worker: id.WorkerID, Generation: 1, Agent: id.Agent, Ordinal: 1, Kind: "force_stop"}); ack.Signal != "" {
			t.Fatal("completed process signalled")
		}
		if s.start(2, func() error { return nil }, func(string) bool { return true }) == nil {
			t.Fatal("cancel lost launch race")
		}
	}
}

func TestReapedLeaderRetiresSignalCapabilityWithoutStopProof(t *testing.T) {
	id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
	attempt := runs.ID()
	s := newSupervisor(id, attempt)
	dir := t.TempDir()
	file := filepath.Join(dir, "survivor.pid")
	exe, _ := os.Executable()
	stop, e := runNative(withSupervisor(context.Background(), s), NativeCommand{Start: func(fn func() error) error { return fn() }, Program: exe, Dir: dir, Args: []string{"-test.run=^TestNativeSupervisorChild$"}, Env: []string{"COSMOFORMER_SUPERVISOR_FIXTURE=survivor", "COSMOFORMER_FIXTURE_PID=" + file}, Permission: runs.Permission{ID: runs.ID(), Ordinal: 1}, Logs: NewLogCapture(&evidence.Registry{})}, func(runs.CommandEvent) error { return nil })
	raw, readErr := os.ReadFile(file)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, parseErr := strconv.Atoi(string(raw))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if e != source.ErrProcess || stop.GroupEmpty == nil || *stop.GroupEmpty {
		t.Fatal("surviving descendant proved stopped", stop, e)
	}
	ack, err := s.apply(runs.Control{ID: runs.ID(), Attempt: attempt, Worker: id.WorkerID, Generation: 1, Agent: id.Agent, Ordinal: 1, Kind: "force_stop"})
	if err != nil || ack.Signal != "" {
		t.Fatal("reaped leader retained a reusable group signal capability", ack, err)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("unproven descendant was automatically killed")
	}
}

func TestLateForceStopAfterDeadlineRetainsExactCompletion(t *testing.T) {
	id := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
	attempt := runs.ID()
	s := newSupervisor(id, attempt)
	ctx, cancel := context.WithCancel(withSupervisor(context.Background(), s))
	defer cancel()
	file := filepath.Join(t.TempDir(), "late-events")
	exe, _ := os.Executable()
	spawned := make(chan runs.CommandEvent, 1)
	type outcome struct {
		stop runs.CommandEvent
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		stop, e := runNative(ctx, NativeCommand{Start: func(fn func() error) error { return fn() }, Program: exe, Args: []string{"-test.run=^TestControlledProcessFixture$"}, Env: []string{"COSMOFORMER_CONTROL_CHILD=1", "COSMOFORMER_CONTROL_FILE=" + file}, Permission: runs.Permission{ID: runs.ID(), Ordinal: 1}, Logs: NewLogCapture(&evidence.Registry{})}, func(e runs.CommandEvent) error { spawned <- e; return nil })
		done <- outcome{stop, e}
	}()
	event := <-spawned
	t.Cleanup(func() { syscall.Kill(-int(*event.PGID), syscall.SIGKILL) })
	until := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(file); e == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("fixture not ready")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case got := <-done:
		t.Fatal("deadline abandoned exact completion", got)
	case <-time.After(2200 * time.Millisecond):
	}
	if syscall.Kill(int(*event.PID), 0) != nil {
		t.Fatal("deadline killed the child")
	}
	raw, _ := os.ReadFile(file)
	if strings.Count(string(raw), "interrupt") != 1 {
		t.Fatal("deadline did not interrupt exactly once", string(raw))
	}
	ack, e := s.apply(runs.Control{ID: runs.ID(), Attempt: attempt, Worker: id.WorkerID, Generation: id.Generation, Agent: id.Agent, Ordinal: 1, Kind: "force_stop"})
	if e != nil || ack.Signal != "SIGKILL" {
		t.Fatal("late explicit force stop not delivered", ack, e)
	}
	select {
	case got := <-done:
		if got.err != nil || got.stop.Event != "stopped" || got.stop.GroupEmpty == nil || !*got.stop.GroupEmpty || got.stop.Pipes != "drained" || got.stop.Signal == nil || *got.stop.Signal != int(syscall.SIGKILL) {
			t.Fatal("late force stop lost positive facts", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late stop evidence not returned")
	}
}
