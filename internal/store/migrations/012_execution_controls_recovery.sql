SET search_path = cosmoformer, pg_catalog, pg_temp;
ALTER TABLE attempts ADD COLUMN stop_conflicted boolean NOT NULL DEFAULT false;
ALTER TABLE control_requests ADD COLUMN worker_id uuid, ADD COLUMN generation bigint,
 ADD COLUMN agent_instance uuid, ADD COLUMN signal_sent text CHECK(signal_sent IN ('SIGINT','SIGKILL')),
 ADD COLUMN delivered_at timestamptz, ADD COLUMN observed_at timestamptz;
DO $$ DECLARE tenant uuid; BEGIN
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE control_requests c SET worker_id=a.worker_id,generation=a.generation,agent_instance=a.agent_instance,command_ordinal=coalesce(c.command_ordinal,0) FROM attempts a WHERE a.id=c.attempt_id;
 END LOOP;
END $$;
ALTER TABLE control_requests ALTER COLUMN worker_id SET NOT NULL, ALTER COLUMN generation SET NOT NULL,
 ALTER COLUMN agent_instance SET NOT NULL, ALTER COLUMN command_ordinal SET NOT NULL,
 ADD CHECK(command_ordinal>=0), ADD FOREIGN KEY(org_id,worker_id) REFERENCES workers(org_id,id);
CREATE FUNCTION control_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP='INSERT' AND NEW.worker_id IS NULL AND NEW.generation IS NULL AND NEW.agent_instance IS NULL THEN
  SELECT a.worker_id,a.generation,a.agent_instance INTO NEW.worker_id,NEW.generation,NEW.agent_instance FROM attempts a WHERE a.id=NEW.attempt_id AND a.org_id=NEW.org_id;
 END IF;
 IF TG_OP='DELETE' OR TG_OP='UPDATE' AND (OLD.org_id,OLD.attempt_id,OLD.worker_id,OLD.generation,OLD.agent_instance,OLD.command_ordinal,OLD.kind,OLD.confirmed,OLD.actor_id,OLD.requested_at) IS DISTINCT FROM (NEW.org_id,NEW.attempt_id,NEW.worker_id,NEW.generation,NEW.agent_instance,NEW.command_ordinal,NEW.kind,NEW.confirmed,NEW.actor_id,NEW.requested_at) THEN RAISE EXCEPTION 'control identity immutable' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT FROM attempts a WHERE (a.org_id,a.id,a.worker_id,a.generation,a.agent_instance)=(NEW.org_id,NEW.attempt_id,NEW.worker_id,NEW.generation,NEW.agent_instance)) THEN RAISE EXCEPTION 'control owner mismatch' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND (OLD.state='observed' AND NEW.state<>'observed' OR OLD.state='delivered' AND NEW.state='requested' OR OLD.signal_sent IS NOT NULL AND OLD.signal_sent IS DISTINCT FROM NEW.signal_sent) THEN RAISE EXCEPTION 'control regression' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER control_identity BEFORE INSERT OR UPDATE OR DELETE ON control_requests FOR EACH ROW EXECUTE FUNCTION control_identity();
ALTER TABLE recovery_cases ADD UNIQUE(org_id,id,stack_id,target_id);
CREATE UNIQUE INDEX recovery_origin_event ON recovery_cases(org_id,origin_attempt_id) WHERE origin_attempt_id IS NOT NULL;
ALTER TABLE operations ADD COLUMN recovery_case_id uuid, ADD COLUMN recovery_evidence_revision bigint CHECK(recovery_evidence_revision>0),
 ADD FOREIGN KEY(org_id,recovery_case_id) REFERENCES recovery_cases(org_id,id),
 ADD CHECK((recovery_case_id IS NULL)=(recovery_evidence_revision IS NULL));
ALTER TABLE plans ADD COLUMN recovery_evidence_revision bigint CHECK(recovery_evidence_revision>0);
CREATE FUNCTION recovery_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'recovery history retained' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT FROM stack_bindings b WHERE (b.org_id,b.stack_id,b.target_id)=(NEW.org_id,NEW.stack_id,NEW.target_id)) OR NEW.origin_attempt_id IS NOT NULL AND NOT EXISTS(SELECT FROM attempts a WHERE (a.org_id,a.id,a.stack_id,a.target_id)=(NEW.org_id,NEW.origin_attempt_id,NEW.stack_id,NEW.target_id)) THEN RAISE EXCEPTION 'recovery chain mismatch' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' THEN
  IF (OLD.org_id,OLD.stack_id,OLD.target_id,OLD.origin_attempt_id) IS DISTINCT FROM (NEW.org_id,NEW.stack_id,NEW.target_id,NEW.origin_attempt_id) OR OLD.status='cleared' AND NEW IS DISTINCT FROM OLD THEN RAISE EXCEPTION 'recovery identity immutable' USING ERRCODE='23514'; END IF;
  IF NEW.evidence IS DISTINCT FROM OLD.evidence THEN
   NEW.evidence_revision:=OLD.evidence_revision+1;
  ELSIF NEW.evidence_revision<>OLD.evidence_revision THEN RAISE EXCEPTION 'evidence revision mismatch' USING ERRCODE='23514'; END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_identity BEFORE INSERT OR UPDATE OR DELETE ON recovery_cases FOR EACH ROW EXECUTE FUNCTION recovery_identity();
