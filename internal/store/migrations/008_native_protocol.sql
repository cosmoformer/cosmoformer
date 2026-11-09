-- Persist immutable attempt inputs and process facts missing from schema 7.
-- No scheduling/admission capability or executor is enabled by this migration.
SET search_path = cosmoformer, pg_catalog;
ALTER TABLE aws_connections
 ADD COLUMN expected_bootstrap_principal text,
 ADD COLUMN session_seconds bigint CHECK(session_seconds IS NULL OR session_seconds=3600);
ALTER TABLE attempts
 ADD COLUMN execution_spec jsonb,
 ADD COLUMN execution_spec_digest bytea CHECK(execution_spec_digest IS NULL OR octet_length(execution_spec_digest)=32),
 ADD COLUMN credential_platform_instance uuid,
 ADD COLUMN credential_deadline_tick bigint,
 ADD COLUMN execution_preparation jsonb,
 ADD COLUMN provider_lock_artifact_id uuid,
 ADD FOREIGN KEY(org_id,provider_lock_artifact_id) REFERENCES artifacts(org_id,id),
 ADD CONSTRAINT execution_spec_identity CHECK((execution_spec IS NULL)=(execution_spec_digest IS NULL));
ALTER TABLE start_permissions
 ADD COLUMN spawn_facts jsonb,
 ADD COLUMN stop_facts jsonb,
 ADD CONSTRAINT native_command_domain CHECK(command IN ('init','schema','plan','show','apply'));
CREATE FUNCTION native_protocol_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF OLD.execution_spec IS DISTINCT FROM NEW.execution_spec OR OLD.execution_spec_digest IS DISTINCT FROM NEW.execution_spec_digest THEN
  RAISE EXCEPTION 'attempt execution identity is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.pre_init_spec_digest IS NOT NULL AND (OLD.pre_init_spec_digest IS DISTINCT FROM NEW.pre_init_spec_digest OR OLD.pre_init_spec_artifact_id IS DISTINCT FROM NEW.pre_init_spec_artifact_id OR OLD.execution_preparation IS DISTINCT FROM NEW.execution_preparation OR OLD.credential_platform_instance IS DISTINCT FROM NEW.credential_platform_instance OR OLD.credential_deadline_tick IS DISTINCT FROM NEW.credential_deadline_tick) THEN
  RAISE EXCEPTION 'pre-init identity is immutable' USING ERRCODE='23514';
 END IF;
 IF OLD.manifest_stage='sealed' AND (OLD.manifest_stage IS DISTINCT FROM NEW.manifest_stage OR OLD.sealed_manifest_digest IS DISTINCT FROM NEW.sealed_manifest_digest OR OLD.sealed_manifest_artifact_id IS DISTINCT FROM NEW.sealed_manifest_artifact_id OR OLD.provider_lock_artifact_id IS DISTINCT FROM NEW.provider_lock_artifact_id) THEN
  RAISE EXCEPTION 'sealed identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER native_protocol_identity BEFORE UPDATE ON attempts FOR EACH ROW EXECUTE FUNCTION native_protocol_identity();
REVOKE ALL ON FUNCTION native_protocol_identity() FROM PUBLIC;
-- These helpers reveal only an attempt's own already-bound target. Runtime
-- still has no SELECT/UPDATE access to installation-wide target records.
CREATE FUNCTION execution_target_epoch(attempt uuid) RETURNS bigint LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT b.state_epoch FROM attempts a JOIN backend_targets b ON b.id=a.target_id
 WHERE a.id=attempt AND a.org_id=nullif(current_setting('app.org_id',true),'')::uuid;
