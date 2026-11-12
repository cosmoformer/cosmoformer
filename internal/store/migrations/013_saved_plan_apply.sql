SET search_path=pg_catalog,cosmoformer,pg_temp;
-- Votes are counted by current account, membership and scoped approval grant.
CREATE FUNCTION cosmoformer.count_plan_approvals(subject uuid) RETURNS bigint LANGUAGE sql STABLE SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
 SELECT count(*) FROM plan_votes v JOIN plans p ON p.id=v.plan_id AND p.org_id=v.org_id
 JOIN approvals e ON e.id=v.last_event_id JOIN stacks s ON s.id=p.stack_id
 JOIN operations o ON o.id=p.operation_id JOIN runs r ON r.id=o.run_id
 WHERE p.id=subject AND v.approved AND v.subject_revision=p.revision AND e.manifest_digest=p.manifest_digest AND e.source_commit=r.source_commit
 AND requester_enabled(v.account_id) AND EXISTS(SELECT FROM memberships m WHERE m.org_id=v.org_id AND m.account_id=v.account_id)
 AND EXISTS(SELECT FROM team_members tm JOIN grants g ON (g.org_id,g.team_id)=(tm.org_id,tm.team_id) WHERE tm.account_id=v.account_id AND g.org_id=v.org_id AND g.project_id=s.project_id AND (g.environment_id IS NULL OR g.environment_id=s.environment_id) AND g.role='approver' AND g.revoked_at IS NULL)
$$;
CREATE FUNCTION cosmoformer.release_operation_holds() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF NEW.status IN ('succeeded','failed','cancelled','superseded','retired') THEN
  UPDATE artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='operation' AND owner_id=NEW.id AND released_at IS NULL;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER release_operation_holds AFTER UPDATE ON cosmoformer.operations FOR EACH ROW EXECUTE FUNCTION cosmoformer.release_operation_holds();
-- Retirement is permanent, including a grant loss/restoration in one transaction.
CREATE FUNCTION cosmoformer.retire_queued_applies() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE tenant uuid; previous text:=current_setting('app.org_id',true);
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  WITH retired AS (
   UPDATE operations o SET status='retired',retired_at=clock_timestamp(),terminal_reason='APPLY_ELIGIBILITY_LOST',revision=o.revision+1
   FROM plans p JOIN stacks s ON s.id=p.stack_id JOIN projects pj ON pj.id=s.project_id JOIN attempts pa ON pa.id=p.attempt_id
   WHERE o.plan_id=p.id AND o.kind='apply' AND o.status='queued' AND NOT EXISTS(SELECT FROM attempts a WHERE a.operation_id=o.id AND a.closed_at IS NULL)
   AND (p.status<>'ready' OR p.expires_at<=clock_timestamp() OR count_plan_approvals(p.id)<greatest(pj.minimum_approvals,(pa.execution_spec->'review_policy'->>'effective_minimum')::int)
    OR NOT requester_enabled(o.requester_id) OR NOT EXISTS(SELECT FROM memberships m WHERE m.org_id=o.org_id AND m.account_id=o.requester_id)
    OR NOT EXISTS(SELECT FROM team_members tm JOIN grants g ON (g.org_id,g.team_id)=(tm.org_id,tm.team_id) WHERE tm.account_id=o.requester_id AND g.org_id=o.org_id AND g.project_id=s.project_id AND (g.environment_id IS NULL OR g.environment_id=s.environment_id) AND g.role='deployer' AND g.revoked_at IS NULL)) RETURNING o.id,o.org_id
  ), recorded AS (INSERT INTO audit_events(org_id,action,subject_id) SELECT org_id,'apply.retired',id FROM retired RETURNING id) UPDATE work_items SET status='retired',revision=revision+1 WHERE operation_id IN(SELECT id FROM retired) AND status='queued';
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
 RETURN NULL;
END $$;
DO $$ DECLARE t text; BEGIN
 FOREACH t IN ARRAY ARRAY['plan_votes','plans','accounts','memberships','team_members','grants'] LOOP
 EXECUTE format('CREATE TRIGGER retire_queued_applies AFTER INSERT OR UPDATE OR DELETE ON cosmoformer.%I FOR EACH STATEMENT EXECUTE FUNCTION cosmoformer.retire_queued_applies()',t);
 END LOOP;
END $$;
-- A unique permission event makes duplicate/late spawn reports harmless. Native
-- state lineage/serial validation remains the final check; no S3 freshness read.
CREATE TABLE cosmoformer.apply_start_events(
 org_id uuid NOT NULL, permission_id uuid PRIMARY KEY, attempt_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY(org_id,permission_id) REFERENCES cosmoformer.start_permissions(org_id,id),
 FOREIGN KEY(org_id,attempt_id) REFERENCES cosmoformer.attempts(org_id,id)
);
ALTER TABLE cosmoformer.apply_start_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE cosmoformer.apply_start_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant ON cosmoformer.apply_start_events USING(org_id=nullif(current_setting('app.org_id',true),'')::uuid) WITH CHECK(org_id=nullif(current_setting('app.org_id',true),'')::uuid);
GRANT SELECT ON cosmoformer.apply_start_events TO cosmoformer_app;
CREATE FUNCTION cosmoformer.apply_spawn_epoch() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE inserted uuid;
BEGIN
 IF NEW.command='apply' AND NEW.status='started' AND OLD.status='issued' THEN
  PERFORM id FROM coordination_gate FOR UPDATE;
  INSERT INTO apply_start_events(org_id,permission_id,attempt_id) VALUES(NEW.org_id,NEW.id,NEW.attempt_id) ON CONFLICT DO NOTHING RETURNING permission_id INTO inserted;
  IF inserted IS NOT NULL THEN UPDATE backend_targets SET state_epoch=state_epoch+1 WHERE id=(SELECT target_id FROM attempts WHERE id=NEW.attempt_id AND org_id=NEW.org_id); END IF;
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER apply_spawn_epoch AFTER UPDATE ON cosmoformer.start_permissions FOR EACH ROW EXECUTE FUNCTION cosmoformer.apply_spawn_epoch();
REVOKE ALL ON FUNCTION cosmoformer.count_plan_approvals(uuid),cosmoformer.release_operation_holds(),cosmoformer.retire_queued_applies(),cosmoformer.apply_spawn_epoch() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cosmoformer.count_plan_approvals(uuid) TO cosmoformer_app;
-- A failed elapsed-clock read can only remove authority, never reset the latch.
CREATE FUNCTION cosmoformer.execution_clock_failed() RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 UPDATE installation SET clock_unreliable=true;
END $$;
REVOKE ALL ON FUNCTION cosmoformer.execution_clock_failed() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cosmoformer.execution_clock_failed() TO cosmoformer_app;
CREATE FUNCTION cosmoformer.apply_operation_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF OLD.kind='apply' AND (OLD.org_id,OLD.run_id,OLD.stack_id,OLD.kind,OLD.plan_id,OLD.requester_id,OLD.automation_repository_id,OLD.accepted_at,OLD.ordering_at) IS DISTINCT FROM (NEW.org_id,NEW.run_id,NEW.stack_id,NEW.kind,NEW.plan_id,NEW.requester_id,NEW.automation_repository_id,NEW.accepted_at,NEW.ordering_at) THEN RAISE EXCEPTION 'apply identity immutable' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER apply_operation_identity BEFORE UPDATE ON cosmoformer.operations FOR EACH ROW EXECUTE FUNCTION cosmoformer.apply_operation_identity();
REVOKE ALL ON FUNCTION cosmoformer.apply_operation_identity() FROM PUBLIC;
