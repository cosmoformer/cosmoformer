//go:build linux

package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

type boundedEvidence struct {
	file     *os.File
	written  int64
	overflow bool
	cancel   context.CancelFunc
}

func (w *boundedEvidence) Write(b []byte) (int, error) {
	n := len(b)
	if w.written+int64(n) > 67108864 {
		w.overflow = true
		w.cancel()
		return n, nil
	}
	k, e := w.file.Write(b)
	w.written += int64(k)
	return k, e
}
func runNative(ctx context.Context, in NativeCommand, spawn func(runs.CommandEvent) error) (runs.CommandEvent, error) {
	if ctx.Err() != nil {
		return runs.CommandEvent{}, source.ErrProcess
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.Command(in.Program, in.Args...)
	cmd.Dir = in.Dir
	cmd.Env = in.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, wout, e := os.Pipe()
	if e != nil {
		return runs.CommandEvent{}, ErrAgent
	}
	defer stdout.Close()
	stderr, werr, e := os.Pipe()
	if e != nil {
		wout.Close()
		return runs.CommandEvent{}, ErrAgent
	}
	defer stderr.Close()
	cmd.Stdout = wout
	cmd.Stderr = werr
	if in.Start == nil {
		wout.Close()
		werr.Close()
		return runs.CommandEvent{}, source.ErrProcess
	}
	s := supervisor(ctx)
	if e = s.start(in.Permission.Ordinal, func() error { return in.Start(cmd.Start) }, func(kind string) bool { return signalProgram(cmd, kind) }); e != nil {
		wout.Close()
		werr.Close()
		return runs.CommandEvent{}, source.ErrProcess
	}
	wout.Close()
	werr.Close()
	pid := int64(cmd.Process.Pid)
	event := runs.CommandEvent{Ordinal: in.Permission.Ordinal, Permission: in.Permission.ID, Event: "spawned", PID: &pid, PGID: &pid}
	type completion struct {
		err   error
		empty bool
	}
	wait := make(chan completion, 1)
	go func() { e, empty := waitProgram(cmd, s); wait <- completion{e, empty} }()
	var bounded *boundedEvidence
	drained := make(chan error, 1)
	if in.Output != nil {
		bounded = &boundedEvidence{file: in.Output, cancel: cancel}
		go func() {
			var wg sync.WaitGroup
			var first, second error
			wg.Add(2)
			go func() { defer wg.Done(); _, first = io.Copy(bounded, stdout) }()
			go func() { defer wg.Done(); second = in.Logs.Drain(emptyReader{}, stderr, "schema") }()
			wg.Wait()
			drained <- errors.Join(first, second)
		}()
	} else {
		go func() { drained <- in.Logs.Drain(stdout, stderr, "console") }()
	}
	// Reporting cannot delay deadline supervision or cancel an already running
	// command merely because contact was lost. Ordered replay retains its ID.
	reported := make(chan error, 1)
	go func() { reported <- spawn(event) }()

	var completed completion
	select {
	case completed = <-wait:
	case <-ctx.Done():
		s.interrupt()
		// The elapsed cap requests SIGINT, but does not end ownership of a
		// live child. Keep the exact completion consumer so a later explicit
		// Force Stop can still yield truthful stop evidence. Heartbeat delivery
		// remains active; no replacement command or automatic SIGKILL is issued.
		completed = <-wait
	}
	stopped := event
	stopped.Event = "stopped"
	empty := completed.empty
	stopped.GroupEmpty = &empty
	stopped.Pipes = "drained"
	pipeUncertain := false
	select {
	case pipeErr := <-drained:
		if pipeErr != nil {
			stopped.Pipes = "gapped"
		}
	case <-time.After(2 * time.Second):
		pipeUncertain = true
		stdout.Close()
		stderr.Close()
		<-drained
		stopped.Pipes = "gapped"
	}
	reportErr := <-reported
	if reportErr != nil {
		reportErr = spawn(event)
	}
	if cmd.ProcessState == nil {
		return event, source.ErrProcess
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return event, source.ErrProcess
	}
	if status.Signaled() {
		sig := int(status.Signal())
		stopped.Signal = &sig
	} else {
		exit := status.ExitStatus()
		stopped.Exit = &exit
	}
	if !empty || pipeUncertain {
		return stopped, source.ErrProcess
	}
	if bounded != nil && (bounded.overflow || stopped.Pipes != "drained") {
		return stopped, ErrAgent
	}
	if reportErr != nil {
		return stopped, reportErr
	}
	_ = completed.err // exact native exit is in the returned event, including nonzero.
	return stopped, nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
