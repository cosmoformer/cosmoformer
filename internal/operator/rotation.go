package operator

import (
	"bytes"
	"context"
	"errors"

	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

type encryptedRecord struct {
	org, id, table, column, purpose string
	revision                        int64
	value                           []byte
}

func records(ctx context.Context, tx pgx.Tx) ([]encryptedRecord, error) {
	var out []encryptedRecord
	err := tenants(ctx, tx, func(org string) error {
		for _, s := range []struct{ table, column, purpose string }{{"secrets", "encrypted_value", "managed_secret"}, {"source_credentials", "encrypted_value", "source_credential"}, {"integrations", "encrypted_credentials", "integration"}} {
			rows, err := tx.Query(ctx, `SELECT id::text,semantic_revision,`+s.column+` FROM cosmoformer.`+s.table+` WHERE `+s.column+` IS NOT NULL ORDER BY id`)
			if err != nil {
				return err
			}
			for rows.Next() {
				r := encryptedRecord{org: org, table: s.table, column: s.column, purpose: s.purpose}
				if err = rows.Scan(&r.id, &r.revision, &r.value); err != nil {
					rows.Close()
					return err
				}
				out = append(out, r)
				if len(out) > 65536 {
					rows.Close()
					return errors.New("offline rotation exceeds 65536-record maintenance bound")
				}
			}
			rows.Close()
			if rows.Err() != nil {
				return rows.Err()
			}
		}
		return nil
	})
	return out, err
}

// Rotate burns a committed, rounded-up quota before opening the atomic update
// transaction. Failed authentication/rollback/interruption never refunds it.
// The caller persists a protected receipt containing id and both fingerprints
// before invoking this method; status resolves uncertain commits under the lock.
func (o *Operator) Rotate(ctx context.Context, e Evidence, id string, old, next *cryptostore.Cipher) error {
	if id == "" {
		id = runs.ID()
	}
	oldFP, newFP := old.Fingerprint(), next.Fingerprint()
	if oldFP == newFP {
		return errors.New("rotation requires a different actual key")
	}
	if err := o.Offline(ctx, e); err != nil {
		return err
	}
	var batch []encryptedRecord
	var amount int64
	err := o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		var active []byte
		var restored bool
		if err := tx.QueryRow(ctx, `SELECT active_key_fingerprint,restore_key_required FROM cosmoformer.installation`).Scan(&active, &restored); err != nil {
			return err
		}
		if !bytes.Equal(active, oldFP[:]) {
			return errors.New("old key does not match committed database key; inspect status")
		}
		if restored && (!e.FreshKey || e.KeyHistoryReference == "") {
			return errors.New("restore requires independent evidence of a genuinely new, never-used write key")
		}
		var known bool
		if restored {
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.crypto_keys WHERE fingerprint=$1)`, newFP[:]).Scan(&known); err != nil {
				return err
			}
			if known {
				return errors.New("restored key history cannot establish fresh quota; supply a genuinely new key")
			}
		}
		var err error
		batch, err = records(ctx, tx)
		if err != nil {
			return err
		}
		amount = (int64(len(batch)) + 4095) / 4096 * 4096
		// A failed rotation can reserve additional quota under its unused target
		// key. A retired/previously active key can never be relabeled as new.
		if _, err = tx.Exec(ctx, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state) VALUES($1,'rotation') ON CONFLICT DO NOTHING`, newFP[:]); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE cosmoformer.crypto_keys SET reserved_encryptions=reserved_encryptions+$2 WHERE fingerprint=$1 AND state='rotation' AND reserved_encryptions<=4294967296-$2`, newFP[:], amount)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("new key is retired, already used or exhausted")
		}
		_, err = tx.Exec(ctx, `INSERT INTO cosmoformer.key_rotations(id,maintenance_id,old_fingerprint,new_fingerprint,reserved_encryptions) VALUES($1,$2,$3,$4,$5)`, id, e.MaintenanceID, oldFP[:], newFP[:], amount)
		return err
	})
	if err != nil {
		return err
	}
	return o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT active_key_fingerprint=$1 AND offline FROM cosmoformer.installation`, oldFP[:]).Scan(&matches); err != nil {
			return err
		}
		if !matches {
			return errors.New("rotation baseline changed")
		}
		used := int64(0)
		for _, r := range batch {
			aad := cryptostore.AAD(r.org, r.id, r.purpose, uint64(r.revision))
			plain, err := old.Open(r.value, aad)
			if err != nil {
				return errors.New("rotation ciphertext authentication failed; quota remains consumed")
			}
			value, err := next.Seal(plain, aad, func(fp [32]byte) error {
				if fp != newFP || used >= amount {
					return errors.New("rotation quota unavailable")
				}
				used++
				return nil
			})
			clear(plain)
			if err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, r.org); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE cosmoformer.`+r.table+` SET `+r.column+`=$1 WHERE id=$2 AND semantic_revision=$3 AND `+r.column+`=$4`, value, r.id, r.revision, r.value)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("rotation record changed")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.crypto_keys SET state='retired' WHERE fingerprint=$1`, oldFP[:]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.crypto_keys SET state='active' WHERE fingerprint=$1`, newFP[:]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET active_key_fingerprint=$1,restore_key_required=false`, newFP[:]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.key_rotations SET committed_at=clock_timestamp() WHERE id=$1 AND committed_at IS NULL`, id); err != nil {
			return err
		}
		return event(ctx, tx, "key.rotate", e)
	})
}
