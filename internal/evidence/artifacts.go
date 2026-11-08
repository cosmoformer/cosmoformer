package evidence

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

// Transaction must revalidate current authorization and take the coordination
// gate on every call. It must never be held across an external storage operation.
var transfers = make(chan struct{}, 4)

func transfer() bool {
	select {
	case transfers <- struct{}{}:
		return true
	default:
		return false
	}
}
func releaseTransfer() { <-transfers }

type Transaction func(func(pgx.Tx) error) error
type Artifact struct {
	ID      string `json:"artifact_id"`
	Upload  string `json:"upload_id"`
	Attempt string `json:"attempt_id"`
	Kind    string `json:"kind"`
	Bucket  string `json:"-"`
	Key     string `json:"-"`
	Version string `json:"version_id"`
	Hash    string `json:"sha256"`
	Size    int64  `json:"size"`
	State   string `json:"status"`
}
type Custody struct {
	Storage Storage
	Bucket  string
}

func Limit(kind string) int64 {
	switch kind {
	case "log_segment":
		return SegmentMax
	case "pre_init_spec", "binary_plan", "raw_plan_json", "provider_lock", "manifest", "provider_schema", "normalized_changes", "findings", "recovery_plan", "evidence_attachment":
		return PlanMax
	}
	return 0
}
func Raw(kind string) bool { return kind != "log_segment" } // sanitized analysis readiness is a later milestone
func validInput(kind, hash string, size int64) bool {
	b, e := hex.DecodeString(hash)
	return e == nil && len(b) == 32 && hash == strings.ToLower(hash) && size >= 0 && Limit(kind) > 0 && size <= Limit(kind)
}
func lookup(ctx context.Context, tx pgx.Tx, field, id string, attempt ...string) (Artifact, error) {
	var a Artifact
	// field is an internal constant, never request text.
	args := []any{id}
	where := field + "=$1"
	if len(attempt) > 0 {
		where += " AND attempt_id=$2"
		args = append(args, attempt[0])
	}
	e := tx.QueryRow(ctx, `SELECT id::text,upload_id::text,attempt_id::text,kind,bucket,object_key,coalesce(version_id,''),encode(sha256,'hex'),byte_count,state FROM cosmoformer.artifacts WHERE `+where, args...).Scan(&a.ID, &a.Upload, &a.Attempt, &a.Kind, &a.Bucket, &a.Key, &a.Version, &a.Hash, &a.Size, &a.State)
	return a, e
}
func (c *Custody) Upload(ctx context.Context, tx Transaction, attempt, upload, kind, hash string, size int64, r io.Reader) (Artifact, error) {
	if !transfer() {
		return Artifact{}, ErrUnavailable
	}
	defer releaseTransfer()
	var a Artifact
	if !validInput(kind, hash, size) || !catalog.UUID(upload) {
		return a, ErrInvalid
	}
	if c == nil || c.Storage == nil {
		return a, ErrUnavailable
	}
	fresh := false
	e := tx(func(q pgx.Tx) error {
		var e error
		a, e = lookup(ctx, q, "upload_id", upload, attempt)
		if e == nil {
			if a.Attempt != attempt || a.Kind != kind || a.Hash != hash || a.Size != size {
				return store.ErrConflict
			}
			if a.State == "uploading" && a.Version == "" {
				return ErrUploadIncomplete
			}
			if a.State != "finalized" && a.State != "uploading" {
				return store.ErrConflict
			}
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		a = Artifact{ID: runs.ID(), Upload: upload, Attempt: attempt, Kind: kind, Bucket: c.Bucket, Key: runs.ID(), Hash: hash, Size: size, State: "uploading"}
		_, e = q.Exec(ctx, `INSERT INTO cosmoformer.artifacts(id,org_id,attempt_id,plan_id,kind,upload_id,bucket,object_key,sha256,byte_count,state) SELECT $1,t.org_id,t.id,(SELECT id FROM cosmoformer.plans WHERE attempt_id=t.id AND status='assembling'),$3,$4,$5,$6,decode($7,'hex'),$8,'uploading' FROM cosmoformer.attempts t WHERE t.id=$2`, a.ID, attempt, kind, upload, c.Bucket, a.Key, hash, size)
		if e != nil {
			return e
		}
		_, e = q.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT org_id,id,'attempt',attempt_id,'custody' FROM cosmoformer.artifacts WHERE id=$1`, a.ID)
		fresh = true
		return e
	})
	if e != nil || !fresh {
		return a, e
	}
	f, e := spool(r, size, hash)
	if e != nil {
		return a, e
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	if e = c.Storage.Check(ctx); e != nil {
		return a, ErrUnavailable
	}
	version, e := c.Storage.Put(ctx, a.Key, f, size)
	if e != nil || !validVersion(version) {
		return a, ErrUnavailable
	}
	e = tx(func(q pgx.Tx) error {
		_, e := q.Exec(ctx, `UPDATE cosmoformer.artifacts SET version_id=$2 WHERE id=$1 AND state='uploading' AND version_id IS NULL`, a.ID, version)
		return e
	})
	a.Version = version
	return a, e
}
func (c *Custody) verified(ctx context.Context, a Artifact) (*os.File, error) {
	if c == nil || c.Storage == nil || a.Bucket != c.Bucket || !validInput(a.Kind, a.Hash, a.Size) || !validVersion(a.Version) {
		return nil, ErrUnavailable
	}
	if e := c.Storage.Check(ctx); e != nil {
		return nil, ErrUnavailable
	}
	o, e := c.Storage.Get(ctx, a.Key, a.Version)
	if e != nil {
		return nil, e
	}
	defer o.Body.Close()
	if o.Version != a.Version || o.Size != a.Size || o.Encryption != "AES256" {
		return nil, ErrInvalid
	}
	return spool(o.Body, a.Size, a.Hash)
}
func (c *Custody) Finalize(ctx context.Context, tx Transaction, attempt, upload, kind, hash string, size int64) (Artifact, error) {
	if !transfer() {
		return Artifact{}, ErrUnavailable
	}
	defer releaseTransfer()
	var a Artifact
	e := tx(func(q pgx.Tx) error {
		var e error
		a, e = lookup(ctx, q, "upload_id", upload, attempt)
		if e != nil {
			return e
		}
		if a.Attempt != attempt || a.Kind != kind || a.Hash != hash || a.Size != size {
			return store.ErrConflict
		}
		if a.State != "uploading" && a.State != "finalized" {
			return ErrUnavailable
		}
		return nil
	})
	if e != nil || a.State == "finalized" {
		return a, e
	}
	f, e := c.verified(ctx, a)
	if e != nil {
		if errors.Is(e, ErrInvalid) {
			c.unavailable(ctx, tx, a.ID)
		}
		return a, ErrUnavailable
	}
	var segment *Segment
	if kind == "log_segment" {
		var v Segment
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if dec.Decode(&v) != nil || v.Validate() != nil {
			f.Close()
			os.Remove(f.Name())
			return a, ErrInvalid
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			f.Close()
			os.Remove(f.Name())
			return a, ErrInvalid
		}
		segment = &v
	}
	f.Close()
	os.Remove(f.Name())
	e = tx(func(q pgx.Tx) error {
		current, e := lookup(ctx, q, "id", a.ID)
		if e != nil {
			return e
		}
		if current.State == "finalized" {
			a = current
			return nil
		}
		if current.State != "uploading" || current.Version != a.Version {
			return ErrUnavailable
		}
		_, e = q.Exec(ctx, `UPDATE cosmoformer.artifacts SET state='finalized',finalized_at=clock_timestamp() WHERE id=$1`, a.ID)
		if e != nil {
			return e
		}
		if kind != "log_segment" {
			if _, e = q.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT org_id,id,'plan',plan_id,'retained_evidence' FROM cosmoformer.artifacts WHERE id=$1 AND plan_id IS NOT NULL ON CONFLICT DO NOTHING`, a.ID); e != nil {
				return e
			}
		}
		// Durable logs append in archive order. Replays cannot allocate another segment.
		if segment != nil {
			v := segment
			var sequence, offset int64
			if e = q.QueryRow(ctx, `SELECT coalesce(max(sequence)+1,0),coalesce(max(last_offset),0) FROM cosmoformer.log_segments WHERE attempt_id=$1 AND stream='console'`, attempt).Scan(&sequence, &offset); e != nil {
				return e
			}
			if v.Sequence != sequence || v.Start != offset {
				return store.ErrConflict
			}
			var artifact any = a.ID
			var reason any
			unknown := false
			if v.Gap != nil {
				artifact = nil
				reason = v.Gap.Reason
				unknown = v.Gap.Unknown
			}
			_, e = q.Exec(ctx, `INSERT INTO cosmoformer.log_segments(org_id,attempt_id,artifact_id,stream,sequence,first_offset,last_offset,gap,gap_reason,unknown_extent) SELECT org_id,attempt_id,$2,'console',$3,$4,$5,$6,$7,$8 FROM cosmoformer.artifacts WHERE id=$1`, a.ID, artifact, v.Sequence, v.Start, v.End, v.Gap != nil, reason, unknown)
		}

		a.State = "finalized"
		return e
	})
	return a, e
}
func (c *Custody) unavailable(ctx context.Context, tx Transaction, id string) {
	_ = tx(func(q pgx.Tx) error {
		_, e := q.Exec(ctx, `UPDATE cosmoformer.artifacts SET state='unavailable' WHERE id=$1 AND state IN ('uploading','finalized')`, id)
		return e
	})
}
func (c *Custody) Read(ctx context.Context, tx Transaction, id string) (Artifact, *os.File, error) {
	if !transfer() {
		return Artifact{}, nil, ErrUnavailable
	}
	defer releaseTransfer()
	var a Artifact
	e := tx(func(q pgx.Tx) error {
		var e error
		a, e = lookup(ctx, q, "id", id)
		if e != nil {
			return e
		}
		if a.State != "finalized" {
			return ErrUnavailable
		}
		return nil
	})
	if e != nil {
		return a, nil, e
	}
	f, e := c.verified(ctx, a)
	if e != nil {
		if errors.Is(e, ErrInvalid) {
			c.unavailable(ctx, tx, id)
		}
		return a, nil, ErrUnavailable
	}
	// Recheck current permissions after external I/O and before releasing bytes.
	e = tx(func(q pgx.Tx) error {
		v, e := lookup(ctx, q, "id", id)
		if e == nil && v.State != "finalized" {
			return ErrUnavailable
		}
		return e
	})
	if e != nil {
		f.Close()
		os.Remove(f.Name())
		return a, nil, e
	}
	return a, f, nil
}
