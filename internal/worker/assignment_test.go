package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestUntrustedAssignmentsCannotStartSourceProcesses(t *testing.T) {
	for _, scenario := range []string{"idle", "expired", "retained sequence", "wrong phase", "wrong runtime", "wrong version", "changed digest"} {
		t.Run(scenario, func(t *testing.T) {
			spec := runs.PreflightSpec{Version: 1, Phase: "preflight", Engine: runs.Runtime{ID: "fixture-runtime"}}
			raw, _ := catalog.Canonical(spec)
			assignment := &runs.Assignment{AttemptID: runs.ID(), Phase: "preflight", RemainingMS: 300000, NextSeq: 1, Spec: spec, Digest: hash(raw)}
			switch scenario {
			case "idle":
				assignment = nil
			case "expired":
				assignment.RemainingMS = 100
			case "retained sequence":
				assignment.NextSeq = 2
			case "wrong phase":
				assignment.Phase = "execution"
			case "wrong runtime":
				assignment.Spec.Engine.ID = "other"
			case "wrong version":
				assignment.Spec.Version = 2
			case "changed digest":
				assignment.Digest = strings.Repeat("0", 64)
			}
			// Keep the digest valid when testing a different protocol guard. Otherwise
			// digest rejection would conceal a missing runtime/version check.
			if assignment != nil && scenario != "changed digest" {
				raw, e := catalog.Canonical(assignment.Spec)
				if e != nil {
					t.Fatal(e)
				}
				assignment.Digest = hash(raw)
			}
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.URL.Path != "/worker/v1/poll" {
					t.Error("unsafe assignment was acknowledged")
				}
				json.NewEncoder(w).Encode(map[string]any{"assignment": assignment})
			}))
			defer server.Close()
			dir := t.TempDir()
			a := Agent{HTTP: server.Client(), Origin: server.URL, AttemptDir: dir, Clock: agentClock{tick: time.Second}, Admission: runs.Admission{RuntimeID: "fixture-runtime"}}
			a.Git.Run = func(context.Context, string, []string, []string) ([]byte, error) {
				t.Error("untrusted assignment started source process")
				return nil, nil
			}
			e := a.RunOne(context.Background())
			if scenario == "idle" {
				if e != nil {
					t.Fatal(e)
				}
			} else if e == nil {
				t.Fatal("unsafe assignment accepted")
			}
			entries, e := os.ReadDir(dir)
			if e != nil || len(entries) != 0 || requests != 1 {
				t.Fatal("assignment created files or extra requests", requests, e)
			}
		})
	}
}

func TestWorkerRejectionCodeCannotInjectLogs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		json.NewEncoder(w).Encode(map[string]string{"code": "private\ncredential"})
	}))
	defer server.Close()
	a := Agent{HTTP: server.Client(), Origin: server.URL}
	var out struct{}
	e := a.call(context.Background(), "/poll", nil, &out)
	if e == nil || strings.Contains(e.Error(), "private") || !strings.Contains(e.Error(), "REQUEST_REJECTED") {
		t.Fatal("untrusted problem text reached worker logs", e)
	}
}
