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
