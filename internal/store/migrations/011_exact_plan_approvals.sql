SET search_path = cosmoformer, pg_catalog, pg_temp;
-- A policy revision changes only with the approval floor, not a project label.
ALTER TABLE projects ADD COLUMN approval_policy_revision bigint NOT NULL DEFAULT 1 CHECK(approval_policy_revision>0);
-- FORCE RLS also applies to the migration owner: visit each tenant explicitly
-- so existing policy identities survive the forward upgrade.
DO $$ DECLARE tenant uuid; previous text:=current_setting('app.org_id',true); BEGIN
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE projects SET approval_policy_revision=revision;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
END $$;
CREATE FUNCTION approval_policy_revision() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 NEW.approval_policy_revision:=OLD.approval_policy_revision+CASE WHEN NEW.minimum_approvals IS DISTINCT FROM OLD.minimum_approvals THEN 1 ELSE 0 END;
 RETURN NEW;
END $$;
CREATE TRIGGER approval_policy_revision BEFORE UPDATE ON projects FOR EACH ROW EXECUTE FUNCTION approval_policy_revision();
ALTER TABLE approvals ADD COLUMN manifest_digest bytea CHECK(manifest_digest IS NULL OR octet_length(manifest_digest)=32),
 ADD COLUMN source_commit text CHECK(source_commit IS NULL OR source_commit ~ '^[0-9a-f]{40}$');
CREATE FUNCTION approval_event_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP<>'INSERT' THEN RAISE EXCEPTION 'approval history is immutable' USING ERRCODE='23514'; END IF;
 IF NEW.subject_kind='plan' AND NOT EXISTS(SELECT FROM plans p JOIN operations o ON o.id=p.operation_id JOIN runs r ON r.id=o.run_id
  WHERE p.org_id=NEW.org_id AND p.id=NEW.subject_id AND p.revision=NEW.subject_revision
  AND (NEW.action='revoke' OR (p.status='ready' AND p.expires_at>clock_timestamp() AND p.provenance='normal'
   AND p.manifest_digest=NEW.manifest_digest AND r.source_commit=NEW.source_commit
   AND EXISTS(SELECT FROM source_observations so WHERE so.org_id=p.org_id AND so.id=NEW.source_observation_id AND so.subject_id=r.source_subject_id AND so.resulting_facts_revision=p.source_facts_revision AND so.facts->>'commit'=r.source_commit)))) THEN
  RAISE EXCEPTION 'approval subject mismatch' USING ERRCODE='23514';
 END IF;
 NEW.accepted_at:=clock_timestamp();
 RETURN NEW;
END $$;
CREATE TRIGGER approval_event_integrity BEFORE INSERT OR UPDATE OR DELETE ON approvals FOR EACH ROW EXECUTE FUNCTION approval_event_integrity();
CREATE FUNCTION plan_vote_integrity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'vote history is retained' USING ERRCODE='23514'; END IF;
 IF TG_OP='UPDATE' AND (OLD.org_id,OLD.plan_id,OLD.account_id) IS DISTINCT FROM (NEW.org_id,NEW.plan_id,NEW.account_id) THEN
  RAISE EXCEPTION 'vote identity is immutable' USING ERRCODE='23514';
 END IF;
 IF NOT EXISTS(SELECT FROM approvals e WHERE e.org_id=NEW.org_id AND e.id=NEW.last_event_id AND e.subject_kind='plan' AND e.subject_id=NEW.plan_id AND e.account_id=NEW.account_id AND e.subject_revision=NEW.subject_revision AND (e.action='approve')=NEW.approved) THEN
  RAISE EXCEPTION 'vote event mismatch' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER plan_vote_integrity BEFORE INSERT OR UPDATE OR DELETE ON plan_votes FOR EACH ROW EXECUTE FUNCTION plan_vote_integrity();
