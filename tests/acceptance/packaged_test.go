//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// This opt-in runs inside the verification image, using the executable copied
// from the production platform stage, not a new go-run/test-server substitute.
func TestPackagedRestartPreservesUncertainOwnership(t *testing.T) {
	if os.Getenv("COSMOFORMER_TEST_PACKAGED") != "1" {
		t.Skip("packaged Linux executable is explicitly selected by make test-package")
	}
	f := newExecutionFixture(t)
	f.prepare(t)
	permission := f.transition(t, "start-permission", f.request(t, 1, "init", ""), 200)
	if permission.Permission == nil {
		t.Fatal("missing permission")
	}
	unknown := runs.ExecutionResult{Outcome: "unknown", Commands: []runs.CommandResult{}, Artifacts: []string{}, PreInitDigest: runs.Digest(f.pre), LogFinal: &runs.LogFinal{Stream: "console", LastSequence: -1, Unknown: true}}
	expectWorkerCode(t, f.workerCall(t, "/attempts/"+f.assignment.AttemptID+"/result", f.message(unknown), 409), "MESSAGE_CONFLICT")
	f.retained(t)
	f.exec(t, `UPDATE cosmoformer.installation SET maintenance=true WHERE id`)
	snapshot := func() string {
		t.Helper()
		var result string
		err := f.db.Owner.QueryRow(context.Background(), `SELECT json_build_array(a.state,o.status,p.id,p.status,(SELECT count(*) FROM cosmoformer.worker_reservations WHERE attempt_id=a.id),(SELECT count(*) FROM cosmoformer.backend_reservations WHERE attempt_id=a.id),(SELECT count(*) FROM cosmoformer.recovery_cases WHERE origin_attempt_id=a.id AND status='open'),(SELECT maintenance FROM cosmoformer.installation WHERE id))::text FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id JOIN cosmoformer.start_permissions p ON p.attempt_id=a.id WHERE a.id=$1`, f.assignment.AttemptID).Scan(&result)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result, `"uncertain"`) || !strings.Contains(result, `"needs_attention"`) {
			t.Fatal("fixture did not retain uncertainty")
		}
		return result
	}
	before := snapshot()
	dir := t.TempDir()
	dsn, key, socket := filepath.Join(dir, "runtime.dsn"), filepath.Join(dir, "encryption.key"), filepath.Join(dir, "api.sock")
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dsn, []byte(strings.Replace(f.db.OwnerDSN, "cosmoformer_owner@", "cosmoformer_app@", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, bytes.Repeat([]byte{42}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	for i := 0; i < 2; i++ {
		cmd := exec.Command("/usr/local/bin/cosmoformer", "serve")
		cmd.Env = []string{"COSMOFORMER_ORIGIN=" + origin, "COSMOFORMER_DATABASE_DSN_FILE=" + dsn, "COSMOFORMER_ENCRYPTION_KEY_FILE=" + key, "COSMOFORMER_UNIX_SOCKET=" + socket, "TMPDIR=/tmp"}
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		finished := false
		t.Cleanup(func() {
			if !finished {
				cmd.Process.Kill()
				<-done
			}
		})
		ready := false
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			response, err := client.Get("http://localhost/health/ready")
			if err == nil {
				response.Body.Close()
				if response.StatusCode == 200 {
					ready = true
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !ready {
			t.Fatal("packaged executable not ready")
		}
		if snapshot() != before {
			t.Fatal("startup changed unresolved ownership")
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			finished = true
			if err != nil {
				t.Fatal("packaged shutdown failed", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("packaged shutdown exceeded allowance")
		}
		if snapshot() != before {
			t.Fatal("shutdown cleared uncertainty or replayed execution")
		}
		f.retained(t)
		transport.CloseIdleConnections()
	}
}
