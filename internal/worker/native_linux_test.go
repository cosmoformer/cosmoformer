//go:build linux

package worker

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
)

func TestNativeSupervisorChild(t *testing.T) {
	mode := os.Getenv("COSMOFORMER_SUPERVISOR_FIXTURE")
	if mode == "" {
		return
	}
	switch mode {
	case "done":
		io.WriteString(os.Stdout, `{"protected":"synthetic-private-json"}`)
		io.WriteString(os.Stderr, "before secret-value-74 after\n")
	case "delay":
		time.Sleep(300 * time.Millisecond)
		io.WriteString(os.Stdout, `{"protected":"synthetic delayed json"}`)
	case "graceful":
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
	case "ignore":
		signal.Ignore(os.Interrupt)
		for {
			time.Sleep(time.Second)
		}
	case "detached", "survivor":
		exe, _ := os.Executable()
		child := exec.Command(exe, "-test.run=^TestNativeSupervisorChild$")
		child.Env = []string{"COSMOFORMER_SUPERVISOR_FIXTURE=ignore"}
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if mode == "detached" {
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		}
		if child.Start() != nil {
			os.Exit(3)
		}
		if os.WriteFile(os.Getenv("COSMOFORMER_FIXTURE_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
			os.Exit(4)
		}
	case "overflow":
		for range 65 {
			os.Stdout.Write(bytes.Repeat([]byte("x"), 1048576))
		}
	}
	os.Exit(0)
}
func TestNativeSupervisorExactStopAndBoundedRawOutput(t *testing.T) {
	for _, mode := range []string{"done", "graceful", "overflow", "detached", "survivor", "delay"} {
		t.Run(mode, func(t *testing.T) {
			executable, e := os.Executable()
			if e != nil {
				t.Fatal(e)
			}
			reg := &evidence.Registry{}
			if e = reg.Register("secret-value-74"); e != nil {
				t.Fatal(e)
			}
			logs := NewLogCapture(reg)
			dir := t.TempDir()
			file, e := os.OpenFile(filepath.Join(dir, "raw"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				t.Fatal(e)
			}
			defer file.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "graceful" {
				timer := time.AfterFunc(300*time.Millisecond, cancel)
				defer timer.Stop()
			}
			var spawned runs.CommandEvent
			reports := 0
			stop, e := runNative(ctx, NativeCommand{Start: func(fn func() error) error { return fn() }, Program: executable, Dir: dir, Args: []string{"-test.run=^TestNativeSupervisorChild$"}, Env: []string{"COSMOFORMER_SUPERVISOR_FIXTURE=" + mode, "COSMOFORMER_FIXTURE_PID=" + filepath.Join(dir, "detached.pid")}, Permission: runs.Permission{ID: runs.ID(), Ordinal: 1}, Output: file, Logs: logs}, func(event runs.CommandEvent) error {
				spawned = event
				reports++
				if mode == "delay" && reports == 1 {
					return ErrAgent
				}
				return nil
			})
			if spawned.PID == nil || spawned.PGID == nil || *spawned.PID != *spawned.PGID {
				t.Fatal("missing exact spawn facts")
			}
			if mode == "detached" || mode == "survivor" {
				raw, readErr := os.ReadFile(filepath.Join(dir, "detached.pid"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				child, parseErr := strconv.Atoi(string(raw))
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				defer syscall.Kill(child, syscall.SIGKILL)
				if e != source.ErrProcess || stop.Pipes != "gapped" {
					t.Fatal("unaccounted inherited writer proved stopped", stop, e)
				}
				return
			}
			if stop.Event != "stopped" || stop.GroupEmpty == nil || !*stop.GroupEmpty || stop.Pipes != "drained" {
				t.Fatal("missing exact stop proof", stop, e)
			}
			if mode == "overflow" {
				if e == nil {
					t.Fatal("raw overflow silently accepted")
				}
				st, _ := file.Stat()
				if st.Size() > 67108864 {
					t.Fatal("unbounded raw evidence")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if mode == "delay" && (reports != 2 || stop.Exit == nil || *stop.Exit != 0 || stop.Signal != nil) {
				t.Fatal("contact loss interrupted permitted command", stop, reports)
			}
			if mode == "done" {
				data, _ := os.ReadFile(file.Name())
				if !bytes.Contains(data, []byte("synthetic-private-json")) {
					t.Fatal("raw stdout not retained")
				}
				segment, ok := logs.Queue.Peek()
				if !ok || segment.Text == nil || bytes.Contains([]byte(*segment.Text), []byte("secret-value-74")) || bytes.Contains([]byte(*segment.Text), []byte("synthetic-private-json")) {
					t.Fatal("raw or known value entered logs")
				}
			}
		})
	}
}