$$;
CREATE FUNCTION execution_attention(attempt uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE a attempts; inserted uuid;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 SELECT * INTO a FROM attempts WHERE id=attempt AND org_id=nullif(current_setting('app.org_id',true),'')::uuid;
 IF NOT FOUND THEN RAISE EXCEPTION 'attempt unavailable' USING ERRCODE='42501'; END IF;
 IF a.closed_at IS NULL THEN
  UPDATE attempts SET state='uncertain',revision=revision+1 WHERE id=a.id;
 END IF;
 UPDATE operations SET status='needs_attention',revision=revision+1 WHERE id=a.operation_id;
 IF a.target_id IS NOT NULL THEN
  INSERT INTO recovery_cases(org_id,stack_id,target_id,origin_attempt_id,status,evidence,opened_at)
   VALUES(a.org_id,a.stack_id,a.target_id,a.id,'open','{}',clock_timestamp())
   ON CONFLICT(target_id) WHERE status='open' DO NOTHING RETURNING id INTO inserted;
  IF inserted IS NOT NULL THEN
   UPDATE backend_targets SET state_epoch=state_epoch+1 WHERE id=a.target_id;
   UPDATE plans SET status='invalidated',invalidation_reason='EXECUTION_UNCERTAIN',revision=revision+1 WHERE target_id=a.target_id AND status='ready';
  END IF;
 END IF;
END $$;
ALTER TABLE installation ADD COLUMN authority_platform_instance uuid, ADD COLUMN authority_tick bigint;
CREATE FUNCTION execution_clock_observe(instance uuid,tick bigint) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE i installation; utc timestamptz;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 SELECT * INTO i FROM installation; utc:=clock_timestamp();
 IF i.clock_unreliable THEN RETURN false; END IF;
 IF utc<i.authority_highwater OR (i.authority_platform_instance=instance AND (tick<i.authority_tick OR abs(extract(epoch FROM (utc-i.authority_highwater))-(tick-i.authority_tick)/1000000000.0)>1)) THEN
  UPDATE installation SET clock_unreliable=true; RETURN false;
 END IF;
 UPDATE installation SET authority_highwater=utc,authority_platform_instance=instance,authority_tick=tick;
 RETURN true;
END $$;
REVOKE ALL ON FUNCTION execution_target_epoch(uuid),execution_attention(uuid),execution_clock_observe(uuid,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION execution_target_epoch(uuid),execution_attention(uuid),execution_clock_observe(uuid,bigint) TO cosmoformer_app;
CREATE UNIQUE INDEX attempt_message_identity ON attempt_messages(org_id,message_id);
-- Native output evidence belongs to its producing command, not simply whichever
-- command later names a same-attempt upload. Logs keep their stream sequencing.
ALTER TABLE artifacts ADD COLUMN command_ordinal bigint NOT NULL DEFAULT 0 CHECK(command_ordinal>=0);
CREATE FUNCTION native_artifact_stage() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  SELECT command_ordinal INTO NEW.command_ordinal FROM attempts WHERE id=NEW.attempt_id;
 ELSIF OLD.command_ordinal IS DISTINCT FROM NEW.command_ordinal THEN
  RAISE EXCEPTION 'artifact command identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER native_artifact_stage BEFORE INSERT OR UPDATE ON artifacts FOR EACH ROW EXECUTE FUNCTION native_artifact_stage();
REVOKE ALL ON FUNCTION native_artifact_stage() FROM PUBLIC;
CREATE FUNCTION execution_binding_matches(attempt uuid,part text,buck text,obj text,profile_name text,table_name text) RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT EXISTS(SELECT FROM attempts a JOIN stack_bindings s ON s.org_id=a.org_id AND s.stack_id=a.stack_id AND s.target_id=a.target_id JOIN backend_targets t ON t.id=s.target_id
 WHERE a.id=attempt AND a.org_id=nullif(current_setting('app.org_id',true),'')::uuid
 AND t.partition=part AND t.bucket=buck AND t.effective_key=obj
 AND s.profile->>'lock_profile'=profile_name AND coalesce(s.profile->>'table_arn','')=coalesce(table_name,''));
$$;
REVOKE ALL ON FUNCTION execution_binding_matches(uuid,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION execution_binding_matches(uuid,text,text,text,text,text) TO cosmoformer_app;
