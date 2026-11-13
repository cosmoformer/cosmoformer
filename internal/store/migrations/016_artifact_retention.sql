SET search_path=pg_catalog,cosmoformer,pg_temp;
-- Existing terminal operations start a conservative retention window at upgrade.
ALTER TABLE cosmoformer.operations ADD COLUMN terminal_at timestamptz;
-- Backfill only the new age fact. Do not enqueue PR delivery or release old
-- holds as a side effect of a metadata-only update. DDL and backfill are atomic.
ALTER TABLE cosmoformer.operations DISABLE TRIGGER queue_pr_feedback;
ALTER TABLE cosmoformer.operations DISABLE TRIGGER release_operation_holds;
DO $$ DECLARE tenant uuid; previous text:=current_setting('app.org_id',true); BEGIN
 FOR tenant IN SELECT id FROM cosmoformer.organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE cosmoformer.operations SET terminal_at=clock_timestamp() WHERE status IN ('succeeded','failed','cancelled','superseded','retired');
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
END $$;
ALTER TABLE cosmoformer.operations ENABLE TRIGGER queue_pr_feedback;
ALTER TABLE cosmoformer.operations ENABLE TRIGGER release_operation_holds;
CREATE FUNCTION cosmoformer.operation_terminal_time() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF NEW.status IN ('succeeded','failed','cancelled','superseded','retired') THEN
  IF TG_OP='INSERT' OR OLD.status NOT IN ('succeeded','failed','cancelled','superseded','retired') THEN NEW.terminal_at:=clock_timestamp();
  ELSE NEW.terminal_at:=OLD.terminal_at; END IF;
 ELSE NEW.terminal_at:=NULL; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER operation_terminal_time BEFORE INSERT OR UPDATE ON cosmoformer.operations FOR EACH ROW EXECUTE FUNCTION cosmoformer.operation_terminal_time();
ALTER TABLE cosmoformer.artifacts ADD COLUMN cleanup_attention text;
ALTER TABLE cosmoformer.cleanup_claims ADD COLUMN bucket text COLLATE "C", ADD COLUMN object_key text COLLATE "C",
 ADD COLUMN delete_intent_at timestamptz, ADD COLUMN storage_attempts integer NOT NULL DEFAULT 0 CHECK(storage_attempts>=0);
DO $$ DECLARE tenant uuid; previous text:=current_setting('app.org_id',true); BEGIN
 FOR tenant IN SELECT id FROM cosmoformer.organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE cosmoformer.cleanup_claims c SET bucket=a.bucket,object_key=a.object_key FROM cosmoformer.artifacts a WHERE a.id=c.artifact_id AND a.org_id=c.org_id;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
END $$;
ALTER TABLE cosmoformer.cleanup_claims ALTER COLUMN bucket SET NOT NULL, ALTER COLUMN object_key SET NOT NULL;
CREATE FUNCTION cosmoformer.cleanup_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE a artifacts;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP='INSERT' THEN
  SELECT * INTO a FROM artifacts WHERE id=NEW.artifact_id AND org_id=NEW.org_id;
  NEW.bucket:=coalesce(NEW.bucket,a.bucket); NEW.object_key:=coalesce(NEW.object_key,a.object_key);
  IF (NEW.bucket,NEW.object_key) IS DISTINCT FROM (a.bucket,a.object_key) THEN RAISE EXCEPTION 'cleanup identity mismatch' USING ERRCODE='23514'; END IF;
 ELSE
  IF (NEW.id,NEW.bucket,NEW.object_key,NEW.created_at) IS DISTINCT FROM (OLD.id,OLD.bucket,OLD.object_key,OLD.created_at) OR
    OLD.delete_intent_at IS NOT NULL AND NEW.delete_intent_at IS DISTINCT FROM OLD.delete_intent_at OR
    OLD.completed_at IS NOT NULL AND (NEW.status,NEW.completed_at,NEW.error_code) IS DISTINCT FROM (OLD.status,OLD.completed_at,OLD.error_code) THEN
   RAISE EXCEPTION 'cleanup identity/outcome immutable' USING ERRCODE='23514';
  END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cleanup_identity BEFORE INSERT OR UPDATE ON cosmoformer.cleanup_claims FOR EACH ROW EXECUTE FUNCTION cosmoformer.cleanup_identity();
-- Only the host owner can inspect/repair backup manifests. Runtime receives the
-- minimal tenant-scoped protection references, never storage identities or keys.
CREATE FUNCTION cosmoformer.required_backup_holds() RETURNS TABLE(artifact_id uuid,owner_id uuid) LANGUAGE sql STABLE SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
 SELECT a.artifact_id,a.backup_id FROM backup_artifacts a JOIN backup_sets b ON b.id=a.backup_id
 WHERE a.org_id=nullif(current_setting('app.org_id',true),'')::uuid AND b.released_at IS NULL
