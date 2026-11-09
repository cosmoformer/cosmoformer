package worker

import (
	"context"
	"encoding/json"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestCaptureSeparatesProtectedOutput(t *testing.T) {
	for _, kind := range []string{"console", "raw-json", "credential-helper"} {
		r := &evidence.Registry{}
		r.Register("secret")
		c := NewLogCapture(r)
		if e := c.Drain(strings.NewReader("secret stdout"), strings.NewReader("secret stderr"), kind); e != nil {
			t.Fatal(e)
		}
		var text string
		for _, s := range c.Queue.Take() {
			if s.Text != nil {
				text += *s.Text
			}
		}
		if strings.Contains(text, "secret") || kind != "console" && strings.Contains(text, "stdout") || kind == "credential-helper" && text != "" {
			t.Fatal(kind, text)
		}
	}
}

type logTransport func(*http.Request) (*http.Response, error)

func (fn logTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }
func reply(status int, body any) *http.Response {
	raw, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}
func TestFlushRecoversOnlyExplicitlyIncompleteUpload(t *testing.T) {
	for _, code := range []string{"UPLOAD_INCOMPLETE", "CONFLICT"} {
		t.Run(code, func(t *testing.T) {
			var mu sync.Mutex
			var ids []string
			var payloads []string
			puts, finals := 0, 0
			artifact := runs.ID()
			a := &Agent{Origin: "https://fixture.invalid", Token: "synthetic", Identity: runs.Identity{WorkerID: runs.ID(), Generation: 1, Agent: runs.ID()}, HTTP: &http.Client{Transport: logTransport(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				raw, _ := io.ReadAll(r.Body)
				if r.Method == "PUT" {
					puts++
					ids = append(ids, r.URL.Path)
					payloads = append(payloads, string(raw))
					if puts == 1 {
						return reply(503, map[string]any{"code": "UNAVAILABLE"}), nil
					}
					if puts == 2 || code == "CONFLICT" {
						return reply(409, map[string]any{"code": code}), nil
					}
					return reply(200, map[string]any{"artifact_id": artifact, "upload_id": r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "received_bytes": len(raw), "sha256": r.Header.Get("Expected-SHA256")}), nil
				}
				finals++
				var request struct {
					Hash string `json:"sha256"`
					Size int64  `json:"size"`
				}
				json.Unmarshal(raw, &request)
				return reply(200, map[string]any{"artifact_id": artifact, "status": "finalized", "version_id": "retained", "sha256": request.Hash, "size": request.Size}), nil
			})}}
			c := NewLogCapture(&evidence.Registry{})
			c.Queue.Append([]byte("first"))
			attempt := runs.ID()
			if c.Flush(context.Background(), a, attempt) == nil {
				t.Fatal("first upload should fail")
			}
			c.Queue.Append([]byte("second"))
			err := c.Flush(context.Background(), a, attempt)
			if err == nil {
				t.Fatal("conflict should fail")
			}
			if ids[0] != ids[1] || payloads[0] != payloads[1] {
				t.Fatal("ambiguous response changed upload identity")
			}
			err = c.Flush(context.Background(), a, attempt)
			if code == "CONFLICT" {
				if err == nil || ids[2] != ids[0] || finals != 0 {
					t.Fatal("generic conflict changed immutable identity")
				}
				return
			}
			if err != nil || ids[2] == ids[0] || payloads[2] != payloads[0] || finals != 2 {
				t.Fatal("incomplete upload blocked subsequent logs", err, ids, finals)
			}
			if _, ok := c.Queue.Peek(); ok {
				t.Fatal("delivered queue not acknowledged")
			}
		})
	}
}
func TestFlushReplaysFinalizationAfterResponseLoss(t *testing.T) {
	var ids []string
	finals := 0
	artifact := runs.ID()
	a := &Agent{Origin: "https://fixture.invalid", HTTP: &http.Client{Transport: logTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if r.Method == "PUT" {
			ids = append(ids, r.URL.Path)
			return reply(200, map[string]any{"artifact_id": artifact, "upload_id": r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "received_bytes": len(raw), "sha256": r.Header.Get("Expected-SHA256")}), nil
		}
		finals++
		if finals == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		var v struct {
			Hash string `json:"sha256"`
			Size int64  `json:"size"`
		}
		json.Unmarshal(raw, &v)
		return reply(200, map[string]any{"artifact_id": artifact, "status": "finalized", "version_id": "retained", "sha256": v.Hash, "size": v.Size}), nil
	})}}
	c := NewLogCapture(&evidence.Registry{})
	c.Queue.Append([]byte("once"))
	attempt := runs.ID()
	if c.Flush(context.Background(), a, attempt) == nil {
		t.Fatal("lost response accepted")
	}
	if e := c.Flush(context.Background(), a, attempt); e != nil {
		t.Fatal(e)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatal("finalization replay changed upload ID")
	}
}
func TestCaptureIndependentUnicodeStreams(t *testing.T) {
	r := &evidence.Registry{}
	r.Register("xx")
	c := NewLogCapture(r)
	if e := c.Drain(strings.NewReader("€stdout"), strings.NewReader("😀stderr"), "console"); e != nil {
		t.Fatal(e)
	}
	var got string
	for _, s := range c.Queue.Take() {
		got += *s.Text
	}
	if !strings.Contains(got, "€") || !strings.Contains(got, "😀") {
		t.Fatal("per-stream Unicode lost", got)
	}
}

