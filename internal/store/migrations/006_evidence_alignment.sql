SET search_path = cosmoformer, pg_catalog;
-- Old evidence has no bucket identity or installation capture metadata. Never
-- invent either or silently discard protection. This runs as FORCE-RLS owner;
-- explicitly visit each tenant before checking for legacy evidence.
DO $$ DECLARE tenant uuid; BEGIN
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  IF EXISTS(SELECT FROM artifacts) OR EXISTS(SELECT FROM backup_sets) THEN
   RAISE EXCEPTION 'EVIDENCE_METADATA_RECONCILIATION_REQUIRED: version-5 evidence requires verified bucket/capture metadata before upgrade';
  END IF;
 END LOOP;
 PERFORM set_config('app.org_id','',true);
END $$;
ALTER TABLE artifacts DROP CONSTRAINT artifacts_state_check;
ALTER TABLE artifacts
 ADD COLUMN bucket text COLLATE "C" NOT NULL CHECK(bucket<>''),
 ADD COLUMN tombstone jsonb,
 ALTER COLUMN version_id DROP NOT NULL,
 ALTER COLUMN sha256 DROP NOT NULL,
 ALTER COLUMN byte_count DROP NOT NULL,
 ADD CONSTRAINT artifacts_state_check CHECK(state IN ('uploading','finalized','deleting','deleted','unavailable')),
 ADD CONSTRAINT artifacts_exact_version CHECK(version_id IS NULL OR (version_id<>'' AND version_id<>'null')),
 ADD CONSTRAINT artifacts_finalized_identity CHECK((state<>'finalized' AND finalized_at IS NULL) OR
   (version_id IS NOT NULL AND sha256 IS NOT NULL AND byte_count IS NOT NULL AND finalized_at IS NOT NULL)),
 ADD CONSTRAINT artifacts_deletion_version CHECK(state NOT IN ('deleting','deleted') OR version_id IS NOT NULL),
 ADD CONSTRAINT artifacts_deleted_evidence CHECK(state<>'deleted' OR (deleted_at IS NOT NULL AND tombstone IS NOT NULL));
-- No backup records can exist after the explicit reconciliation guard above.
ALTER TABLE backup_sets DROP CONSTRAINT backup_sets_org_id_fkey;
DROP POLICY tenant_scope ON backup_sets;
ALTER TABLE backup_sets DISABLE ROW LEVEL SECURITY;
REVOKE ALL ON backup_sets FROM cosmoformer_app;
ALTER TABLE backup_sets DROP COLUMN org_id, DROP COLUMN label, DROP COLUMN state,
 DROP COLUMN manifest_digest, DROP COLUMN sealed_at, DROP COLUMN revision,
 ADD COLUMN cutoff timestamptz NOT NULL,
 ADD COLUMN set_digest bytea NOT NULL CHECK(octet_length(set_digest)=32),
 ADD COLUMN artifact_count bigint NOT NULL CHECK(artifact_count>=0);
ALTER TABLE artifact_holds DROP CONSTRAINT artifact_holds_purpose_check;
ALTER TABLE artifact_holds RENAME COLUMN purpose TO owner_kind;
ALTER TABLE artifact_holds RENAME COLUMN subject_id TO owner_id;
ALTER TABLE artifact_holds ADD COLUMN reason text NOT NULL CHECK(reason<>''),
 ADD CONSTRAINT artifact_holds_owner_kind CHECK(owner_kind IN ('plan','operation','attempt','recovery','backup')),
 ADD COLUMN plan_owner_id uuid GENERATED ALWAYS AS (CASE WHEN owner_kind='plan' THEN owner_id END) STORED,
 ADD COLUMN operation_owner_id uuid GENERATED ALWAYS AS (CASE WHEN owner_kind='operation' THEN owner_id END) STORED,
 ADD COLUMN attempt_owner_id uuid GENERATED ALWAYS AS (CASE WHEN owner_kind='attempt' THEN owner_id END) STORED,
 ADD COLUMN recovery_owner_id uuid GENERATED ALWAYS AS (CASE WHEN owner_kind='recovery' THEN owner_id END) STORED,
 ADD COLUMN backup_owner_id uuid GENERATED ALWAYS AS (CASE WHEN owner_kind='backup' THEN owner_id END) STORED,
 ADD FOREIGN KEY(org_id,plan_owner_id) REFERENCES plans(org_id,id),
 ADD FOREIGN KEY(org_id,operation_owner_id) REFERENCES operations(org_id,id),
 ADD FOREIGN KEY(org_id,attempt_owner_id) REFERENCES attempts(org_id,id),
 ADD FOREIGN KEY(org_id,recovery_owner_id) REFERENCES recovery_cases(org_id,id),
 ADD FOREIGN KEY(backup_owner_id) REFERENCES backup_sets(id);