$$;
CREATE FUNCTION cosmoformer.required_artifact_holds() RETURNS TABLE(artifact_id uuid,owner_kind text,owner_id uuid) LANGUAGE sql STABLE SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
 SELECT a.id,'plan',p.id FROM artifacts a JOIN plans p ON p.id=a.plan_id
 WHERE (p.status='ready' AND p.expires_at>statement_timestamp() OR p.status='assembling' AND EXISTS(SELECT FROM attempts t WHERE t.operation_id=p.operation_id AND t.closed_at IS NULL)) AND a.kind<>'log_segment'
 UNION
 SELECT a.id,'attempt',t.id FROM artifacts a JOIN attempts t ON t.id=a.attempt_id OR
  t.closed_at IS NULL AND (t.execution_spec->'retained_plan'->'artifact_ids' ? a.id::text OR EXISTS(SELECT FROM operations o WHERE o.id=t.operation_id AND o.plan_id=a.plan_id))
 WHERE t.closed_at IS NULL
 UNION
 SELECT a.id,'operation',o.id FROM artifacts a JOIN operations o ON o.id=a.operation_id OR o.plan_id=a.plan_id
 WHERE o.status NOT IN ('succeeded','failed','cancelled','superseded','retired')
 UNION
 SELECT a.id,'recovery',r.id FROM artifacts a JOIN attempts t ON t.id=a.attempt_id JOIN recovery_cases r ON r.target_id=t.target_id
 WHERE (r.status='open' OR EXISTS(SELECT FROM attempts u JOIN operations o ON o.id=u.operation_id WHERE o.recovery_case_id=r.id AND u.closed_at IS NULL))
 -- Opening a later case copies AVAILABLE history; it cannot retroactively turn
 -- a legitimately completed archival deletion into a required evidence use.
 AND (a.state IN ('uploading','finalized') OR
 EXISTS(SELECT FROM artifact_holds h WHERE h.artifact_id=a.id AND h.owner_kind='recovery' AND h.owner_id=r.id AND h.released_at IS NULL) OR
 EXISTS(SELECT FROM cleanup_claims c WHERE c.artifact_id=a.id AND c.claimed_at>=r.opened_at))
 UNION SELECT artifact_id,'backup',owner_id FROM required_backup_holds()
$$;
-- A single predicate serves preview and the guarded claim recheck. No hold
-- release, reservation release or stopped-process inference occurs here.
CREATE FUNCTION cosmoformer.cleanup_eligible(subject uuid,retention_days bigint) RETURNS boolean LANGUAGE sql STABLE SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
 SELECT retention_days>=30 AND EXISTS(
 SELECT FROM artifacts a JOIN attempts t ON t.id=a.attempt_id JOIN operations o ON o.id=a.operation_id
 WHERE a.id=subject AND a.state IN ('finalized','uploading') AND a.version_id IS NOT NULL AND a.version_id NOT IN ('','null') AND a.cleanup_attention IS NULL
 AND t.closed_at IS NOT NULL AND o.status IN ('succeeded','failed','cancelled','superseded','retired') AND o.terminal_at IS NOT NULL
 AND greatest(t.closed_at,CASE WHEN a.state='uploading' THEN t.closed_at ELSE o.terminal_at END)<=statement_timestamp()-retention_days*interval '1 day'
 AND NOT EXISTS(SELECT FROM attempts u WHERE u.operation_id=o.id AND u.closed_at IS NULL)
 AND NOT EXISTS(SELECT FROM artifact_holds h WHERE h.artifact_id=a.id AND h.released_at IS NULL)
 AND NOT EXISTS(SELECT FROM required_artifact_holds() h WHERE h.artifact_id=a.id)
 AND NOT EXISTS(SELECT FROM cleanup_claims c WHERE c.artifact_id=a.id))
$$;
CREATE FUNCTION cosmoformer.artifact_start_blocked(subject uuid) RETURNS boolean LANGUAGE sql STABLE SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
 SELECT EXISTS(SELECT FROM artifacts a JOIN attempts t ON t.id=a.attempt_id WHERE t.stack_id=subject AND
 (a.cleanup_attention IS NOT NULL OR EXISTS(SELECT FROM required_artifact_holds() h WHERE h.artifact_id=a.id AND
 (a.state IN ('deleting','deleted','unavailable') OR EXISTS(SELECT FROM cleanup_claims c WHERE c.artifact_id=a.id)))))
$$;
-- Bounded tenant discovery includes tenants without enabled repositories.
CREATE FUNCTION cosmoformer.cleanup_tenants(after_id uuid) RETURNS SETOF uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE tenant uuid; previous text:=current_setting('app.org_id',true);
BEGIN
 FOR tenant IN SELECT id FROM organizations WHERE after_id IS NULL OR id>after_id ORDER BY id LIMIT 100 LOOP
  RETURN NEXT tenant;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
END $$;
REVOKE ALL ON FUNCTION cosmoformer.operation_terminal_time(),cosmoformer.cleanup_identity(),cosmoformer.required_backup_holds(),cosmoformer.required_artifact_holds(),cosmoformer.cleanup_eligible(uuid,bigint),cosmoformer.artifact_start_blocked(uuid),cosmoformer.cleanup_tenants(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cosmoformer.required_backup_holds(),cosmoformer.required_artifact_holds(),cosmoformer.cleanup_eligible(uuid,bigint),cosmoformer.artifact_start_blocked(uuid),cosmoformer.cleanup_tenants(uuid) TO cosmoformer_app;

-- Runs before the ordinary AFTER-insert plan invalidation. The claim is already
-- visible, so an attention-only artifact update cannot acquire a new hold.
CREATE FUNCTION cosmoformer.cleanup_reference_attention() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF EXISTS(SELECT FROM required_artifact_holds() h WHERE h.artifact_id=NEW.artifact_id) THEN
  WITH changed AS (UPDATE artifacts SET cleanup_attention='REQUIRED_EVIDENCE_PROTECTION_LOST' WHERE id=NEW.artifact_id RETURNING org_id,id)
  INSERT INTO audit_events(org_id,action,subject_id) SELECT org_id,'cleanup.attention',id FROM changed;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER cleanup_reference_attention AFTER INSERT ON cosmoformer.cleanup_claims FOR EACH ROW EXECUTE FUNCTION cosmoformer.cleanup_reference_attention();
REVOKE ALL ON FUNCTION cosmoformer.cleanup_reference_attention() FROM PUBLIC;
