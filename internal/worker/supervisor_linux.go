//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/source"
)

type cappedOutput struct {
	mu       sync.Mutex
	b        []byte
	overflow bool
}

func (b *cappedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.b)+len(p) > 1048576 {
		b.overflow = true
		return len(p), nil
	}
	b.b = append(b.b, p...)
	return len(p), nil
}
func RunGit(ctx context.Context, dir string, args, env []string) ([]byte, error) {
	return runProgram(ctx, "/usr/bin/git", dir, args, env)
}
func runProgram(ctx context.Context, program, dir string, args, env []string) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, source.ErrSource
	}
	cmd := exec.Command(program, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, wout, e := os.Pipe()
	if e != nil {
		return nil, source.ErrSource
	}
	defer stdout.Close()
	stderr, werr, e := os.Pipe()
	if e != nil {
		wout.Close()
		return nil, source.ErrSource
	}
	defer stderr.Close()
	cmd.Stdout = wout
	cmd.Stderr = werr
	s := supervisor(ctx)
	if e = s.start(0, cmd.Start, func(kind string) bool { return signalProgram(cmd, kind) }); e != nil {
		wout.Close()
		werr.Close()
		return nil, source.ErrSource
	}
	wout.Close()
	werr.Close()
	var output cappedOutput
	drained := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); io.Copy(&output, stdout) }()
		go func() { defer wg.Done(); io.Copy(io.Discard, stderr) }()
		wg.Wait()
		close(drained)
	}()
	done := make(chan error, 1)
	go func() {
		e, empty := waitProgram(cmd, s)
		if !empty {
			e = source.ErrProcess
		}
		done <- e
	}()
	select {
	case e = <-done:
	case <-ctx.Done():
		s.interrupt()
		select {
		case e = <-done:
		case <-time.After(2 * time.Second):
			return nil, source.ErrProcess
		}
	}
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		stdout.Close()
		stderr.Close()
		<-drained
		return nil, source.ErrProcess
	}
	if e == source.ErrProcess {
		return nil, e
	}
	if e != nil || output.overflow {
		return nil, source.ErrSource
	}
	return output.b, nil
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