CREATE FUNCTION recovery_vote_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP='DELETE' OR TG_OP='UPDATE' AND (OLD.org_id,OLD.case_id,OLD.account_id) IS DISTINCT FROM (NEW.org_id,NEW.case_id,NEW.account_id) THEN RAISE EXCEPTION 'vote identity immutable' USING ERRCODE='23514'; END IF;
 IF NOT EXISTS(SELECT FROM approvals e JOIN recovery_cases r ON r.org_id=e.org_id AND r.id=e.subject_id WHERE e.org_id=NEW.org_id AND e.id=NEW.last_event_id AND e.subject_kind='recovery' AND e.subject_id=NEW.case_id AND e.account_id=NEW.account_id AND e.subject_revision=NEW.evidence_revision AND (e.action='approve')=NEW.approved AND r.evidence_revision=e.subject_revision AND r.status='open' AND r.stop_verified AND r.reconciliation_complete AND e.source_observation_id IS NULL AND e.manifest_digest IS NULL AND e.source_commit IS NULL) THEN RAISE EXCEPTION 'recovery vote mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_vote_integrity BEFORE INSERT OR UPDATE OR DELETE ON recovery_votes FOR EACH ROW EXECUTE FUNCTION recovery_vote_integrity();
-- Copy all available attempt/plan/log evidence before any ownership release.
CREATE FUNCTION recovery_hold_artifact() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 INSERT INTO artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason)
 SELECT NEW.org_id,NEW.id,'recovery',r.id,'execution_uncertain' FROM recovery_cases r
 JOIN attempts a ON a.id=NEW.attempt_id AND a.org_id=r.org_id
 LEFT JOIN operations o ON o.id=a.operation_id
 WHERE r.status='open' AND (r.origin_attempt_id=a.id OR o.recovery_case_id=r.id) AND NEW.state IN ('uploading','finalized') AND NOT EXISTS(SELECT FROM cleanup_claims WHERE artifact_id=NEW.id) ON CONFLICT DO NOTHING;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_hold_artifact AFTER INSERT OR UPDATE ON artifacts FOR EACH ROW EXECUTE FUNCTION recovery_hold_artifact();
CREATE OR REPLACE FUNCTION execution_attention(attempt uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE a attempts; inserted uuid; case_id uuid; existing_status text;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 SELECT * INTO a FROM attempts WHERE id=attempt AND org_id=nullif(current_setting('app.org_id',true),'')::uuid;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt unavailable' USING ERRCODE='42501'; END IF;
 SELECT id,status INTO case_id,existing_status FROM recovery_cases WHERE org_id=a.org_id AND origin_attempt_id=a.id;
 IF existing_status='cleared' THEN RETURN; END IF;
 IF a.closed_at IS NULL THEN UPDATE attempts SET state='uncertain',revision=revision+1 WHERE id=a.id; END IF;
 UPDATE operations SET status='needs_attention',terminal_reason='EXECUTION_UNCERTAIN',revision=revision+1 WHERE id=a.operation_id;
 IF a.target_id IS NULL THEN RETURN; END IF;
 IF case_id IS NULL THEN
  INSERT INTO recovery_cases(org_id,stack_id,target_id,origin_attempt_id,status,evidence,opened_at)
  VALUES(a.org_id,a.stack_id,a.target_id,a.id,'open','{}',clock_timestamp())
  ON CONFLICT(target_id) WHERE status='open' DO NOTHING RETURNING id INTO inserted;
  IF inserted IS NOT NULL THEN UPDATE backend_targets SET state_epoch=state_epoch+1 WHERE id=a.target_id; END IF;
 END IF;
 UPDATE recovery_cases SET stop_verified=false,reconciliation_complete=false,evidence='{}',revision=revision+1 WHERE target_id=a.target_id AND status='open' AND stop_verified;
 INSERT INTO artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason)
 SELECT ar.org_id,ar.id,'recovery',r.id,'execution_uncertain' FROM artifacts ar JOIN attempts t ON t.id=ar.attempt_id
 JOIN recovery_cases r ON r.target_id=a.target_id AND r.status='open'
 WHERE t.target_id=a.target_id AND ar.state IN ('uploading','finalized') AND NOT EXISTS(SELECT FROM cleanup_claims WHERE artifact_id=ar.id) ON CONFLICT DO NOTHING;
