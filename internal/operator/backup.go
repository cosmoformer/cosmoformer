package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/jackc/pgx/v5"
)

type BackupArtifact struct {
	Org     string `json:"org_id"`
	ID      string `json:"artifact_id"`
	Bucket  string `json:"bucket"`
	Key     string `json:"key"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}
type BackupReceipt struct {
	ID        string           `json:"backup_id"`
	Cutoff    time.Time        `json:"cutoff"`
	Digest    string           `json:"digest"`
	Count     int              `json:"count"`
	Artifacts []BackupArtifact `json:"artifacts"`
}

func (o *Operator) BackupCreate(ctx context.Context, e Evidence, id string) (BackupReceipt, error) {
	out := BackupReceipt{ID: id, Artifacts: []BackupArtifact{}}
	if !catalog.UUID(id) {
		return out, errors.New("--backup-id must be a UUID")
	}
	if err := o.Offline(ctx, e); err != nil {
		return out, err
	}
	err := o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.backup_sets WHERE id=$1)`, id).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("backup identity already captured; inspect retained receipt, do not recapture")
		}
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&out.Cutoff); err != nil {
			return err
		}
		if err := tenants(ctx, tx, func(org string) error {
			var busy bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifacts WHERE state IN ('uploading','deleting')) OR EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE completed_at IS NULL)`).Scan(&busy); err != nil {
				return err
			}
			if busy {
				return errors.New("artifact writes or cleanup remain unresolved")
			}
			// All these rows exist before the gated snapshot cutoff. Clock
			// corrections may put stored timestamps in the future; those rows
			// still belong to the database backup and must remain protected.
			rows, err := tx.Query(ctx, `SELECT id::text,bucket,object_key,version_id,encode(sha256,'hex'),byte_count FROM cosmoformer.artifacts WHERE state='finalized' ORDER BY id`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				a := BackupArtifact{Org: org}
				if err = rows.Scan(&a.ID, &a.Bucket, &a.Key, &a.Version, &a.SHA256, &a.Size); err != nil {
					return err
				}
				out.Artifacts = append(out.Artifacts, a)
			}
			return rows.Err()
		}); err != nil {
			return err
		}
		out.Count = len(out.Artifacts)
		b, _ := json.Marshal(out.Artifacts)
		h := sha256.Sum256(b)
		out.Digest = hex.EncodeToString(h[:])
		if _, err := tx.Exec(ctx, `INSERT INTO cosmoformer.backup_sets(id,cutoff,set_digest,artifact_count) VALUES($1,$2,$3,$4)`, id, out.Cutoff, h[:], out.Count); err != nil {
			return err
		}
		for _, a := range out.Artifacts {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, a.Org); err != nil {
				return err
			}
			hash, _ := hex.DecodeString(a.SHA256)
			if _, err := tx.Exec(ctx, `INSERT INTO cosmoformer.backup_artifacts(backup_id,org_id,artifact_id,bucket,object_key,version_id,sha256,byte_count) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, a.Org, a.ID, a.Bucket, a.Key, a.Version, hash, a.Size); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) VALUES($1,$2,'backup',$3,'backup_capture')`, a.Org, a.ID, id); err != nil {
				return err
			}
		}
		return event(ctx, tx, "backup.capture", e)
	})
	return out, err
}
func (o *Operator) BackupRelease(ctx context.Context, e Evidence, id string) error {
	if !catalog.UUID(id) || !e.RetentionChecked {
		return errors.New("backup UUID and explicit retention/custody checks required")
	}
	if err := o.Offline(ctx, e); err != nil {
		return err
	}
	return o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE cosmoformer.backup_sets SET released_at=clock_timestamp() WHERE id=$1 AND released_at IS NULL`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("backup not found or already released")
		}
		if err = tenants(ctx, tx, func(_ string) error {
			_, err := tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='backup' AND owner_id=$1 AND released_at IS NULL`, id)
			return err
		}); err != nil {
			return err
		}
		return event(ctx, tx, "backup.release", e)
	})
}

// BackupInspect recovers a lost filesystem receipt from the exact committed
// manifest. It never recaptures, extends, or releases a backup's protection.
func (o *Operator) BackupInspect(ctx context.Context, id string) (BackupReceipt, error) {
	out := BackupReceipt{ID: id, Artifacts: []BackupArtifact{}}
	if !catalog.UUID(id) {
		return out, errors.New("--backup-id must be a UUID")
	}
	err := o.tx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT cutoff,encode(set_digest,'hex'),artifact_count FROM cosmoformer.backup_sets WHERE id=$1`, id).Scan(&out.Cutoff, &out.Digest, &out.Count); err != nil {
			return errors.New("backup capture not found")
		}
		if err := tenants(ctx, tx, func(org string) error {
			rows, err := tx.Query(ctx, `SELECT artifact_id::text,bucket,object_key,version_id,encode(sha256,'hex'),byte_count FROM cosmoformer.backup_artifacts WHERE backup_id=$1 ORDER BY artifact_id`, id)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				a := BackupArtifact{Org: org}
				if err = rows.Scan(&a.ID, &a.Bucket, &a.Key, &a.Version, &a.SHA256, &a.Size); err != nil {
					return err
				}
				out.Artifacts = append(out.Artifacts, a)
			}
			return rows.Err()
		}); err != nil {
			return err
		}
		b, _ := json.Marshal(out.Artifacts)
		h := sha256.Sum256(b)
		if len(out.Artifacts) != out.Count || hex.EncodeToString(h[:]) != out.Digest {
			return errors.New("backup manifest digest/count mismatch; retain maintenance")
		}
		return nil
	})
	return out, err
}
