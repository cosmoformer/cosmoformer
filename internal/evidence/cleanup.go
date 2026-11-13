package evidence

import (
	"context"
	"errors"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

// CleanupSettings are deployment configuration, never browser-editable settings.
type CleanupSettings struct {
	Enabled         bool  `json:"enabled"`
	IntervalSeconds int64 `json:"interval_seconds"`
	RetentionDays   int64 `json:"retention_days"`
	Batch           int   `json:"batch"`
}

func DefaultCleanupSettings() CleanupSettings {
	return CleanupSettings{IntervalSeconds: 86400, RetentionDays: 30, Batch: 100}
}
func (s CleanupSettings) Validate() error {
	// Upper bounds keep conversions to time.Duration and PostgreSQL intervals safe.
	if s.IntervalSeconds < 3600 || s.IntervalSeconds > 315360000 || s.RetentionDays < 30 || s.RetentionDays > 365000 || s.Batch < 1 || s.Batch > 100 {
		return ErrInvalid
	}
	return nil
}

// DeletionStorage deliberately has no latest-version or bulk deletion operation.
// Keeping this extension separate lets read-only custody adapters remain read-only.
type DeletionStorage interface {
	Delete(context.Context, string, string) error
}

type CleanupItem struct {
	Artifact string `json:"artifact_id" format:"uuid"`
	Kind     string `json:"kind"`
	Outcome  string `json:"outcome" enum:"eligible,protected,unresolved,pending,failed,completed,attention"`
	Reason   string `json:"reason"`
	Claim    string `json:"claim_id,omitempty" format:"uuid"`
}
type CleanupPage struct {
	Items []CleanupItem `json:"items" nullable:"false"`
	Next  string        `json:"next_cursor"`
}

const cleanupProjection = `WITH candidates AS (
 SELECT a.id,a.kind,coalesce(c.claim_id::text,'') AS claim,
 CASE WHEN a.cleanup_attention IS NOT NULL THEN 'attention'
 WHEN c.status='deleted' THEN 'completed' WHEN c.status='failed' THEN 'failed' WHEN c.claim_id IS NOT NULL THEN 'pending'
 WHEN a.state IN ('deleted','unavailable') THEN 'failed'
 WHEN cosmoformer.cleanup_eligible(a.id,$1) THEN 'eligible'
 WHEN a.state='uploading' AND a.version_id IS NULL THEN 'unresolved' ELSE 'protected' END AS outcome,
 coalesce(a.cleanup_attention,c.error_code,CASE WHEN a.state='uploading' AND a.version_id IS NULL THEN 'EXACT_VERSION_UNRESOLVED' WHEN a.state='unavailable' THEN 'EVIDENCE_UNAVAILABLE' ELSE '' END) AS reason
 FROM cosmoformer.artifacts a LEFT JOIN cosmoformer.cleanup_claims c ON c.artifact_id=a.id
 ) SELECT id::text,kind,outcome,reason,claim FROM candidates
 WHERE ($2='' OR id>NULLIF($2,'')::uuid) AND ($3='' OR outcome=$3) ORDER BY id LIMIT $4`

// Preview is read-only, including hold release/repair. Filter before keyset paging.
func PreviewCleanup(ctx context.Context, tx pgx.Tx, retention int64, after, outcome string, limit int) (CleanupPage, error) {
	out := CleanupPage{Items: []CleanupItem{}}
	if retention < 30 || retention > 365000 || limit < 1 || limit > 100 {
		return out, ErrInvalid
	}
	rows, e := tx.Query(ctx, cleanupProjection, retention, after, outcome, limit+1)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var item CleanupItem
		if e = rows.Scan(&item.Artifact, &item.Kind, &item.Outcome, &item.Reason, &item.Claim); e != nil {
			return out, e
		}
		out.Items = append(out.Items, item)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	if len(out.Items) > limit {
		out.Items = out.Items[:limit]
		out.Next = out.Items[limit-1].Artifact
	}
	return out, nil
}

// ReconcileHolds must run under the coordination gate. Repair never resurrects
// claimed evidence; backup protection remains owner-only even when it is missing.
func ReconcileHolds(ctx context.Context, tx pgx.Tx, limit int) error {
	if limit < 1 || limit > 100 {
		return ErrInvalid
	}
	_, e := tx.Exec(ctx, `WITH broken AS (
 SELECT DISTINCT a.id FROM cosmoformer.required_artifact_holds() h JOIN cosmoformer.artifacts a ON a.id=h.artifact_id
 WHERE a.cleanup_attention IS NULL AND (a.state NOT IN ('uploading','finalized') OR EXISTS(SELECT FROM cosmoformer.cleanup_claims c WHERE c.artifact_id=a.id)
 OR h.owner_kind='backup' AND NOT EXISTS(SELECT FROM cosmoformer.artifact_holds x WHERE x.artifact_id=a.id AND x.owner_kind=h.owner_kind AND x.owner_id=h.owner_id AND x.released_at IS NULL)) ORDER BY a.id LIMIT $1
 ), changed AS (UPDATE cosmoformer.artifacts SET cleanup_attention='REQUIRED_EVIDENCE_PROTECTION_LOST' WHERE id IN(SELECT id FROM broken) RETURNING org_id,id)
 INSERT INTO cosmoformer.audit_events(org_id,action,subject_id) SELECT org_id,'cleanup.attention',id FROM changed`, limit)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason)
 SELECT a.org_id,a.id,h.owner_kind,h.owner_id,'consistency_repair' FROM cosmoformer.required_artifact_holds() h JOIN cosmoformer.artifacts a ON a.id=h.artifact_id
 WHERE h.owner_kind<>'backup' AND a.state IN ('uploading','finalized') AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims c WHERE c.artifact_id=a.id)
 AND NOT EXISTS(SELECT FROM cosmoformer.artifact_holds x WHERE x.artifact_id=a.id AND x.owner_kind=h.owner_kind AND x.owner_id=h.owner_id AND x.released_at IS NULL)
 ORDER BY a.id,h.owner_kind,h.owner_id LIMIT $1 ON CONFLICT DO NOTHING`, limit)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `WITH stale AS (
 SELECT h.id FROM cosmoformer.artifact_holds h WHERE h.released_at IS NULL AND h.owner_kind<>'backup'
 AND NOT EXISTS(SELECT FROM cosmoformer.required_artifact_holds() r WHERE r.artifact_id=h.artifact_id AND r.owner_kind=h.owner_kind AND r.owner_id=h.owner_id)
 ORDER BY h.id LIMIT $1)
 UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE id IN(SELECT id FROM stale)`, limit)
	return e
}

// ClaimCleanup rechecks the complete use matrix in the same gated transaction
// as the immutable claim and deleting transition. Resumptions keep that claim.
func ClaimCleanup(ctx context.Context, tx pgx.Tx, settings CleanupSettings, limit int) ([]string, error) {
	if settings.Validate() != nil || limit < 1 || limit > settings.Batch {
		return nil, ErrInvalid
	}
	var paused bool
	if e := tx.QueryRow(ctx, `SELECT maintenance OR offline OR clock_unreliable FROM cosmoformer.installation`).Scan(&paused); e != nil {
		return nil, e
	}
	if paused {
		return nil, ErrUnavailable
	}
	if e := ReconcileHolds(ctx, tx, limit); e != nil {
		return nil, e
	}
	rows, e := tx.Query(ctx, `SELECT claim_id::text FROM cosmoformer.cleanup_claims WHERE completed_at IS NULL AND (retry_at IS NULL OR retry_at<=clock_timestamp()) ORDER BY claimed_at,claim_id LIMIT $1`, limit)
	if e != nil {
		return nil, e
	}
	claims := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		claims = append(claims, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	if len(claims) == limit {
		return claims, nil
	}
	page, e := PreviewCleanup(ctx, tx, settings.RetentionDays, "", "eligible", limit-len(claims))
	if e != nil {
		return nil, e
	}
	for _, item := range page.Items {
		id := runs.ID()
		tag, e := tx.Exec(ctx, `INSERT INTO cosmoformer.cleanup_claims(org_id,artifact_id,exact_version,claim_id,claimed_at)
 SELECT org_id,id,version_id,$2,clock_timestamp() FROM cosmoformer.artifacts WHERE id=$1 AND cosmoformer.cleanup_eligible(id,$3)`, item.Artifact, id, settings.RetentionDays)
		if e != nil {
			return nil, e
		}
		if tag.RowsAffected() != 1 {
			continue
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.artifacts SET state='deleting' WHERE id=$1`, item.Artifact); e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.audit_events(org_id,action,subject_id) SELECT org_id,'cleanup.claimed',id FROM cosmoformer.artifacts WHERE id=$1`, item.Artifact); e != nil {
			return nil, e
		}
		claims = append(claims, id)
	}
	return claims, nil
}

// DeleteClaim uses short authorized transactions around storage I/O. The durable
// delete intent is recorded only after exact-version presence/identity verification.
// On a lost response/restart it repeats Delete against that same immutable version.
var cleanupIO = make(chan struct{}, 1)

func (c *Custody) DeleteClaim(ctx context.Context, tx Transaction, claim string) error {
	select {
	case cleanupIO <- struct{}{}:
		defer func() { <-cleanupIO }()
	default:
		return ErrUnavailable
	}
	var a Artifact
	var intent, completed, attention, knownSize bool
	e := tx(func(q pgx.Tx) error {
		var paused bool
		if e := q.QueryRow(ctx, `SELECT maintenance OR offline FROM cosmoformer.installation`).Scan(&paused); e != nil {
			return e
		}
		if paused {
			return ErrUnavailable
		}
		var e error
		e = q.QueryRow(ctx, `SELECT a.id::text,a.kind,c.bucket,c.object_key,c.exact_version,coalesce(encode(a.sha256,'hex'),''),coalesce(a.byte_count,0),a.state,c.delete_intent_at IS NOT NULL,c.completed_at IS NOT NULL,a.cleanup_attention IS NOT NULL,a.byte_count IS NOT NULL
 FROM cosmoformer.cleanup_claims c JOIN cosmoformer.artifacts a ON a.id=c.artifact_id WHERE c.claim_id=$1 AND (c.bucket,c.object_key,c.exact_version)=(a.bucket,a.object_key,a.version_id)`, claim).Scan(&a.ID, &a.Kind, &a.Bucket, &a.Key, &a.Version, &a.Hash, &a.Size, &a.State, &intent, &completed, &attention, &knownSize)
		return e
	})
	if e != nil || completed {
		return e
	}
	if attention {
		return ErrUnavailable
	}
	if a.State != "deleting" {
		return cleanupFailure(ctx, tx, claim, "CLAIM_STATE_MISMATCH", false)
	}
	if c == nil || c.Storage == nil || a.Bucket != c.Bucket || a.Key == "" || !validVersion(a.Version) {
		return cleanupFailure(ctx, tx, claim, "STORAGE_IDENTITY_MISMATCH", false)
	}
	storage, ok := c.Storage.(DeletionStorage)
	if !ok {
		return cleanupFailure(ctx, tx, claim, "STORAGE_DELETE_UNAVAILABLE", false)
	}
	if e = c.Storage.Check(ctx); e != nil {
		return cleanupFailure(ctx, tx, claim, "STORAGE_UNAVAILABLE", false)
	}
	if !intent {
		object, e := c.Storage.Get(ctx, a.Key, a.Version)
		if e != nil {
			return cleanupFailure(ctx, tx, claim, func() string {
				if errors.Is(e, ErrInvalid) {
					return "MISSING_VERSION"
				}
				return "STORAGE_UNAVAILABLE"
			}(), errors.Is(e, ErrInvalid))
		}
		if object.Body == nil {
			return cleanupFailure(ctx, tx, claim, "STORAGE_IDENTITY_MISMATCH", false)
		}
		object.Body.Close()
		if object.Version != a.Version || knownSize && object.Size != a.Size || object.Encryption != "AES256" {
			return cleanupFailure(ctx, tx, claim, "STORAGE_IDENTITY_MISMATCH", false)
		}
	}
	admitted, e := admitDeletion(ctx, tx, claim)
	if e != nil {
		return e
	}
	if !admitted {
		return ErrUnavailable
	}
	if e = storage.Delete(ctx, a.Key, a.Version); e != nil {
		return cleanupFailure(ctx, tx, claim, "STORAGE_DELETE_UNCONFIRMED", false)
	}
	return tx(func(q pgx.Tx) error {
		tag, e := q.Exec(ctx, `UPDATE cosmoformer.cleanup_claims SET status='deleted',completed_at=clock_timestamp(),retry_at=NULL,error_code=NULL WHERE claim_id=$1 AND completed_at IS NULL`, claim)
		if e != nil || tag.RowsAffected() == 0 {
			return e
		}
		_, e = q.Exec(ctx, `UPDATE cosmoformer.artifacts SET state='deleted',deleted_at=clock_timestamp(),tombstone=jsonb_build_object('claim_id',$2::text,'outcome','exact_version_deleted') WHERE id=$1`, a.ID, claim)
		if e != nil {
			return e
		}
		_, e = q.Exec(ctx, `INSERT INTO cosmoformer.audit_events(org_id,action,subject_id) SELECT org_id,'cleanup.deleted',artifact_id FROM cosmoformer.cleanup_claims WHERE claim_id=$1`, claim)
		return e
	})
}

// Recheck at the last storage admission point, after preflight I/O. A scan or
// operator reconciliation may have recorded lost protection while it was running.
// Commit attention before returning refusal; never roll it back with an error.
func admitDeletion(ctx context.Context, tx Transaction, claim string) (bool, error) {
	admitted := false
	e := tx(func(q pgx.Tx) error {
		var paused bool
		if e := q.QueryRow(ctx, `SELECT maintenance OR offline FROM cosmoformer.installation`).Scan(&paused); e != nil {
			return e
		}
		if paused {
			return ErrUnavailable
		}
		var completed, blocked bool
		e := q.QueryRow(ctx, `SELECT c.completed_at IS NOT NULL,
 a.state<>'deleting' OR a.cleanup_attention IS NOT NULL OR EXISTS(SELECT FROM cosmoformer.required_artifact_holds() h WHERE h.artifact_id=a.id)
 FROM cosmoformer.cleanup_claims c JOIN cosmoformer.artifacts a ON a.id=c.artifact_id
 WHERE c.claim_id=$1 AND (c.bucket,c.object_key,c.exact_version)=(a.bucket,a.object_key,a.version_id)`, claim).Scan(&completed, &blocked)
		if e != nil || completed {
			return e
		}
		if blocked {
			_, e = q.Exec(ctx, `WITH changed AS (
 UPDATE cosmoformer.artifacts SET cleanup_attention='REQUIRED_EVIDENCE_PROTECTION_LOST'
 WHERE id=(SELECT artifact_id FROM cosmoformer.cleanup_claims WHERE claim_id=$1) AND cleanup_attention IS NULL RETURNING org_id,id)
 INSERT INTO cosmoformer.audit_events(org_id,action,subject_id) SELECT org_id,'cleanup.attention',id FROM changed`, claim)
			return e
		}
		tag, e := q.Exec(ctx, `UPDATE cosmoformer.cleanup_claims SET delete_intent_at=coalesce(delete_intent_at,clock_timestamp()),storage_attempts=storage_attempts+1 WHERE claim_id=$1 AND completed_at IS NULL`, claim)
		admitted = e == nil && tag.RowsAffected() == 1
		return e
	})
	return admitted, e
}

func cleanupFailure(ctx context.Context, tx Transaction, claim, code string, missing bool) error {
	e := tx(func(q pgx.Tx) error {
		tag, e := q.Exec(ctx, `UPDATE cosmoformer.cleanup_claims SET status='failed',error_code=$2,retry_at=CASE WHEN $3 THEN NULL ELSE clock_timestamp()+interval '5 minutes' END,completed_at=CASE WHEN $3 THEN clock_timestamp() ELSE NULL END WHERE claim_id=$1 AND completed_at IS NULL`, claim, code, missing)
		if e != nil || tag.RowsAffected() == 0 {
			return e
		}
		if missing {
			if _, e = q.Exec(ctx, `UPDATE cosmoformer.artifacts SET state='unavailable',tombstone=jsonb_build_object('claim_id',$1::text,'outcome','unexpected_missing_version') WHERE id=(SELECT artifact_id FROM cosmoformer.cleanup_claims WHERE claim_id=$1)`, claim); e != nil {
				return e
			}
		}
		_, e = q.Exec(ctx, `INSERT INTO cosmoformer.audit_events(org_id,action,subject_id,facts) SELECT org_id,'cleanup.failed',artifact_id,jsonb_build_object('code',$2::text) FROM cosmoformer.cleanup_claims WHERE claim_id=$1`, claim, code)
		return e
	})
	if e != nil {
		return e
	}
	return ErrUnavailable
}

const CleanupRunTimeout = 20 * time.Second

// PendingCleanup keeps a replay inside its original receipt and the current
// execution bound. Completed claims cannot consume the bound on every replay.
func PendingCleanup(ctx context.Context, tx pgx.Tx, claims []string, limit int) ([]string, error) {
	out := []string{}
	if limit < 1 || limit > 100 || len(claims) > 100 {
		return out, ErrInvalid
	}
	rows, e := tx.Query(ctx, `SELECT claim_id::text FROM cosmoformer.cleanup_claims WHERE claim_id=ANY($1::uuid[]) AND completed_at IS NULL ORDER BY claimed_at,claim_id LIMIT $2`, claims, limit)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return out, e
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
