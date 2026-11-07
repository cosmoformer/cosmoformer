//go:build integration

package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestSignedWebhookRejectsAlteredReplayAndMalformedEvents(t *testing.T) {
	f := newSourceFixture(t)
	f.configure(t)
	delivery := runs.ID()
	body := []byte(`{"repository":{"uuid":"{fictional-repository}"}}`)
	send := func(payload []byte, id, event, hook string, status int) {
		t.Helper()
		hash := hmac.New(sha256.New, []byte(strings.Repeat("w", 32)))
		hash.Write(payload)
		r := httptest.NewRequest("POST", "/webhooks/bitbucket/"+f.Org+"/"+f.Integration, bytes.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Hub-Signature", "sha256="+hex.EncodeToString(hash.Sum(nil)))
		r.Header.Set("X-Hook-UUID", hook)
		r.Header.Set("X-Request-UUID", id)
		r.Header.Set("X-Event-Key", event)
		w := httptest.NewRecorder()
		f.Handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("event %s: got %d want %d: %s", event, w.Code, status, w.Body.String())
		}
	}
	send(body, delivery, "repo:push", f.Hook, 200)
	send(body, delivery, "repo:push", f.Hook, 200)
	send([]byte(`{"repository":{"uuid":"{fictional-repository}"},"fixture":"changed"}`), delivery, "repo:push", f.Hook, 409)
	send(body, "invalid-delivery", "repo:push", f.Hook, 422)
	send(body, runs.ID(), "unsupported:event", f.Hook, 422)
	send(body, runs.ID(), "repo:push", "foreign-hook", 403)
	send([]byte(`{`), runs.ID(), "repo:push", f.Hook, 422)
	send([]byte(`{}`), runs.ID(), "repo:push", f.Hook, 422)
	send(body, runs.ID(), "pullrequest:updated", f.Hook, 422)
	send([]byte(`{"repository":{"uuid":"{foreign-repository}"}}`), runs.ID(), "repo:push", f.Hook, 403)
	var receipts, jobs int
	if e := f.DB.Owner.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM cosmoformer.webhook_receipts),(SELECT count(*) FROM cosmoformer.outbox)`).Scan(&receipts, &jobs); e != nil || receipts != 1 || jobs != 1 {
		t.Fatal("rejected delivery changed queue or receipt evidence", receipts, jobs, e)
	}
	if f.Remote.Calls != 0 {
		t.Fatal("webhook request performed provider I/O")
	}
}