func TestAttemptLogCapturesShareCredentialRegistry(t *testing.T) {
	a := &Agent{}
	first, second := a.Logs(), a.Logs()
	if first.Registry != second.Registry {
		t.Fatal("attempt credential registry was replaced")
	}
	if e := first.Registry.Register("synthetic-sensitive"); e != nil {
		t.Fatal(e)
	}
	if e := second.Drain(strings.NewReader("synthetic-sensitive output"), strings.NewReader(""), "console"); e != nil {
		t.Fatal(e)
	}
	var text string
	for _, s := range second.Queue.Take() {
		if s.Text != nil {
			text += *s.Text
		}
	}
	if strings.Contains(text, "synthetic-sensitive") || !strings.Contains(text, "output") {
		t.Fatal("shared credential was not redacted", text)
	}
}

func TestPreviewUsesUnorderedAuthenticatedEndpoint(t *testing.T) {
	attempt := runs.ID()
	id := runs.Identity{WorkerID: runs.ID(), Generation: 7, Agent: runs.ID()}
	text := "safe console"
	calls := 0
	a := &Agent{Origin: "https://fixture.invalid", Token: "synthetic", Identity: id, HTTP: &http.Client{Transport: logTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/worker/v1/attempts/"+attempt+"/log-preview" || r.Header.Get("Authorization") != "Bearer synthetic" || r.Header.Get("X-Worker-Generation") != "7" {
			t.Error("preview identity", r.Method, r.URL.Path, r.Header)
		}
		var p map[string]any
		if e := json.NewDecoder(r.Body).Decode(&p); e != nil {
			t.Fatal(e)
		}
		if p["stream"] != "console" || p["epoch"] != "preview-epoch" || p["text"] != text || p["sequence"] != float64(0) {
			t.Error("preview payload", p)
		}
		if _, ok := p["message_id"]; ok {
			t.Error("preview consumed ordered message identity")
		}
		return reply(200, map[string]any{}), nil
	})}}
	segment := evidence.Segment{Sequence: 0, Start: 0, End: int64(len(text)), Text: &text}
	if e := a.Preview(context.Background(), attempt, "preview-epoch", segment); e != nil {
		t.Fatal(e)
	}
	if a.Preview(context.Background(), "invalid", "", segment) == nil {
		t.Fatal("invalid attempt accepted")
	}
	segment.End++
	if a.Preview(context.Background(), attempt, "", segment) == nil {
		t.Fatal("invalid segment accepted")
	}
	if calls != 1 {
		t.Fatal("invalid preview reached transport", calls)
	}
}

func TestPublisherCancellationRetainsUnacknowledgedSegment(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := NewLogCapture(&evidence.Registry{})
	capture.Queue.Append([]byte("keep on failed send"))
	a := &Agent{Origin: "https://fixture.invalid", HTTP: &http.Client{Transport: logTransport(func(*http.Request) (*http.Response, error) { cancel(); return nil, io.ErrUnexpectedEOF })}}
	if e := capture.Publish(ctx, a, runs.ID()); e != context.Canceled {
		t.Fatal(e)
	}
	if _, ok := capture.Queue.Peek(); !ok {
		t.Fatal("cancelled send acknowledged undelivered logs")
	}
	if e := capture.Publish(ctx, a, runs.ID()); e != context.Canceled {
		t.Fatal(e)
	}
}
