package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// ArchiveSegment retries must retain uploadID and the exact segment. A failed
// call is never acknowledgment of delivery; callers retain the bounded queue or
// send an explicit gap after loss. No raw stdout accepts this entry point.
func (a *Agent) ArchiveSegment(ctx context.Context, attempt, uploadID string, s evidence.Segment) error {
	if !catalog.UUID(attempt) || !catalog.UUID(uploadID) || s.Validate() != nil {
		return ErrAgent
	}
	payload, e := json.Marshal(s)
	if e != nil || len(payload) > int(evidence.SegmentMax) {
		return ErrAgent
	}
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	path := "/worker/v1/attempts/" + attempt + "/artifacts/" + uploadID
	if e = a.evidenceRequest(ctx, "PUT", path, payload, hash); e != nil {
		return e
	}
	final, _ := json.Marshal(map[string]any{"request_id": uploadID, "kind": "log_segment", "sha256": hash, "size": len(payload)})
	return a.evidenceRequest(ctx, "POST", path+"/finalize", final, "")
}
func (a *Agent) evidenceRequest(ctx context.Context, method, path string, body []byte, hash string) error {
	r, e := http.NewRequestWithContext(ctx, method, a.Origin+path, bytes.NewReader(body))
	if e != nil {
		return ErrAgent
	}
	r.Header.Set("Authorization", "Bearer "+a.Token)
	r.Header.Set("X-Worker-ID", a.Identity.WorkerID)
	r.Header.Set("X-Worker-Generation", strconv.FormatInt(a.Identity.Generation, 10))
	r.Header.Set("X-Agent-Instance", a.Identity.Agent)
	r.Header.Set("Content-Type", "application/json")
	if method == "PUT" {
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set("Artifact-Kind", "log_segment")
		r.Header.Set("Expected-SHA256", hash)
	}
	client := *a.HTTP
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(r)
	if e != nil {
		return ErrAgent
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, 1048577))
	if e != nil || len(raw) > 1048576 {
		return ErrAgent
	}
	if method == "PUT" && res.StatusCode == http.StatusConflict {
		var problem struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &problem) == nil && problem.Code == "UPLOAD_INCOMPLETE" {
			return evidence.ErrUploadIncomplete
		}
	}
	if res.StatusCode != 200 {
		return ErrAgent
	}
	if method == "PUT" {
		var v struct {
			ID       string `json:"artifact_id"`
			Upload   string `json:"upload_id"`
			Received int64  `json:"received_bytes"`
			Hash     string `json:"sha256"`
		}
		if json.Unmarshal(raw, &v) != nil || !catalog.UUID(v.ID) || v.Upload != path[strings.LastIndex(path, "/")+1:] || v.Received != int64(len(body)) || v.Hash != hash {
			return ErrAgent
		}
	}
	if strings.HasSuffix(path, "/finalize") {
		var v struct {
			ID      string `json:"artifact_id"`
			Status  string `json:"status"`
			Version string `json:"version_id"`
			Hash    string `json:"sha256"`
			Size    int64  `json:"size"`
		}
		var expected struct {
			Hash string `json:"sha256"`
			Size int64  `json:"size"`
		}
		json.Unmarshal(body, &expected)
		if json.Unmarshal(raw, &v) != nil || !catalog.UUID(v.ID) || v.Status != "finalized" || v.Version == "" || v.Version == "null" || v.Hash != expected.Hash || v.Size != expected.Size {
			return ErrAgent
		}
	}

	return nil
}

// Preview never acknowledges custody and never consumes the ordered W sequence.
func (a *Agent) Preview(ctx context.Context, attempt, epoch string, s evidence.Segment) error {
	if !catalog.UUID(attempt) || s.Validate() != nil {
		return ErrAgent
	}
	text := ""
	if s.Text != nil {
		text = *s.Text
	}
	payload, _ := json.Marshal(map[string]any{"stream": "console", "epoch": epoch, "sequence": s.Sequence, "byte_start": s.Start, "text": text, "gap": s.Gap})
	return a.evidenceRequest(ctx, "POST", "/worker/v1/attempts/"+attempt+"/log-preview", payload, "")
}