-- Input mutations and the last approval gate use the same lock. Invalidate on
-- semantic changes, including an ABA, without modifying finalized evidence.
CREATE FUNCTION invalidate_reviewed_input() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE before_row jsonb:='{}'; after_row jsonb:='{}'; ident text; project text; environment text;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_OP<>'INSERT' THEN before_row:=to_jsonb(OLD); END IF;
 IF TG_OP<>'DELETE' THEN after_row:=to_jsonb(NEW); END IF;
 ident:=coalesce(after_row->>'id',before_row->>'id');
 project:=coalesce(after_row->>'project_id',before_row->>'project_id');
 environment:=coalesce(after_row->>'environment_id',before_row->>'environment_id');
 -- Exclude operational ciphertext/refresh and cosmetic metadata. A semantic
 -- revision is deliberately retained in comparisons for secrets/credentials.
 IF TG_OP='UPDATE' AND (CASE TG_TABLE_NAME
  WHEN 'secrets' THEN before_row-ARRAY['encrypted_value','revision','name'] = after_row-ARRAY['encrypted_value','revision','name']
  WHEN 'source_credentials' THEN before_row-ARRAY['encrypted_value','revision'] = after_row-ARRAY['encrypted_value','revision']
  WHEN 'integrations' THEN before_row->'enabled'=after_row->'enabled'
  WHEN 'projects' THEN (before_row->'minimum_approvals',before_row->'archived') IS NOT DISTINCT FROM (after_row->'minimum_approvals',after_row->'archived')
  WHEN 'environments' THEN before_row->'archived'=after_row->'archived'
  WHEN 'stacks' THEN (before_row->'effective_revision_id',before_row->'archived',before_row->'assessment') IS NOT DISTINCT FROM (after_row->'effective_revision_id',after_row->'archived',after_row->'assessment')
  WHEN 'worker_pools' THEN before_row->'enabled'=after_row->'enabled'
  WHEN 'artifacts' THEN before_row->'state'=after_row->'state' OR after_row->>'state'='finalized'
  ELSE before_row-ARRAY['revision','name'] = after_row-ARRAY['revision','name'] END) THEN RETURN NEW; END IF;
 WITH affected AS (
  UPDATE plans p SET status='invalidated',invalidation_reason=CASE WHEN TG_TABLE_NAME IN ('artifacts','cleanup_claims') THEN 'EVIDENCE_UNAVAILABLE' WHEN TG_TABLE_NAME='recovery_cases' THEN 'RECOVERY_REQUIRED' ELSE 'INPUTS_CHANGED' END,revision=p.revision+1
  FROM attempts a,stacks s
  WHERE p.status='ready' AND a.id=p.attempt_id AND s.id=p.stack_id AND CASE TG_TABLE_NAME
   WHEN 'stacks' THEN s.id::text=ident
   WHEN 'projects' THEN s.project_id::text=ident
   WHEN 'environments' THEN s.environment_id::text=ident
   WHEN 'repositories' THEN s.repository_id::text=ident OR a.execution_spec->'resource_grant_refs' @> jsonb_build_array(jsonb_build_object('resource_id',ident,'kind','repository'))
   WHEN 'integrations' THEN EXISTS(SELECT FROM repositories r WHERE r.id=s.repository_id AND r.integration_id::text=ident)
   WHEN 'worker_pools' THEN a.execution_spec->'pool'->>'id'=ident
   WHEN 'secrets' THEN a.execution_spec->'secret_refs' @> jsonb_build_array(jsonb_build_object('id',ident))
   WHEN 'source_credentials' THEN a.execution_spec->'source_credential_refs' @> jsonb_build_array(jsonb_build_object('id',ident)) OR a.execution_spec->'resource_grant_refs' @> jsonb_build_array(jsonb_build_object('resource_id',coalesce(after_row->>'repository_id',before_row->>'repository_id'),'kind','repository'))
   WHEN 'resource_grants' THEN s.project_id::text=project AND (environment IS NULL OR s.environment_id::text=environment)
   WHEN 'aws_connections' THEN a.execution_spec->'aws'->>'id'=ident OR s.project_id::text=project AND (environment IS NULL OR s.environment_id::text=environment)
   WHEN 'stack_bindings' THEN s.id::text=coalesce(after_row->>'stack_id',before_row->>'stack_id')
   WHEN 'artifacts' THEN p.id::text=coalesce(after_row->>'plan_id',before_row->>'plan_id') AND a.stop_proof->'artifact_ids' ? ident
   WHEN 'cleanup_claims' THEN a.stop_proof->'artifact_ids' ? coalesce(after_row->>'artifact_id',before_row->>'artifact_id')
   WHEN 'recovery_cases' THEN p.target_id::text=after_row->>'target_id' AND after_row->>'status'='open'
   ELSE false END RETURNING p.org_id,p.id,p.invalidation_reason
 ) INSERT INTO audit_events(org_id,action,subject_id,facts) SELECT org_id,'plan.invalidated',id,jsonb_build_object('reason',invalidation_reason) FROM affected;
 RETURN coalesce(NEW,OLD);
