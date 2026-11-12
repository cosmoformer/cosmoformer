// Package operator implements explicit host-only, offline recovery operations.
package operator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

// Evidence references independently collected host/process and external custody
// facts. No timestamp, heartbeat absence or database projection proves a stop.
// The maintenance identity prevents reuse across maintenance windows.
type Evidence struct {
	MaintenanceID          string          `json:"maintenance_id"`
	Reference              string          `json:"reference"`
	WritersStopped         bool            `json:"writers_stopped"`
	CleanupStopped         bool            `json:"cleanup_stopped"`
	Workers                []WorkerStop    `json:"workers"`
	ClockValidation        string          `json:"clock_validation"`
	RetentionChecked       bool            `json:"retention_checked"`
	InventoryComplete      bool            `json:"inventory_complete"`
	ExternalWritersStopped bool            `json:"external_writers_stopped"`
	InventoryReference     string          `json:"inventory_reference"`
	SourceReconciled       bool            `json:"source_reconciled"`
	FreshKey               bool            `json:"fresh_key_never_used"`
	KeyHistoryReference    string          `json:"key_history_reference"`
	Backends               []Backend       `json:"backends"`
	Artifacts              []ArtifactCheck `json:"artifacts"`
}
type WorkerStop struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	Agent      string `json:"agent"`
	State      string `json:"state"` // independently verified stopped or idle
	Reference  string `json:"reference"`
}
type Backend struct {
	Org         string  `json:"org_id"`
	Stack       string  `json:"stack_id"`
	Partition   string  `json:"partition"`
	Bucket      string  `json:"bucket"`
	Key         string  `json:"key"`
	LockProfile string  `json:"lock_profile"`
	TableARN    *string `json:"table_arn"`
	Reference   string  `json:"reference"`
}
type ArtifactCheck struct {
	Org       string `json:"org_id"`
	ID        string `json:"id"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	Available bool   `json:"available"`
	Reference string `json:"reference"`
}

// ReadEvidenceFile keeps large recovery inventories separate from the smaller
// credential-file limit. Both retain the protected regular-file boundary.
func ReadEvidenceFile(path string) (Evidence, error) {
	var empty Evidence
	f, err := os.Open(path)
	if err != nil {
		return empty, errors.New("cannot open protected operator evidence")
	}
	defer f.Close()
	s, err := f.Stat()
	if err != nil || !s.Mode().IsRegular() || s.Mode().Perm()&0077 != 0 || s.Size() == 0 || s.Size() > 16<<20 {
		return empty, errors.New("operator evidence must be an owner-only regular file of at most 16 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if err != nil || len(b) > 16<<20 {
		return empty, errors.New("cannot read bounded operator evidence")
	}
	return ParseEvidence(b)
}

func ParseEvidence(b []byte) (Evidence, error) {
	var e Evidence
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&e); err != nil {
		return e, errors.New("invalid operator evidence")
	}
	if d.Decode(new(any)) != io.EOF {
		return e, errors.New("invalid operator evidence trailing data")
	}
	return e, nil
}

type Operator struct{ Conn *pgx.Conn }

// Open uses a dedicated session. The session advisory lock spans quota commit
// and ciphertext commit, so an uncertain earlier operation must settle before
// status or another host command can inspect/retry it.
func Open(ctx context.Context, dsn string) (*Operator, error) {
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, errors.New("operator database connection unavailable")
	}
	fail := func(err error) (*Operator, error) { c.Close(ctx); return nil, err }
	var safe bool
	err = c.QueryRow(ctx, `SELECT session_user=current_user AND current_user='cosmoformer_owner' AND NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND (SELECT nspowner=r.oid FROM pg_namespace WHERE nspname='cosmoformer') FROM pg_roles r WHERE rolname=current_user`).Scan(&safe)
	if err != nil || !safe {
		return fail(errors.New("host operation requires verified migration-owner authority"))
	}
	var version int32
	if err = c.QueryRow(ctx, `SELECT version FROM cosmoformer.schema_version`).Scan(&version); err != nil || version != store.SchemaVersion {
		return fail(errors.New("unsupported operator schema; run forward migrations"))
	}
	if _, err = c.Exec(ctx, `SELECT pg_advisory_lock(707133,13)`); err != nil {
		return fail(err)
	}
	return &Operator{c}, nil
}
func (o *Operator) Close() { o.Conn.Close(context.Background()) }
func (o *Operator) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := o.Conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("operator commit uncertain; remain in maintenance and inspect status under the operator lock")
	}
	return nil
}
func tenants(ctx context.Context, tx pgx.Tx, fn func(string) error) error {
	rows, err := tx.Query(ctx, `SELECT id::text FROM cosmoformer.organizations ORDER BY id`)
	if err != nil {
		return err
	}
	var orgs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		orgs = append(orgs, id)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, id := range orgs {
		if _, err = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, id); err != nil {
			return err
		}
		if err = fn(id); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `SELECT set_config('app.org_id','',true)`)
	return err
}
func event(ctx context.Context, tx pgx.Tx, action string, e Evidence) error {
	b, _ := json.Marshal(e)
	_, err := tx.Exec(ctx, `INSERT INTO cosmoformer.operator_events(id,maintenance_id,action,evidence) VALUES($1,$2,$3,$4)`, runs.ID(), e.MaintenanceID, action, b)
	return err
}

type Status struct {
	ActiveReserved      int64           `json:"active_reserved_encryptions"`
	RotationRecommended bool            `json:"rotation_recommended"`
	BackupSets          json.RawMessage `json:"backup_sets"`
	Maintenance         bool            `json:"maintenance"`
	MaintenanceID       string          `json:"maintenance_id"`
	Offline             bool            `json:"offline"`
	ClockUnreliable     bool            `json:"clock_unreliable"`
	ActiveKey           string          `json:"active_key_fingerprint"`
	RestoreID           string          `json:"restore_id"`
	RestoreReconciled   bool            `json:"restore_reconciled"`
	RestoreKeyRequired  bool            `json:"restore_key_required"`
	Rotations           json.RawMessage `json:"rotations"`
}

func (o *Operator) Status(ctx context.Context) (Status, error) {
	var s Status
	// Warn a full quota block before 90%, before a block allocation crosses it.
	err := o.tx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT maintenance,coalesce(maintenance_id::text,''),offline,clock_unreliable,coalesce(encode(active_key_fingerprint,'hex'),''),coalesce(restore_id::text,''),restore_reconciled,restore_key_required,coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'old',encode(old_fingerprint,'hex'),'new',encode(new_fingerprint,'hex'),'reserved',reserved_encryptions,'committed_at',committed_at) ORDER BY created_at) FROM cosmoformer.key_rotations),'[]') ,coalesce((SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=active_key_fingerprint),0),coalesce((SELECT reserved_encryptions>=3865466470 FROM cosmoformer.crypto_keys WHERE fingerprint=active_key_fingerprint),false),coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'cutoff',cutoff,'digest',encode(set_digest,'hex'),'count',artifact_count,'released_at',released_at) ORDER BY created_at) FROM cosmoformer.backup_sets),'[]') FROM cosmoformer.installation`).Scan(&s.Maintenance, &s.MaintenanceID, &s.Offline, &s.ClockUnreliable, &s.ActiveKey, &s.RestoreID, &s.RestoreReconciled, &s.RestoreKeyRequired, &s.Rotations, &s.ActiveReserved, &s.RotationRecommended, &s.BackupSets)
	})
	return s, err
}
func (o *Operator) Enter(ctx context.Context) error {
	return o.tx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET maintenance=true,maintenance_id=CASE WHEN maintenance AND maintenance_id IS NOT NULL THEN maintenance_id ELSE gen_random_uuid() END`)
		if err != nil {
			return err
		}
		var id string
		if err = tx.QueryRow(ctx, `SELECT maintenance_id::text FROM cosmoformer.installation`).Scan(&id); err != nil {
			return err
		}
		return event(ctx, tx, "maintenance.enter", Evidence{MaintenanceID: id})
	})
}
func maintenance(ctx context.Context, tx pgx.Tx, e Evidence) error {
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT maintenance AND maintenance_id::text=$1 FROM cosmoformer.installation`, e.MaintenanceID).Scan(&ok); err != nil {
		return err
	}
	if !ok || !catalog.UUID(e.MaintenanceID) || e.Reference == "" {
		return errors.New("maintenance identity and independent operator evidence required")
	}
	return nil
}
func verifyStops(ctx context.Context, tx pgx.Tx, e Evidence) error {
	if err := maintenance(ctx, tx, e); err != nil {
		return err
	}
	if !e.WritersStopped || !e.CleanupStopped {
		return errors.New("explicit stopped normal-writer and cleanup evidence required")
	}
	// This is a corroborating check, never the proof of stopped external writers.
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE datname=current_database() AND usename='cosmoformer_app')`).Scan(&active); err != nil {
		return err
	}
	if active {
		return errors.New("normal application database sessions remain; stop all normal writers")
	}
	seen := map[string]bool{}
	for _, w := range e.Workers {
		if !catalog.UUID(w.ID) || w.Reference == "" || w.State != "stopped" && w.State != "idle" || seen[w.ID] {
			return errors.New("invalid or duplicate worker stop evidence")
		}
		seen[w.ID] = true
	}
	return tenants(ctx, tx, func(_ string) error {
		rows, err := tx.Query(ctx, `SELECT id::text,generation,coalesce(agent_instance::text,''),EXISTS(SELECT FROM cosmoformer.attempts a WHERE a.worker_id=w.id AND a.closed_at IS NULL) FROM cosmoformer.workers w`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, agent string
			var gen int64
			var unresolved bool
			if err = rows.Scan(&id, &gen, &agent, &unresolved); err != nil {
				return err
			}
			found := false
			for _, w := range e.Workers {
				if w.ID == id && w.Generation == gen && w.Agent == agent {
					found = true
					if unresolved && w.State != "stopped" {
						return errors.New("unresolved or uncertain worker requires independent stopped-process evidence")
					}
				}
			}
			if !found {
				return errors.New("worker inventory incomplete or generation changed")
			}
		}
		return rows.Err()
	})
}

// Offline is latched durably before any encryption or filesystem handoff. A
// later failure never implicitly resumes writers or execution authority.
func (o *Operator) Offline(ctx context.Context, e Evidence) error {
	return o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET offline=true`); err != nil {
			return err
		}
		return event(ctx, tx, "offline.verify", e)
	})
}
func (o *Operator) Exit(ctx context.Context, e Evidence, key []byte) error {
	return o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT NOT clock_unreliable AND NOT restore_key_required AND (restore_id IS NULL OR restore_reconciled) AND active_key_fingerprint=$1 FROM cosmoformer.installation`, key).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return errors.New("resumption blocked: clock, key or restore reconciliation incomplete")
		}
		if err := event(ctx, tx, "maintenance.exit", e); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET maintenance=false,offline=false`)
		return err
	})
}
func invalidate(ctx context.Context, tx pgx.Tx, reason string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM cosmoformer.sessions; DELETE FROM cosmoformer.account_proofs; DELETE FROM cosmoformer.browser_requests`); err != nil {
		return err
	}
	return tenants(ctx, tx, func(_ string) error {
		if _, err := tx.Exec(ctx, `DELETE FROM cosmoformer.oauth_states`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE cosmoformer.plans SET status='invalidated',invalidation_reason=$1,revision=revision+1 WHERE status IN ('assembling','ready','recovery_only')`, reason)
		return err
	})
}
func (o *Operator) ClockReset(ctx context.Context, e Evidence, verified bool) error {
	if !verified || e.ClockValidation == "" {
		return errors.New("--verified and recorded independent UTC clock validation required")
	}
	if err := o.Offline(ctx, e); err != nil {
		return err
	}
	return o.tx(ctx, func(tx pgx.Tx) error {
		if err := verifyStops(ctx, tx, e); err != nil {
			return err
		}
		if err := invalidate(ctx, tx, "CLOCK_RESET"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cosmoformer.installation SET authority_highwater=clock_timestamp(),authority_platform_instance=NULL,authority_tick=NULL,clock_unreliable=false`); err != nil {
			return err
		}
		return event(ctx, tx, "clock.reset", e)
	})
}