// RetainEvidence sends protected bytes through custody and checks both exact
// receipts. It returns only the finalized opaque artifact ID.
func (a *Agent) RetainEvidence(ctx context.Context, attempt, kind string, raw []byte) (string, error) {
	if !catalog.UUID(attempt) || evidence.Limit(kind) == 0 || int64(len(raw)) > evidence.Limit(kind) {
		return "", ErrAgent
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	upload := runs.ID()
	path := "/worker/v1/attempts/" + attempt + "/artifacts/" + upload
	send := func(method, path string, body []byte) ([]byte, error) {
		req, e := http.NewRequestWithContext(ctx, method, a.Origin+path, bytes.NewReader(body))
		if e != nil {
			return nil, ErrAgent
		}
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("X-Worker-ID", a.Identity.WorkerID)
		req.Header.Set("X-Worker-Generation", strconv.FormatInt(a.Identity.Generation, 10))
		req.Header.Set("X-Agent-Instance", a.Identity.Agent)
		req.Header.Set("Content-Type", "application/json")
		if method == "PUT" {
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Artifact-Kind", kind)
			req.Header.Set("Expected-SHA256", hash)
		}
		client := *a.HTTP
		client.Timeout = 30 * time.Second
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		res, e := client.Do(req)
		if e != nil {
			return nil, ErrAgent
		}
		defer res.Body.Close()
		b, e := io.ReadAll(io.LimitReader(res.Body, 1048577))
		if e != nil || len(b) > 1048576 || res.StatusCode != 200 {
			return nil, ErrAgent
		}
		return b, nil
	}
	b, e := send("PUT", path, raw)
	if e != nil {
		return "", e
	}
	var receipt struct {
		ID     string `json:"artifact_id"`
		Upload string `json:"upload_id"`
		Size   int64  `json:"received_bytes"`
		Hash   string `json:"sha256"`
	}
	if json.Unmarshal(b, &receipt) != nil || !catalog.UUID(receipt.ID) || receipt.Upload != upload || receipt.Size != int64(len(raw)) || receipt.Hash != hash {
		return "", ErrAgent
	}
	body, _ := json.Marshal(map[string]any{"request_id": runs.ID(), "kind": kind, "sha256": hash, "size": len(raw)})
	b, e = send("POST", path+"/finalize", body)
	if e != nil {
		return "", e
	}
	var final struct {
		ID      string `json:"artifact_id"`
		Status  string `json:"status"`
		Version string `json:"version_id"`
		Size    int64  `json:"size"`
		Hash    string `json:"sha256"`
	}
	if json.Unmarshal(b, &final) != nil || final.ID != receipt.ID || final.Status != "finalized" || final.Version == "" || final.Version == "null" || final.Hash != hash || final.Size != int64(len(raw)) {
		return "", ErrAgent
	}
	return final.ID, nil
}

// RestoreEvidence accepts only an exact artifact named in this Apply assignment.
// The platform independently checks attempt/operation holds and current authority.
func (a *Agent) RestoreEvidence(ctx context.Context, assignment runs.Assignment, kind string) ([]byte, error) {
	if assignment.Kind != "apply" || assignment.Execution == nil || assignment.Execution.RetainedPlan == nil {
		return nil, ErrAgent
	}
	var ref *runs.RetainedArtifact
	for i, r := range assignment.Execution.RetainedPlan.Records {
		if r.Kind == kind {
			if ref != nil {
				return nil, ErrAgent
			}
			ref = &assignment.Execution.RetainedPlan.Records[i]
		}
	}
	if ref == nil || !catalog.UUID(ref.ID) || !runs.ValidDigest(ref.Hash) || ref.Size < 0 || ref.Size > evidence.Limit(kind) {
		return nil, ErrAgent
	}
	req, e := http.NewRequestWithContext(ctx, "GET", a.Origin+"/worker/v1/attempts/"+assignment.AttemptID+"/artifacts/"+ref.ID, nil)
	if e != nil {
		return nil, ErrAgent
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("X-Worker-ID", a.Identity.WorkerID)
	req.Header.Set("X-Worker-Generation", strconv.FormatInt(a.Identity.Generation, 10))
	req.Header.Set("X-Agent-Instance", a.Identity.Agent)
	client := *a.HTTP
	client.Timeout = 30 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(req)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, ErrAgent
	}
	raw, e := io.ReadAll(io.LimitReader(res.Body, ref.Size+1))
	if e != nil || int64(len(raw)) != ref.Size || hash(raw) != ref.Hash {
		return nil, ErrAgent
	}
	return raw, nil
}
