-- Existing requests and reconciliation jobs remain source-only. Native intent
-- is recorded only on insertion and cannot be added to queued/active history.
SET search_path = pg_catalog, cosmoformer, pg_temp;
ALTER TABLE cosmoformer.operations ADD COLUMN native_planning boolean NOT NULL DEFAULT false,
 ADD CONSTRAINT native_planning_kind CHECK(NOT native_planning OR kind='plan');
CREATE FUNCTION cosmoformer.planning_intent_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF OLD.native_planning IS DISTINCT FROM NEW.native_planning THEN
  RAISE EXCEPTION 'planning intent is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER planning_intent_identity BEFORE UPDATE ON cosmoformer.operations FOR EACH ROW EXECUTE FUNCTION cosmoformer.planning_intent_identity();
REVOKE ALL ON FUNCTION cosmoformer.planning_intent_identity() FROM PUBLIC;
-- Readiness seals plan evidence permanently. Invalidation/expiry may change
-- usability, but neither can revive or rewrite what was reviewed.
CREATE FUNCTION cosmoformer.ready_plan_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF OLD.ready_at IS NOT NULL AND
  (NEW.org_id,NEW.operation_id,NEW.attempt_id,NEW.stack_id,NEW.target_id,NEW.provenance,NEW.recovery_case_id,NEW.source_facts_revision,NEW.state_epoch,NEW.manifest_artifact_id,NEW.manifest_digest,NEW.rule_version,NEW.profile_id,NEW.ready_at,NEW.expires_at)
  IS DISTINCT FROM
  (OLD.org_id,OLD.operation_id,OLD.attempt_id,OLD.stack_id,OLD.target_id,OLD.provenance,OLD.recovery_case_id,OLD.source_facts_revision,OLD.state_epoch,OLD.manifest_artifact_id,OLD.manifest_digest,OLD.rule_version,OLD.profile_id,OLD.ready_at,OLD.expires_at) THEN
  RAISE EXCEPTION 'ready plan evidence is immutable' USING ERRCODE='23514';
 END IF;
 IF NEW.status='ready' AND OLD.status NOT IN ('assembling','ready') THEN
  RAISE EXCEPTION 'plan cannot regain readiness' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER ready_plan_identity BEFORE UPDATE ON cosmoformer.plans FOR EACH ROW EXECUTE FUNCTION cosmoformer.ready_plan_identity();
REVOKE ALL ON FUNCTION cosmoformer.ready_plan_identity() FROM PUBLIC;