END $$;
REVOKE ALL ON FUNCTION control_identity(),recovery_identity(),recovery_vote_integrity(),recovery_hold_artifact() FROM PUBLIC;
CREATE TABLE recovery_evidence_versions (
 org_id uuid NOT NULL, case_id uuid NOT NULL, evidence_revision bigint NOT NULL CHECK(evidence_revision>0),
 submitted_by uuid NOT NULL REFERENCES accounts(id), evidence jsonb NOT NULL,
 authority text NOT NULL CHECK(authority IN ('agent_protocol','manual_host','manual_operator')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(org_id,case_id,evidence_revision),
 FOREIGN KEY(org_id,case_id) REFERENCES recovery_cases(org_id,id)
);
ALTER TABLE recovery_evidence_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE recovery_evidence_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON recovery_evidence_versions USING(org_id=nullif(current_setting('app.org_id',true),'')::uuid) WITH CHECK(org_id=nullif(current_setting('app.org_id',true),'')::uuid);
GRANT SELECT,INSERT ON recovery_evidence_versions TO cosmoformer_app;
CREATE FUNCTION recovery_operation_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF TG_OP='UPDATE' AND (OLD.recovery_case_id,OLD.recovery_evidence_revision) IS DISTINCT FROM (NEW.recovery_case_id,NEW.recovery_evidence_revision) THEN RAISE EXCEPTION 'recovery provenance immutable' USING ERRCODE='23514'; END IF;
 IF NEW.recovery_case_id IS NOT NULL AND NOT EXISTS(SELECT FROM recovery_cases r WHERE r.org_id=NEW.org_id AND r.id=NEW.recovery_case_id AND r.stack_id=NEW.stack_id) THEN RAISE EXCEPTION 'recovery operation mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_operation_identity BEFORE INSERT OR UPDATE ON operations FOR EACH ROW EXECUTE FUNCTION recovery_operation_identity();
REVOKE ALL ON FUNCTION recovery_operation_identity() FROM PUBLIC;
CREATE FUNCTION recovery_plan_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF TG_OP='UPDATE' AND (OLD.provenance,OLD.recovery_case_id,OLD.recovery_evidence_revision) IS DISTINCT FROM (NEW.provenance,NEW.recovery_case_id,NEW.recovery_evidence_revision) THEN RAISE EXCEPTION 'plan provenance immutable' USING ERRCODE='23514'; END IF;
 IF (NEW.provenance='recovery') IS DISTINCT FROM (NEW.recovery_evidence_revision IS NOT NULL) OR NOT EXISTS(SELECT FROM operations o WHERE o.id=NEW.operation_id AND o.org_id=NEW.org_id AND o.stack_id=NEW.stack_id AND o.recovery_case_id IS NOT DISTINCT FROM NEW.recovery_case_id AND o.recovery_evidence_revision IS NOT DISTINCT FROM NEW.recovery_evidence_revision) THEN RAISE EXCEPTION 'recovery plan chain mismatch' USING ERRCODE='23514'; END IF;
 IF NEW.provenance='recovery' AND NEW.status='ready' THEN RAISE EXCEPTION 'diagnostic plan never deployable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_plan_identity BEFORE INSERT OR UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION recovery_plan_identity();
REVOKE ALL ON FUNCTION recovery_plan_identity() FROM PUBLIC;
CREATE FUNCTION recovery_approval_event_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF NEW.subject_kind='recovery' AND (NEW.source_observation_id IS NOT NULL OR NEW.manifest_digest IS NOT NULL OR NEW.source_commit IS NOT NULL OR NOT EXISTS(SELECT FROM recovery_cases r WHERE r.org_id=NEW.org_id AND r.id=NEW.subject_id AND r.evidence_revision=NEW.subject_revision AND r.status='open' AND r.stop_verified AND r.reconciliation_complete)) THEN RAISE EXCEPTION 'recovery approval subject mismatch' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER recovery_approval_event_integrity BEFORE INSERT ON approvals FOR EACH ROW EXECUTE FUNCTION recovery_approval_event_integrity();
REVOKE ALL ON FUNCTION recovery_approval_event_integrity() FROM PUBLIC;