-- Replace the earlier owner uniqueness with reason-aware active protection.
DROP INDEX artifact_holds_org_id_artifact_id_purpose_subject_id_idx;
CREATE UNIQUE INDEX artifact_holds_active_owner ON artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) WHERE released_at IS NULL;
CREATE INDEX artifact_holds_owner ON artifact_holds(org_id,owner_kind,owner_id);
CREATE INDEX artifact_holds_plan_fk ON artifact_holds(org_id,plan_owner_id) WHERE plan_owner_id IS NOT NULL;
CREATE INDEX artifact_holds_operation_fk ON artifact_holds(org_id,operation_owner_id) WHERE operation_owner_id IS NOT NULL;
CREATE INDEX artifact_holds_attempt_fk ON artifact_holds(org_id,attempt_owner_id) WHERE attempt_owner_id IS NOT NULL;
CREATE INDEX artifact_holds_recovery_fk ON artifact_holds(org_id,recovery_owner_id) WHERE recovery_owner_id IS NOT NULL;
CREATE INDEX artifact_holds_backup_fk ON artifact_holds(backup_owner_id) WHERE backup_owner_id IS NOT NULL;
CREATE INDEX artifacts_retention ON artifacts(org_id,state,created_at,id);
CREATE INDEX artifacts_plan ON artifacts(org_id,plan_id) WHERE plan_id IS NOT NULL;
-- A preparation retry replaces an assembling plan's current attempt. Historical
-- artifacts retain their original attempt and plan association through the
-- stable owning operation, rather than constraining the plan's current attempt.
ALTER TABLE attempts ADD UNIQUE(org_id,operation_id,id);
ALTER TABLE plans ADD UNIQUE(org_id,operation_id,id);
ALTER TABLE artifacts ADD COLUMN operation_id uuid NOT NULL,
 ADD FOREIGN KEY(org_id,operation_id,attempt_id) REFERENCES attempts(org_id,operation_id,id),
 ADD FOREIGN KEY(org_id,operation_id,plan_id) REFERENCES plans(org_id,operation_id,id);
CREATE INDEX artifacts_attempt_operation ON artifacts(org_id,operation_id,attempt_id);
CREATE INDEX artifacts_plan_operation ON artifacts(org_id,operation_id,plan_id) WHERE plan_id IS NOT NULL;
ALTER TABLE cleanup_claims ADD COLUMN status text NOT NULL DEFAULT 'claimed' CHECK(status IN ('claimed','deleted','failed')),
 ADD COLUMN retry_at timestamptz, ADD COLUMN error_code text,
 ADD CONSTRAINT cleanup_exact_version CHECK(exact_version<>'' AND exact_version<>'null');
-- Artifact identity is the authority for the exact bucket/key/version; the FK
-- prevents a claim from naming a different version without duplicating storage.
ALTER TABLE artifacts ADD UNIQUE(org_id,id,version_id);
ALTER TABLE cleanup_claims ADD FOREIGN KEY(org_id,artifact_id,exact_version) REFERENCES artifacts(org_id,id,version_id);
CREATE INDEX cleanup_claims_retry ON cleanup_claims(org_id,status,retry_at) WHERE completed_at IS NULL;
-- Backup holds are host-owner writes only; scoped runtime reads remain useful
-- for eligibility checks. No global capture helper is exposed to the runtime.
CREATE POLICY backup_holds_insert ON artifact_holds AS RESTRICTIVE FOR INSERT
 WITH CHECK(owner_kind<>'backup' OR current_user='cosmoformer_owner');
CREATE POLICY backup_holds_update ON artifact_holds AS RESTRICTIVE FOR UPDATE
 USING(owner_kind<>'backup' OR current_user='cosmoformer_owner')
 WITH CHECK(owner_kind<>'backup' OR current_user='cosmoformer_owner');
CREATE POLICY backup_holds_delete ON artifact_holds AS RESTRICTIVE FOR DELETE
 USING(owner_kind<>'backup' OR current_user='cosmoformer_owner');
