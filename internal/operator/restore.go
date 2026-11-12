package operator

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/jackc/pgx/v5"
)

// RestoreBegin is the first operation on a restored database, before serve or
// worker enrollment. It commits fail-closed revocation separately from inventory
// reconciliation so incomplete evidence cannot roll the revocation back.
func (o *Operator) RestoreBegin(ctx context.Context, id string) error {
	if !catalog.UUID(id) {
		return errors.New("--restore-id UUID required")
	}
	return o.tx(ctx, func(tx pgx.Tx) error {
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.operator_events WHERE id=$1)`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return errors.New("restore identity already used; inspect status")
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET maintenance=true,offline=true,maintenance_id=gen_random_uuid(),restore_id=$1,restore_reconciled=false,restore_key_required=true,clock_unreliable=true`, id); err != nil {
			return err
		}
		// Every restored allowance is historical, including unfinished rotation
		// targets. Permanently retire them before accepting a fresh write key;
		// clearing restore_key_required must never make one reusable later.
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.crypto_keys SET state='retired'`); err != nil {
			return err
		}
		if err := invalidate(ctx, tx, "RESTORE"); err != nil {
			return err
		}
		// Passwords and all restored bearer/proof credentials are no longer authority.
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.accounts SET password_phc=NULL,credential_revision=credential_revision+1,revision=revision+1`); err != nil {
			return err
		}
		if err := tenants(ctx, tx, func(_ string) error {
			var verifier [32]byte
			if _, err := rand.Read(verifier[:]); err != nil {
				return err
			}
			for _, q := range []string{
				`UPDATE cosmoformer.workers SET verifier=$1,paused=true,generation=generation+1,agent_instance=NULL,revision=revision+1`,
			} {
				if _, err := tx.Exec(ctx, q, verifier[:]); err != nil {
					return err
				}
			}
			for _, q := range []string{
				`UPDATE cosmoformer.source_credentials SET enabled=false,revision=revision+1`,
				`UPDATE cosmoformer.integrations SET enabled=false,reconnect_required=true,refresh_claim=NULL,refresh_claimed_at=NULL,hook_uuid=NULL,revision=revision+1`,
				`UPDATE cosmoformer.outbox SET status='failed' WHERE status='pending'`,
				`UPDATE cosmoformer.operations o SET status='retired',retired_at=clock_timestamp(),terminal_reason='RESTORE',revision=revision+1 WHERE status='queued' AND NOT EXISTS(SELECT FROM cosmoformer.attempts a WHERE a.operation_id=o.id AND a.closed_at IS NULL)`,
				`UPDATE cosmoformer.work_items SET status='retired',revision=revision+1 WHERE status='queued' AND operation_id IN(SELECT id FROM cosmoformer.operations WHERE status='retired')`,
			} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			// Revocation must preserve ciphertext AAD semantic identity. Source
			// credentials are disabled; their semantic revision remains unchanged.
			return nil
		}); err != nil {
			return err
		}
		var mid string
		if err := tx.QueryRow(ctx, `SELECT maintenance_id::text FROM cosmoformer.installation`).Scan(&mid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO cosmoformer.operator_events(id,maintenance_id,action,evidence) VALUES($1,$2,'restore.begin','{}')`, id, mid)
		return err
	})
}
func (o *Operator) RestoreReconcile(ctx context.Context, e Evidence) error {
	if !e.InventoryComplete || !e.ExternalWritersStopped || !e.SourceReconciled || e.InventoryReference == "" {
		return errors.New("complete external worker/writer/backend/Git inventory and stop evidence required")
	}
	// A restored absence of work cannot justify an idle classification. Every
	// inventoried process, including a worker newer than the backup, must have
	// independent stopped-process evidence before reconciliation can complete.
	for _, w := range e.Workers {
		if w.State != "stopped" {
			return errors.New("restore requires stopped-process evidence for every worker, including post-backup workers")
		}
	}
	if err := o.Offline(ctx, e); err != nil {
		return err
	}
	return o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT restore_id IS NOT NULL AND NOT restore_reconciled FROM cosmoformer.installation`).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return errors.New("restore reconciliation is not pending")
		}
		// bind_backend already owns exact state and auxiliary namespace exclusion.
		// Explicit post-backup bindings may be adopted only onto an existing exact
		// stack/revision. A missing stack cannot silently turn a used target into free.
		seen := map[string]bool{}
		for _, b := range e.Backends {
			identity := b.Partition + "/" + b.Bucket + "/" + b.Key
			if !catalog.UUID(b.Org) || !catalog.UUID(b.Stack) || b.Reference == "" || seen[identity] {
				return errors.New("invalid or duplicate backend inventory")
			}
			seen[identity] = true
			if _, err := tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, b.Org); err != nil {
				return err
			}
			var matches bool
			if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT FROM cosmoformer.stack_bindings WHERE stack_id=$1) OR EXISTS(SELECT FROM cosmoformer.stack_bindings b JOIN cosmoformer.backend_targets t ON t.id=b.target_id WHERE b.stack_id=$1 AND t.partition=$2 AND t.bucket=$3 AND t.effective_key=$4 AND b.profile->>'lock_profile'=$5 AND (b.profile->>'table_arn') IS NOT DISTINCT FROM $6::text)`, b.Stack, b.Partition, b.Bucket, b.Key, b.LockProfile, b.TableARN).Scan(&matches); err != nil {
				return err
			}
			if !matches {
				return errors.New("backend inventory conflicts with retained binding")
			}
			if _, err := tx.Exec(ctx, `SELECT cosmoformer.bind_backend($1,$2,$3,$4,$5,$6,$7)`, b.Org, b.Stack, b.Partition, b.Bucket, b.Key, b.LockProfile, b.TableARN); err != nil {
				return errors.New("backend adoption failed; retain maintenance and reconcile exact stack/namespace inventory")
			}
		}
		if err := tenants(ctx, tx, func(org string) error {
			rows, err := tx.Query(ctx, `SELECT b.stack_id::text,t.partition,t.bucket,t.effective_key FROM cosmoformer.stack_bindings b JOIN cosmoformer.backend_targets t ON t.id=b.target_id`)
			if err != nil {
				return err
			}
			for rows.Next() {
				var stack, part, bucket, key string
				if err = rows.Scan(&stack, &part, &bucket, &key); err != nil {
					rows.Close()
					return err
				}
				found := false
				for _, b := range e.Backends {
					if b.Org == org && b.Stack == stack && b.Partition == part && b.Bucket == bucket && b.Key == key {
						found = true
					}
				}
				if !found {
					rows.Close()
					return errors.New("backend inventory omits a retained binding")
				}
			}
			rows.Close()
			if rows.Err() != nil {
				return rows.Err()
			}
			// Every adopted backend enters the existing recovery/voting path. Existing
			// cases receive new evidence identity, invalidating old votes. No stop or
			// reconciliation clearance is fabricated from the host's inventory.
			for _, q := range []string{
				`UPDATE cosmoformer.backend_targets SET state_epoch=state_epoch+1 WHERE id IN(SELECT target_id FROM cosmoformer.stack_bindings)`,
				`UPDATE cosmoformer.recovery_cases SET evidence=jsonb_build_object('restore_id',(SELECT restore_id FROM cosmoformer.installation)),stop_verified=false,reconciliation_complete=false,revision=revision+1 WHERE status='open'`,
				`INSERT INTO cosmoformer.recovery_cases(org_id,stack_id,target_id,origin_attempt_id,status,evidence,opened_at) SELECT org_id,stack_id,target_id,(SELECT a.id FROM cosmoformer.attempts a WHERE a.target_id=b.target_id AND a.closed_at IS NULL ORDER BY a.created_at,a.id LIMIT 1),'open',jsonb_build_object('restore_id',(SELECT restore_id FROM cosmoformer.installation)),clock_timestamp() FROM cosmoformer.stack_bindings b WHERE NOT EXISTS(SELECT FROM cosmoformer.recovery_cases r WHERE r.target_id=b.target_id AND r.status='open')`,
				`INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT ar.org_id,ar.id,'recovery',r.id,'restore' FROM cosmoformer.artifacts ar JOIN cosmoformer.attempts a ON a.id=ar.attempt_id JOIN cosmoformer.recovery_cases r ON r.target_id=a.target_id AND r.status='open' WHERE ar.state IN ('uploading','finalized') AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims c WHERE c.artifact_id=ar.id) ON CONFLICT DO NOTHING`,
			} {
				if _, err = tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			checks := map[string]ArtifactCheck{}
			for _, a := range e.Artifacts {
				if a.Org != org {
					continue
				}
				if _, ok := checks[a.ID]; ok || a.Reference == "" {
					return errors.New("invalid artifact verification evidence")
				}
				checks[a.ID] = a
			}
			ar, err := tx.Query(ctx, `SELECT id::text,bucket,object_key,coalesce(version_id,''),coalesce(encode(sha256,'hex'),''),coalesce(byte_count,0) FROM cosmoformer.artifacts WHERE state IN ('uploading','finalized','deleting') ORDER BY id`)
			if err != nil {
				return err
			}
			var unavailable []string
			for ar.Next() {
				var id, bucket, key, version, hash string
				var size int64
				if err = ar.Scan(&id, &bucket, &key, &version, &hash, &size); err != nil {
					ar.Close()
					return err
				}
				a, ok := checks[id]
				if ok && (a.Bucket != bucket || a.Key != key || a.Version != version || a.SHA256 != hash || a.Size != size) {
					ar.Close()
					return errors.New("artifact check must name the exact retained version and digest")
				}
				if !ok || !a.Available || version == "" {
					unavailable = append(unavailable, id)
				}
				delete(checks, id)
			}
			ar.Close()
			if ar.Err() != nil {
				return ar.Err()
			}
			if len(checks) > 0 {
				return errors.New("artifact check names an unknown artifact")
			}
			for _, id := range unavailable {
				if _, err = tx.Exec(ctx, `UPDATE cosmoformer.artifacts SET state='unavailable',revision=revision+1 WHERE id=$1`, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET restore_reconciled=true`); err != nil {
			return err
		}
		return event(ctx, tx, "restore.reconcile", e)
	})
}
