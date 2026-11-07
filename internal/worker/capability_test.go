package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestAgentCredentialServerRejectsForeignCapabilities(t *testing.T) {
	var requests atomic.Int64
	var unsafe atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		token := "fictional-repository-token"
		if unsafe.Load() {
			token = "invalid\ncredential"
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "x-token-auth", "token": token, "credential_revision": 1})
	}))
	defer server.Close()
	dir, e := os.MkdirTemp("", "cf-cap-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	identity := runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}
	a := Agent{HTTP: server.Client(), Origin: server.URL, Token: "fictional-platform-only", Identity: identity}
	assignment := runs.Assignment{AttemptID: runs.ID()}
	url := "https://bitbucket.org/fictional/repo.git"
	file, stop, e := a.serveCredentials(context.Background(), dir, assignment, map[string]string{url: runs.ID()})
	if e != nil {
		t.Fatal(e)
	}
	defer stop()
	for _, bad := range []helperRequest{
		{AttemptID: runs.ID(), Identity: identity, URL: url},
		{AttemptID: assignment.AttemptID, Identity: runs.Identity{WorkerID: identity.WorkerID, Generation: 2, Agent: identity.Agent}, URL: url},
		{AttemptID: assignment.AttemptID, Identity: identity, URL: "https://bitbucket.org/fictional/other.git"},
	} {
		conn, e := net.DialTimeout("unix", filepath.Join(dir, "git.sock"), time.Second)
		if e != nil {
			t.Fatal(e)
		}
		conn.SetDeadline(time.Now().Add(time.Second))
		if e := json.NewEncoder(conn).Encode(bad); e != nil {
			t.Fatal(e)
		}
		raw, e := io.ReadAll(conn)
		conn.Close()
		if e != nil || len(raw) != 0 {
			t.Fatal("foreign capability received response", e)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("foreign capability reached credential endpoint")
	}
	input := "protocol=https\nhost=bitbucket.org\npath=fictional/repo.git\n\n"
	var out bytes.Buffer
	if e := CredentialHelper("get", file, strings.NewReader(input), &out); e != nil || out.String() != "username=x-token-auth\npassword=fictional-repository-token\n\n" {
		t.Fatal("valid capability did not resolve", e)
	}
	a.mu.Lock()
	registered := len(a.terms)
	a.mu.Unlock()
	if requests.Load() != 1 || registered != 2 {
		t.Fatal("credential not registered for redaction")
	}
	unsafe.Store(true)
	out.Reset()
	if e := CredentialHelper("get", file, strings.NewReader(input), &out); e == nil || out.Len() != 0 {
		t.Fatal("line injection reached Git")
	}
	stop()
	if _, e := os.Stat(file); !os.IsNotExist(e) {
		t.Fatal("stopped helper retained context file")
	}
}

func TestAgentRunMaintainsHeartbeatWhilePollingAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	heartbeat := make(chan struct{}, 1)
	workerID, agentID := runs.ID(), runs.ID()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/admit":
			json.NewEncoder(w).Encode(map[string]any{"generation": 1, "idle_lease_seconds": 30, "heartbeat_seconds": 5})
		case "/worker/v1/poll":
			io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-ctx.Done():
			}
		case "/worker/v1/heartbeat":
			var body struct {
				runs.Identity
				Phase     string `json:"phase"`
				RequestID string `json:"request_id"`
				Tick      int64  `json:"agent_tick"`
			}
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil || body.WorkerID != workerID || body.Agent != agentID || body.Generation != 1 || body.Phase != "preflight" || body.Tick != 1000 || body.RequestID == "" {
				t.Error("heartbeat lost admitted identity or elapsed tick")
			}
			json.NewEncoder(w).Encode(map[string]any{"contact_received_at": time.Now(), "controls": []string{}, "paused": false})
			heartbeat <- struct{}{}
			cancel()
		default:
			t.Error("unexpected worker route")
		}
	}))
	defer server.Close()
	a := Agent{HTTP: server.Client(), Origin: server.URL, Clock: agentClock{tick: time.Second}, AttemptDir: t.TempDir(), Admission: runs.Admission{WorkerID: workerID, Agent: agentID}}
	a.Git.Run = RunGit
	if e := a.Run(ctx); e == nil {
		t.Fatal("cancelled worker loop continued")
	}
	select {
	case <-heartbeat:
	case <-time.After(time.Second):
		t.Fatal("long poll suppressed heartbeat")
	}
}
