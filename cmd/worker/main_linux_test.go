//go:build linux

package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

type localWorkerTransport func(*http.Request) (*http.Response, error)

func (f localWorkerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestWorkerStartupRequiresProtectedIdentityAndApprovedRuntime(t *testing.T) {
	if _, e := os.Stat("/usr/local/share/cosmoformer/runtime.json"); e != nil {
		if os.Getenv("COSMOFORMER_TEST_SOURCE") == "1" {
			t.Fatal(e)
		}
		t.Skip("requires baked Linux runtime")
	}
	old := os.Args
	os.Args = []string{"worker"}
	t.Cleanup(func() { os.Args = old })
	token := filepath.Join(t.TempDir(), "worker.json")
	t.Setenv("COSMOFORMER_TOKEN_FILE", token)
	if e := run(); e == nil {
		t.Fatal("missing identity accepted")
	}
	if e := os.WriteFile(token, []byte(`{"worker_id":"invalid","token":"synthetic"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := run(); e == nil || !strings.Contains(e.Error(), "identity") {
		t.Fatal("invalid identity accepted", e)
	}
	if e := os.WriteFile(token, []byte(`{"worker_id":"`+runs.ID()+`","token":"synthetic-only"}`), 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("COSMOFORMER_RUNTIME_ID", "")
	t.Setenv("COSMOFORMER_IMAGE_DIGEST", "")
	if e := run(); e == nil || !strings.Contains(e.Error(), "runtime ID") {
		t.Fatal("runtime approval missing", e)
	}
	t.Setenv("COSMOFORMER_RUNTIME_ID", runs.ID())
	t.Setenv("COSMOFORMER_IMAGE_DIGEST", "sha256:"+strings.Repeat("a", 64))
	t.Setenv("COSMOFORMER_ATTEMPT_DIR", t.TempDir())
	t.Setenv("COSMOFORMER_PLATFORM_ORIGIN", "https://platform.example.invalid")
	t.Setenv("COSMOFORMER_EXPECTED_BOOTSTRAP_PRINCIPAL", "")
	if e := run(); e == nil || !strings.Contains(e.Error(), "EXPECTED_BOOTSTRAP_PRINCIPAL") {
		t.Fatal("bootstrap identity prerequisite missing", e)
	}
	t.Setenv("COSMOFORMER_EXPECTED_BOOTSTRAP_PRINCIPAL", "arn:aws:iam::111122223333:role/synthetic-bootstrap")
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	calls := 0
	http.DefaultTransport = localWorkerTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "platform.example.invalid" || r.URL.Path != "/worker/v1/admit" || r.Header.Get("Authorization") != "Bearer synthetic-only" {
			t.Error("unexpected startup request")
		}
		return &http.Response{StatusCode: 409, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"WORKER_PAUSED"}`)), Request: r}, nil
	})
	if e := run(); e == nil || !strings.Contains(e.Error(), "WORKER_PAUSED") || calls != 1 {
		t.Fatal("rejected admission did not stop worker", calls, e)
	}
}