END $$;
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['stacks','projects','environments','repositories','integrations','worker_pools','secrets','source_credentials','resource_grants','aws_connections','stack_bindings','artifacts','cleanup_claims','recovery_cases'] LOOP
  EXECUTE format('CREATE TRIGGER invalidate_reviewed_input AFTER INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION invalidate_reviewed_input()',t);
 END LOOP;
END $$;
-- Runtime withdrawal is installation-wide, with no generally callable tenant
-- bypass. Restore tenant context even when invoked by a host catalog update.
CREATE FUNCTION invalidate_runtime_plans() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE tenant uuid; previous text:=current_setting('app.org_id',true);
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF (OLD.engine,OLD.version,OLD.architecture,OLD.image_digest,OLD.approved,OLD.withdrawn) IS NOT DISTINCT FROM (NEW.engine,NEW.version,NEW.architecture,NEW.image_digest,NEW.approved,NEW.withdrawn) THEN RETURN NEW; END IF;
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE plans p SET status='invalidated',invalidation_reason='RUNTIME_CHANGED',revision=p.revision+1 FROM attempts a WHERE a.id=p.attempt_id AND p.status='ready' AND a.execution_spec->'engine'->>'runtime_id'=OLD.id::text;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
 RETURN NEW;
END $$;
CREATE TRIGGER invalidate_runtime_plans AFTER UPDATE ON runtime_catalog FOR EACH ROW EXECUTE FUNCTION invalidate_runtime_plans();
CREATE FUNCTION release_invalid_plan_holds() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF NEW.status IN ('invalidated','expired','unsupported','recovery_only') AND OLD.status IS DISTINCT FROM NEW.status THEN
  UPDATE artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='plan' AND owner_id=NEW.id AND released_at IS NULL;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER release_invalid_plan_holds AFTER UPDATE ON plans FOR EACH ROW EXECUTE FUNCTION release_invalid_plan_holds();
REVOKE ALL ON FUNCTION approval_policy_revision(),approval_event_integrity(),plan_vote_integrity(),invalidate_reviewed_input(),invalidate_runtime_plans(),release_invalid_plan_holds() FROM PUBLIC;
-- Known platform state changes permanently consume earlier reviewed epochs.
CREATE FUNCTION invalidate_epoch_plans() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE tenant uuid; previous text:=current_setting('app.org_id',true);
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF NEW.state_epoch<OLD.state_epoch THEN RAISE EXCEPTION 'state epoch cannot decrease' USING ERRCODE='23514'; END IF;
 IF NEW.state_epoch=OLD.state_epoch THEN RETURN NEW; END IF;
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE plans SET status='invalidated',invalidation_reason='STATE_CHANGED',revision=revision+1 WHERE target_id=NEW.id AND status='ready';
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
 RETURN NEW;
END $$;
CREATE TRIGGER invalidate_epoch_plans AFTER UPDATE ON backend_targets FOR EACH ROW EXECUTE FUNCTION invalidate_epoch_plans();
REVOKE ALL ON FUNCTION invalidate_epoch_plans() FROM PUBLIC;
