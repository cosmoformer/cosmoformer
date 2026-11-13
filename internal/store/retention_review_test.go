//go:build integration

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/httpapi"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

func reviewClaim(t *testing.T, legacy, unknownSize bool) (*testsupport.Database, map[string]string, evidence.Transaction, string, string) {
	t.Helper()
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	state := "finalized"
	if unknownSize {
		state = "uploading"
	}
	id := cleanupArtifact(t, db, x, state)
	if unknownSize {
		sqlOK(t, db.Owner, `UPDATE cosmoformer.artifacts SET version_id='exact',byte_count=NULL,sha256=NULL WHERE id=$1`, id)
	}
	closeCleanupFixture(t, db, x)
	tx := cleanupTx(t, db, x["org"])
	ctx := context.Background()
	claim := runs.ID()
	if legacy {
		if e := tx(func(q pgx.Tx) error { return evidence.ReconcileHolds(ctx, q, 100) }); e != nil {
			t.Fatal(e)
		}
		// This is a valid schema-15 durable claim; the forward migration preserves it.
		sqlOK(t, db.Owner, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'exact',$3,clock_timestamp())`, x["org"], id, claim)
	} else {
		if e := tx(func(q pgx.Tx) error {
			ids, e := evidence.ClaimCleanup(ctx, q, evidence.DefaultCleanupSettings(), 1)
			if e == nil {
				if len(ids) != 1 {
					t.Fatalf("claim count: %v", ids)
				}
				claim = ids[0]
			}
			return e
		}); e != nil {
			t.Fatal(e)
		}
	}
	return db, x, tx, id, claim
}
func TestRetentionReviewClaimedReadIsUnavailableBeforeStorage(t *testing.T) {
	_, _, tx, id, _ := reviewClaim(t, true, false)
	storage := &cleanupStorage{}
	c := &evidence.Custody{Storage: storage, Bucket: "synthetic"}
	_, file, e := c.Read(context.Background(), tx, id)
	if file != nil {
		file.Close()
		t.Fatal("claimed evidence was returned")
	}
	if !errors.Is(e, evidence.ErrUnavailable) || storage.gets != 0 {
		t.Fatalf("claim fence must precede storage reads: error=%v reads=%d", e, storage.gets)
	}
}
func TestRetentionReviewInconsistentClaimCannotDelete(t *testing.T) {
	_, _, tx, _, claim := reviewClaim(t, true, false)
	storage := &cleanupStorage{}
	c := &evidence.Custody{Storage: storage, Bucket: "synthetic"}
	if e := c.DeleteClaim(context.Background(), tx, claim); e == nil {
		t.Fatal("inconsistent claim admitted")
	}
	if storage.gets != 0 || storage.deletes != 0 {
		t.Fatalf("inconsistent claim reached storage: reads=%d deletes=%d", storage.gets, storage.deletes)
	}
	var reason string
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(context.Background(), `SELECT error_code FROM cosmoformer.cleanup_claims WHERE claim_id=$1`, claim).Scan(&reason)
	}); e != nil || reason != "CLAIM_STATE_MISMATCH" {
		t.Fatal(reason, e)
	}
}
func TestRetentionReviewDeletionRechecksProtectionBeforeIntent(t *testing.T) {
	for _, change := range []string{"attention", "missing_live_hold"} {
		t.Run(change, func(t *testing.T) {
			db, x, tx, id, claim := reviewClaim(t, false, false)
			storage := &cleanupStorage{}
			calls := 0
			ctx := context.Background()
			storage.outside = func() {
				calls++
				if calls != 1 {
					return
				}
				if change == "attention" {
					sqlOK(t, db.Owner, `UPDATE cosmoformer.artifacts SET cleanup_attention='REQUIRED_EVIDENCE_PROTECTION_LOST' WHERE id=$1`, id)
				} else {
					sqlOK(t, db.Owner, `UPDATE cosmoformer.operations SET status='needs_attention' WHERE id=$1`, x["operation"])
				}
			}
			c := &evidence.Custody{Storage: storage, Bucket: "synthetic"}
			if e := c.DeleteClaim(ctx, tx, claim); e == nil {
				t.Fatal("protection changed but deletion was admitted")
			}
			if storage.deletes != 0 {
				t.Fatal("deleted protected version")
			}
			var blocked, noIntent bool
			if e := tx(func(q pgx.Tx) error {
				return q.QueryRow(ctx, `SELECT a.cleanup_attention IS NOT NULL,c.delete_intent_at IS NULL FROM cosmoformer.artifacts a JOIN cosmoformer.cleanup_claims c ON c.artifact_id=a.id WHERE a.id=$1`, id).Scan(&blocked, &noIntent)
			}); e != nil || !blocked || !noIntent {
				t.Fatal("attention/intent not durable", blocked, noIntent, e)
			}
		})
	}
}
func TestRetentionReviewKnownOrphanVersionNeedsNoGuessedSize(t *testing.T) {
	_, _, tx, _, claim := reviewClaim(t, false, true)
	storage := &cleanupStorage{}
	c := &evidence.Custody{Storage: storage, Bucket: "synthetic"}
	if e := c.DeleteClaim(context.Background(), tx, claim); e != nil || storage.deletes != 1 {
		t.Fatal("known exact orphan was not deleted", e, storage.deletes)
	}
}

type replayStorage struct {
	keys    map[string]bool
	fail    bool
	deletes int
}

func (s *replayStorage) Check(context.Context) error { return nil }
func (s *replayStorage) Put(context.Context, string, io.Reader, int64) (string, error) {
	panic("unexpected upload")
}
func (s *replayStorage) Get(_ context.Context, key, version string) (evidence.Object, error) {
	if _, ok := s.keys[key]; !ok || version != "exact" {
		return evidence.Object{}, evidence.ErrInvalid
	}
	return evidence.Object{Version: version, Size: 4, Encryption: "AES256", Body: io.NopCloser(strings.NewReader("good"))}, nil
}
func (s *replayStorage) Delete(_ context.Context, key, version string) error {
	s.deletes++
	if _, ok := s.keys[key]; !ok || version != "exact" {
		return evidence.ErrInvalid
	}
	if s.fail {
		return evidence.ErrUnavailable
	}
	s.keys[key] = true
	return nil
}
func TestRetentionReviewReplayHonorsCurrentBatchAndMakesProgress(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	a := cleanupArtifact(t, db, x, "finalized")
	b := cleanupArtifact(t, db, x, "finalized")
	closeCleanupFixture(t, db, x)
	token, csrf, key := identity.Token(), identity.Token(), runs.ID()
	sqlOK(t, db.Owner, `UPDATE cosmoformer.accounts SET installation_admin=true WHERE id=$1`, x["account"])
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.sessions(account_id,verifier,csrf_verifier,credential_revision) VALUES($1,$2,$3,1)`, x["account"], identity.Digest(token), identity.Digest(csrf))
	st := &replayStorage{keys: map[string]bool{a: false, b: false}, fail: true}
	settings := evidence.DefaultCleanupSettings()
	settings.Batch = 2
	handler, _ := httpapi.NewWithOptions(db.App, "https://localhost:5173", httpapi.Options{Cleanup: settings, Evidence: &evidence.Custody{Storage: st, Bucket: "synthetic"}})
	payload, _ := json.Marshal(map[string]any{"org_id": x["org"], "limit": 2})
	call := func() []string {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/admin/cleanup", bytes.NewReader(payload))
		r.AddCookie(&http.Cookie{Name: "__Host-cosmoformer_session", Value: token})
		r.Header.Set("Origin", "https://localhost:5173")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Cosmoformer-Request", "browser")
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("cleanup status %d: %s", w.Code, w.Body.String())
		}
		var out struct {
			Claims []string `json:"claim_ids"`
		}
		if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
			t.Fatal(e)
		}
		return out.Claims
	}
	original := call()
	if len(original) != 2 || st.deletes != 2 {
		t.Fatal(original, st.deletes)
	}
	settings.Batch = 1
	st.fail = false
	handler, _ = httpapi.NewWithOptions(db.App, "https://localhost:5173", httpapi.Options{Cleanup: settings, Evidence: &evidence.Custody{Storage: st, Bucket: "synthetic"}})
	for _, want := range []int{3, 4, 4} {
		again := call()
		if !bytes.Equal(reviewJSON(original), reviewJSON(again)) || st.deletes != want {
			t.Fatal("replay changed receipt, exceeded bound or failed to advance", again, st.deletes, want)
		}
	}
}
func reviewJSON(v any) []byte { b, _ := json.Marshal(v); return b }
