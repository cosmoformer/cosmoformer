//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

type syntheticStorage struct {
	body, version string
	puts          int
}

func (s *syntheticStorage) Check(context.Context) error { return nil }
func (s *syntheticStorage) Put(_ context.Context, _ string, r io.Reader, _ int64) (string, error) {
	b, e := io.ReadAll(r)
	s.body = string(b)
	s.puts++
	return "exact", e
}
func (s *syntheticStorage) Get(_ context.Context, _ string, v string) (evidence.Object, error) {
	if v != "exact" {
		return evidence.Object{}, evidence.ErrUnavailable
	}
	version := s.version
	if version == "" {
		version = "exact"
	}
	return evidence.Object{Version: version, Size: int64(len(s.body)), Encryption: "AES256", Body: io.NopCloser(strings.NewReader(s.body))}, nil
}
func digest(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func TestCustodyReplayIntegrityTenantAndHolds(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	ctx := context.Background()
	tx := func(fn func(pgx.Tx) error) error {
		q, e := db.App.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer q.Rollback(ctx)
		if _, e = q.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
			return e
		}
		if _, e = q.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, x["org"]); e != nil {
			return e
		}
		if e = fn(q); e != nil {
			return e
		}
		return q.Commit(ctx)
	}
	st := &syntheticStorage{}
	c := &evidence.Custody{Storage: st, Bucket: "synthetic"}
	upload := runs.ID()
	a, e := c.Upload(ctx, tx, x["attempt"], upload, "manifest", digest("good"), 4, strings.NewReader("good"))
	if e != nil {
		t.Fatal(e)
	}
	replay, e := c.Upload(ctx, tx, x["attempt"], upload, "manifest", digest("good"), 4, strings.NewReader("good"))
	if e != nil || replay.ID != a.ID || st.puts != 1 {
		t.Fatal("lost reply duplicated", e)
	}
	if _, e = c.Upload(ctx, tx, x["attempt"], upload, "manifest", digest("evil"), 4, strings.NewReader("evil")); e == nil {
		t.Fatal("mismatched retry")
	}
	a, e = c.Finalize(ctx, tx, x["attempt"], upload, "manifest", digest("good"), 4)
	if e != nil {
		t.Fatal(e)
	}
	b, e := c.Finalize(ctx, tx, x["attempt"], upload, "manifest", digest("good"), 4)
	if e != nil || b.Version != a.Version {
		t.Fatal("finalize replay", e)
	}
	var held int
	if e = tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.artifact_holds WHERE artifact_id=$1 AND owner_kind='attempt' AND released_at IS NULL`, a.ID).Scan(&held)
	}); e != nil || held != 1 {
		t.Fatal("missing hold", e)
	}
	sqlReject(t, db.Owner, "23514", `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at) VALUES($1,$2,'exact',gen_random_uuid(),now())`, x["org"], a.ID)
	_, f, e := c.Read(ctx, tx, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	os.Remove(f.Name())
	st.version = "wrong"
	if _, _, e = c.Read(ctx, tx, a.ID); e == nil {
		t.Fatal("wrong version accepted")
	}
	var state string
	tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT state FROM cosmoformer.artifacts WHERE id=$1`, a.ID).Scan(&state)
	})
	if state != "unavailable" {
		t.Fatal("not marked unavailable")
	}
	incomplete := runs.ID()
	if _, e = c.Upload(ctx, tx, x["attempt"], incomplete, "manifest", digest("good"), 4, strings.NewReader("goo")); e == nil {
		t.Fatal("incomplete upload")
	}
	if _, e = c.Finalize(ctx, tx, x["attempt"], incomplete, "manifest", digest("good"), 4); e == nil {
		t.Fatal("incomplete finalized")
	}
	if _, e = c.Upload(ctx, tx, x["attempt"], incomplete, "manifest", digest("good"), 4, strings.NewReader("good")); e == nil {
		t.Fatal("ambiguous upload replay reused key")
	}
}
func TestLogArchiveOrdersSegmentsAndExplicitGaps(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	ctx := context.Background()
	st := &syntheticStorage{}
	c := &evidence.Custody{Storage: st, Bucket: "synthetic"}
	tx := func(fn func(pgx.Tx) error) error {
		q, e := db.App.Pool.Begin(ctx)
		if e != nil {
			return e
		}
		defer q.Rollback(ctx)
		q.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`)
		q.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, x["org"])
		if e = fn(q); e != nil {
			return e
		}
		return q.Commit(ctx)
	}
	put := func(payload string, want bool) {
		t.Helper()
		u := runs.ID()
		_, e := c.Upload(ctx, tx, x["attempt"], u, "log_segment", digest(payload), int64(len(payload)), strings.NewReader(payload))
		if e != nil {
			t.Fatal(e)
		}
		_, e = c.Finalize(ctx, tx, x["attempt"], u, "log_segment", digest(payload), int64(len(payload)))
		if (e == nil) != want {
			t.Fatalf("ordered finalize success=%t expected=%t: %v", e == nil, want, e)
		}
	}
	put(`{"sequence":1,"byte_start":0,"byte_end":1,"text":"x"}`, false)
	put(`{"sequence":0,"byte_start":0,"byte_end":1,"text":"x"}`, true)
	put(`{"sequence":1,"byte_start":1,"byte_end":20,"gap":{"reason":"worker_buffer_overflow","unknown_extent":false}}`, true)
	put(`{"sequence":2,"byte_start":20,"byte_end":20,"gap":{"reason":"worker_restart","unknown_extent":true}}`, true)
	var count int
	var end int64
	if e := tx(func(q pgx.Tx) error {
		return q.QueryRow(ctx, `SELECT count(*),max(last_offset) FROM cosmoformer.log_segments WHERE attempt_id=$1`, x["attempt"]).Scan(&count, &end)
	}); e != nil || count != 3 || end != 20 {
		t.Fatal(count, end, e)
	}
}
