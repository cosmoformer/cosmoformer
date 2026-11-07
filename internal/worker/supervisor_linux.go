//go:build linux

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
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
	var output cappedOutput
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if e := cmd.Start(); e != nil {
		return nil, source.ErrSource
	}
	pid := cmd.Process.Pid
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var e error
	select {
	case e = <-done:
	case <-ctx.Done():
		// The exact still-owned direct child receives one graceful interrupt. A
		// deadline never escalates to SIGKILL or claims that a group was stopped.
		select {
		case e = <-done:
		default:
			cmd.Process.Signal(syscall.SIGINT)
			select {
			case e = <-done:
			case <-time.After(2 * time.Second):
				return nil, source.ErrProcess
			}
		}
	}
	if syscall.Kill(-pid, 0) != syscall.ESRCH {
		return nil, source.ErrProcess
	}
	if e != nil || output.overflow {
		return nil, source.ErrSource
	}
	return output.b, nil
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
