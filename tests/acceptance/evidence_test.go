//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

type evidenceStorage struct {
	mu      sync.Mutex
	objects map[string]string
}

func (s *evidenceStorage) Check(context.Context) error { return nil }
func (s *evidenceStorage) Put(_ context.Context, k string, r io.Reader, _ int64) (string, error) {
	b, e := io.ReadAll(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[k] = string(b)
	return "retained", e
}
func (s *evidenceStorage) Get(_ context.Context, k, v string) (evidence.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[k]
	if !ok || v != "retained" {
		return evidence.Object{}, evidence.ErrInvalid
	}
	return evidence.Object{Version: v, Size: int64(len(b)), Encryption: "AES256", Body: io.NopCloser(bytes.NewBufferString(b))}, nil
}
func TestEvidenceHTTPWorkerFencesAndBrowserLogs(t *testing.T) {
	f := newPreflightFixture(t)
	if e := f.agent.Admit(context.Background()); e != nil {
		t.Fatal(e)
	}
	raw := f.workerCall(t, "/poll", runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}, 200)
	var polled struct {
		Assignment *runs.Assignment `json:"assignment"`
	}
	json.Unmarshal(raw, &polled)
	a := polled.Assignment
	if a == nil {
		t.Fatal("missing assignment")
	}
	call := func(method, path string, body []byte, hash, kind string, generation int64, status int) []byte {
		t.Helper()
		r, _ := http.NewRequest(method, f.agent.Origin+"/worker/v1/attempts/"+a.AttemptID+path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.token)
		r.Header.Set("X-Worker-ID", f.workerID)
		r.Header.Set("X-Worker-Generation", strconv.FormatInt(generation, 10))
		r.Header.Set("X-Agent-Instance", f.agent.Identity.Agent)
		r.Header.Set("Content-Type", "application/json")
		if method == "PUT" {
			r.Header.Set("Content-Type", "application/octet-stream")
			r.Header.Set("Artifact-Kind", kind)
			r.Header.Set("Expected-SHA256", hash)
		}
		client := *f.agent.HTTP
		client.Transport = f.wire.base
		res, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != status {
			t.Fatalf("%s %s status=%d body=%s", method, path, res.StatusCode, b)
		}
		return b
	}
	text := "redacted console"
	payload, _ := json.Marshal(evidence.Segment{Sequence: 0, Start: 0, End: int64(len(text)), Text: &text})
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	upload := runs.ID()
	path := "/artifacts/" + upload
	call("PUT", path, payload, hash, "log_segment", f.agent.Identity.Generation+1, 409)
	oldWorker := f.workerID
	f.workerID = runs.ID()
	call("PUT", path, payload, hash, "log_segment", f.agent.Identity.Generation, 409)
	f.workerID = oldWorker
	oldAgent := f.agent.Identity.Agent
	f.agent.Identity.Agent = runs.ID()
	call("PUT", path, payload, hash, "log_segment", f.agent.Identity.Generation, 409)
	f.agent.Identity.Agent = oldAgent
	oldAttempt := a.AttemptID
	a.AttemptID = runs.ID()
	call("PUT", path, payload, hash, "log_segment", f.agent.Identity.Generation, 403)
	a.AttemptID = oldAttempt

	call("PUT", path, payload, hash, "manifest", f.agent.Identity.Generation, 403)
	receipt := call("PUT", path, payload, hash, "log_segment", f.agent.Identity.Generation, 200)
	var artifact evidence.Artifact
	json.Unmarshal(receipt, &artifact)
	call("PUT", path, payload, hash, "log_segment", f.agent.Identity.Generation, 200)
	final, _ := json.Marshal(map[string]any{"request_id": runs.ID(), "kind": "log_segment", "sha256": hash, "size": len(payload)})
	call("POST", path+"/finalize", final, "", "", f.agent.Identity.Generation, 200)
	call("POST", path+"/finalize", final, "", "", f.agent.Identity.Generation, 200)
	call("GET", "/artifacts/"+artifact.ID, nil, "", "", f.agent.Identity.Generation, 200)
	gap := evidence.Segment{Sequence: 1, Start: int64(len(text)), End: int64(len(text) + 10), Gap: &evidence.Gap{Reason: "worker_buffer_overflow"}}
	id := runs.ID()
	original := f.agent.HTTP.Transport
	f.agent.HTTP.Transport = f.wire.base
	if e := f.agent.ArchiveSegment(context.Background(), a.AttemptID, id, gap); e != nil {
		t.Fatal(e)
	}
	if e := f.agent.ArchiveSegment(context.Background(), a.AttemptID, id, gap); e != nil {
		t.Fatal("worker replay", e)
	}
	f.agent.HTTP.Transport = original
	var run string
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT run_id::text FROM cosmoformer.operations WHERE id=$1`, a.OperationID).Scan(&run); e != nil {
		t.Fatal(e)
	}
	var before time.Time
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT last_activity_at FROM cosmoformer.sessions WHERE verifier=$1`, identity.Digest(f.admin.token)).Scan(&before); e != nil {
		t.Fatal(e)
	}
	batch := f.admin.call("GET", "/organizations/"+f.org+"/runs/"+run+"/logs?attempt_id="+a.AttemptID, nil, 200)
	var after time.Time
	if e := f.db.Owner.QueryRow(context.Background(), `SELECT last_activity_at FROM cosmoformer.sessions WHERE verifier=$1`, identity.Digest(f.admin.token)).Scan(&after); e != nil || !before.Equal(after) {
		t.Fatal("log read renewed idle session", e)
	}

	if batch["archive_complete"] != false || len(batch["segments"].([]any)) != 2 {
		t.Fatal("untruthful archive", batch)
	}
	// Administrative ownership alone confers no raw grant. This fixture inserts
	// retained metadata for the real preflight attempt; no execution is admitted.
	var rawID string
	e := f.db.Owner.QueryRow(context.Background(), `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,version_id,sha256,byte_count,state,finalized_at) SELECT org_id,attempt_id,'manifest',gen_random_uuid(),bucket,object_key,version_id,sha256,byte_count,'finalized',now() FROM cosmoformer.artifacts WHERE id=$1 RETURNING id::text`, artifact.ID).Scan(&rawID)
	if e != nil {
		t.Fatal(e)
	}
	f.admin.call("GET", "/organizations/"+f.org+"/artifacts/"+rawID+"/content", nil, 403)
	f.admin.call("GET", "/organizations/"+runs.ID()+"/artifacts/"+rawID+"/content", nil, 404)
	f.exec(t, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) SELECT org_id,team_id,project_id,environment_id,'raw_evidence_read' FROM cosmoformer.grants WHERE role='planner' AND revoked_at IS NULL`)
	f.admin.call("GET", "/organizations/"+f.org+"/artifacts/"+rawID+"/content", nil, 200)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=now() WHERE role='raw_evidence_read'`)
	f.admin.call("GET", "/organizations/"+f.org+"/artifacts/"+rawID+"/content", nil, 403)
	f.exec(t, `UPDATE cosmoformer.grants SET revoked_at=now() WHERE role='planner'`)
	f.admin.call("GET", "/organizations/"+f.org+"/runs/"+run+"/logs?attempt_id="+a.AttemptID, nil, 403)

}
func TestLogCompletionReceiptsAndPositiveClose(t *testing.T) {
	for _, scenario := range []string{"complete", "unknown", "omitted"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPreflightFixture(t)
			ctx := context.Background()
			if e := f.agent.Admit(ctx); e != nil {
				t.Fatal(e)
			}
			raw := f.workerCall(t, "/poll", runs.Poll{Identity: f.agent.Identity, RequestID: runs.ID(), Wait: 20}, 200)
			var p struct {
				Assignment *runs.Assignment `json:"assignment"`
			}
			json.Unmarshal(raw, &p)
			a := p.Assignment
			if a == nil {
				t.Fatal("missing assignment")
			}
			text := "masked output"
			original := f.agent.HTTP.Transport
			f.agent.HTTP.Transport = f.wire.base
			if e := f.agent.ArchiveSegment(ctx, a.AttemptID, runs.ID(), evidence.Segment{Sequence: 0, Start: 0, End: int64(len(text)), Text: &text}); e != nil {
				t.Fatal(e)
			}
			f.agent.HTTP.Transport = original
			result := runs.Result{Outcome: "preparation_failed", Reason: "SOURCE_CHECKOUT_FAILED", NoStateChild: true, Reaped: true}
			if scenario != "omitted" {
				result.LogFinal = &runs.LogFinal{Stream: "console", LastSequence: 0, Complete: scenario == "complete", Unknown: scenario == "unknown"}
			}
			payload, _ := json.Marshal(result)
			message := runs.Message{Protocol: 1, Identity: f.agent.Identity, AttemptID: a.AttemptID, MessageID: runs.ID(), Seq: 1, Payload: payload}
			f.workerCall(t, "/attempts/"+a.AttemptID+"/result", message, 200)
			f.workerCall(t, "/attempts/"+a.AttemptID+"/result", message, 200)
			batch := f.admin.call("GET", "/organizations/"+f.org+"/runs/"+a.RunID+"/logs?attempt_id="+a.AttemptID, nil, 200)
			if batch["archive_complete"] != (scenario == "complete") {
				t.Fatal("false completeness", batch)
			}
			var holds int
			if e := f.db.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_id=$1 AND owner_kind='attempt' AND released_at IS NULL`, a.AttemptID).Scan(&holds); e != nil || holds != 0 {
				t.Fatal("positive close did not release holds", e)
			}
			if scenario != "complete" && len(batch["segments"].([]any)) != 2 {
				t.Fatal("missing unknown gap", batch)
			}
		})
	}
}