-- Triggers serialize evidence mutations with the existing shared gate. They
-- enforce identity immutability and prevent new holds after a deletion claim;
-- eligibility/automatic hold acquisition belongs to the later workflows.
CREATE FUNCTION evidence_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'evidence history is retained' USING ERRCODE='23514'; END IF;
 IF TG_OP<>'DELETE' AND NEW.org_id IS DISTINCT FROM nullif(current_setting('app.org_id',true),'')::uuid THEN
  RAISE EXCEPTION 'tenant scope required' USING ERRCODE='42501';
 END IF;
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_TABLE_NAME='artifacts' THEN
  IF TG_OP='INSERT' THEN
   -- Derive provenance from the tenant-visible attempt; composite FKs also
   -- reject a caller-supplied operation that does not own that exact attempt.
   IF NEW.operation_id IS NULL THEN
    SELECT operation_id INTO NEW.operation_id FROM attempts WHERE org_id=NEW.org_id AND id=NEW.attempt_id;
    IF NOT FOUND THEN
     RAISE EXCEPTION 'artifact attempt not found' USING ERRCODE='23503';
    END IF;
   END IF;
   RETURN NEW;
  END IF;
  IF (OLD.finalized_at IS NOT NULL OR OLD.state IN ('deleting','deleted') OR
      EXISTS(SELECT FROM cleanup_claims WHERE org_id=OLD.org_id AND artifact_id=OLD.id)) AND
   (NEW.id,NEW.org_id,NEW.operation_id,NEW.attempt_id,NEW.plan_id,NEW.kind,NEW.upload_id,NEW.bucket,NEW.object_key,NEW.version_id,NEW.sha256,NEW.byte_count,NEW.finalized_at)
   IS DISTINCT FROM
   (OLD.id,OLD.org_id,OLD.operation_id,OLD.attempt_id,OLD.plan_id,OLD.kind,OLD.upload_id,OLD.bucket,OLD.object_key,OLD.version_id,OLD.sha256,OLD.byte_count,OLD.finalized_at) THEN
   RAISE EXCEPTION 'finalized or claimed artifact identity is immutable' USING ERRCODE='23514';
  END IF;
  IF OLD.state<>NEW.state AND NOT
   ((OLD.state='uploading' AND NEW.state IN ('finalized','deleting','unavailable')) OR
    (OLD.state='finalized' AND NEW.state IN ('deleting','unavailable')) OR
    (OLD.state='deleting' AND NEW.state IN ('deleted','unavailable'))) THEN
   RAISE EXCEPTION 'invalid artifact lifecycle transition' USING ERRCODE='23514';
  END IF;
  IF OLD.state='uploading' AND NEW.state='deleting' AND
   (NOT EXISTS(SELECT FROM attempts WHERE org_id=OLD.org_id AND id=OLD.attempt_id AND closed_at IS NOT NULL) OR
    NOT EXISTS(SELECT FROM cleanup_claims WHERE org_id=OLD.org_id AND artifact_id=OLD.id AND exact_version=OLD.version_id)) THEN
   RAISE EXCEPTION 'orphan cleanup requires a closed attempt and exact-version claim' USING ERRCODE='23514';
  END IF;
  IF NEW.state IN ('deleting','deleted') AND EXISTS(SELECT FROM artifact_holds WHERE org_id=OLD.org_id AND artifact_id=OLD.id AND released_at IS NULL) THEN
   RAISE EXCEPTION 'artifact is held' USING ERRCODE='23514';
  END IF;
 ELSIF TG_TABLE_NAME='artifact_holds' THEN
  IF NEW.released_at IS NULL AND
   (NOT EXISTS(SELECT FROM artifacts WHERE org_id=NEW.org_id AND id=NEW.artifact_id AND state IN ('uploading','finalized')) OR
    EXISTS(SELECT FROM cleanup_claims WHERE org_id=NEW.org_id AND artifact_id=NEW.artifact_id)) THEN
   RAISE EXCEPTION 'evidence unavailable' USING ERRCODE='23514';
  END IF;
 ELSE
  IF TG_OP='UPDATE' AND (NEW.org_id,NEW.artifact_id,NEW.exact_version,NEW.claim_id,NEW.claimed_at)
    IS DISTINCT FROM (OLD.org_id,OLD.artifact_id,OLD.exact_version,OLD.claim_id,OLD.claimed_at) THEN
   RAISE EXCEPTION 'cleanup claim identity is immutable' USING ERRCODE='23514';
  END IF;
  IF EXISTS(SELECT FROM artifact_holds WHERE org_id=NEW.org_id AND artifact_id=NEW.artifact_id AND released_at IS NULL) THEN
   RAISE EXCEPTION 'artifact is held' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
REVOKE ALL ON FUNCTION evidence_integrity() FROM PUBLIC;
CREATE TRIGGER artifacts_integrity BEFORE INSERT OR UPDATE OR DELETE ON artifacts FOR EACH ROW EXECUTE FUNCTION evidence_integrity();
CREATE TRIGGER artifact_holds_integrity BEFORE INSERT OR UPDATE ON artifact_holds FOR EACH ROW EXECUTE FUNCTION evidence_integrity();
CREATE TRIGGER cleanup_claims_integrity BEFORE INSERT OR UPDATE OR DELETE ON cleanup_claims FOR EACH ROW EXECUTE FUNCTION evidence_integrity();
ALTER TABLE log_segments ADD COLUMN gap_reason text, ADD COLUMN unknown_extent boolean NOT NULL DEFAULT false,
 ADD CONSTRAINT log_segments_evidence_or_gap CHECK(
  (gap AND artifact_id IS NULL AND gap_reason IS NOT NULL AND gap_reason<>'') OR
  (NOT gap AND artifact_id IS NOT NULL AND gap_reason IS NULL AND NOT unknown_extent));
ALTER TABLE log_segments ADD FOREIGN KEY(org_id,attempt_id,artifact_id) REFERENCES artifacts(org_id,attempt_id,id);
