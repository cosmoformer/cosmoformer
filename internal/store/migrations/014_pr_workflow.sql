-- Preserve old immutable operation intent. New reconciliation requests choose
-- native planning explicitly; source-check history is never upgraded.
SET search_path=pg_catalog,cosmoformer,pg_temp;
ALTER TABLE cosmoformer.source_subjects ADD COLUMN path_selection jsonb NOT NULL DEFAULT '{}';
-- Persist ambiguity before the first POST. A missing marker after response loss
-- cannot authorize a second creation. Definite rejection may clear this flag.
ALTER TABLE cosmoformer.feedback_comments ADD COLUMN creation_pending boolean NOT NULL DEFAULT false,
 ADD COLUMN recovery_marker uuid NOT NULL DEFAULT gen_random_uuid();
CREATE FUNCTION cosmoformer.feedback_comment_identity() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF (NEW.org_id,NEW.repository_id,NEW.pr_number,NEW.recovery_marker) IS DISTINCT FROM (OLD.org_id,OLD.repository_id,OLD.pr_number,OLD.recovery_marker)
 OR OLD.comment_id<>'' AND NEW.comment_id IS DISTINCT FROM OLD.comment_id THEN
  RAISE EXCEPTION 'feedback identity is immutable' USING ERRCODE='23514';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER feedback_comment_identity BEFORE UPDATE ON cosmoformer.feedback_comments FOR EACH ROW EXECUTE FUNCTION cosmoformer.feedback_comment_identity();
REVOKE ALL ON FUNCTION cosmoformer.feedback_comment_identity() FROM PUBLIC;

-- Source invalidation and unclaimed coalescing happen even when changed paths
-- select no replacement work. Running attempts retain their original outcome.
CREATE FUNCTION cosmoformer.pr_source_changed() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 IF OLD.facts_revision=NEW.facts_revision THEN RETURN NEW; END IF;
 UPDATE operations o SET status='superseded',terminal_reason='SOURCE_CHANGED',revision=revision+1
 WHERE o.kind='plan' AND o.native_planning AND o.automation_repository_id IS NOT NULL AND o.status='queued'
 AND NOT EXISTS(SELECT FROM attempts a WHERE a.operation_id=o.id AND a.closed_at IS NULL)
 AND o.run_id IN(SELECT id FROM runs WHERE source_subject_id=NEW.id AND source_facts_revision<>NEW.facts_revision);
 UPDATE work_items w SET status='retired',revision=revision+1 WHERE w.status='queued'
 AND w.operation_id IN(SELECT o.id FROM operations o JOIN runs r ON r.id=o.run_id WHERE r.source_subject_id=NEW.id AND o.status='superseded');
 RETURN NEW;
END $$;
CREATE TRIGGER pr_source_changed AFTER UPDATE ON cosmoformer.source_subjects FOR EACH ROW EXECUTE FUNCTION cosmoformer.pr_source_changed();

-- Delivery is an independent durable effect. Payloads carry identities only;
-- the sender derives bounded content after obtaining the per-PR delivery lock.
CREATE FUNCTION cosmoformer.queue_pr_feedback() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE subj uuid; tenant uuid; repo uuid; number text; facts bigint; target record;
BEGIN
 IF TG_TABLE_NAME='source_subjects' THEN
  IF OLD.facts_revision=NEW.facts_revision THEN RETURN NEW; END IF;
  subj:=NEW.id;
 ELSIF TG_TABLE_NAME='runs' THEN subj:=NEW.source_subject_id;
 ELSIF TG_TABLE_NAME='operations' THEN SELECT source_subject_id INTO subj FROM runs WHERE id=NEW.run_id;
 ELSIF TG_TABLE_NAME='plans' THEN SELECT r.source_subject_id INTO subj FROM runs r JOIN operations o ON o.run_id=r.id WHERE o.id=NEW.operation_id;
 ELSIF TG_TABLE_NAME='recovery_cases' THEN
  FOR target IN SELECT DISTINCT source_subject_id FROM runs WHERE stack_id=NEW.stack_id LOOP
   INSERT INTO outbox(org_id,kind,subject_id,payload,status,next_attempt_at,delivery_key) SELECT org_id,'pr_feedback',repository_id,jsonb_build_object('pr_number',external_key::bigint,'facts_revision',facts_revision),'pending',clock_timestamp(),gen_random_uuid()::text FROM source_subjects WHERE id=target.source_subject_id AND kind='pr';
  END LOOP;
  RETURN NEW;
 ELSIF TG_TABLE_NAME='plan_votes' THEN SELECT r.source_subject_id INTO subj FROM runs r JOIN operations o ON o.run_id=r.id JOIN plans p ON p.operation_id=o.id WHERE p.id=NEW.plan_id;
 END IF;
 FOR target IN SELECT s.org_id,s.repository_id,s.external_key,s.facts_revision FROM source_subjects s
 WHERE s.kind='pr' AND (s.id=subj OR (s.facts->>'state'='MERGED' AND s.repository_id=(SELECT repository_id FROM source_subjects WHERE id=subj AND kind='branch'))) LOOP
 tenant:=target.org_id;repo:=target.repository_id;number:=target.external_key;facts:=target.facts_revision;
  UPDATE outbox SET status='delivered',delivered_at=clock_timestamp(),revision=revision+1
  WHERE org_id=tenant AND kind='pr_feedback' AND subject_id=repo AND payload->>'pr_number'=number AND status='pending' AND claim_id IS NULL;
  INSERT INTO outbox(org_id,kind,subject_id,payload,status,next_attempt_at,delivery_key)
  VALUES(tenant,'pr_feedback',repo,jsonb_build_object('pr_number',number::bigint,'facts_revision',facts),'pending',clock_timestamp(),gen_random_uuid()::text);
 END LOOP;
 RETURN NEW;
END $$;
CREATE TRIGGER queue_pr_feedback AFTER UPDATE ON cosmoformer.source_subjects FOR EACH ROW EXECUTE FUNCTION cosmoformer.queue_pr_feedback();
CREATE TRIGGER queue_pr_feedback AFTER INSERT ON cosmoformer.runs FOR EACH ROW EXECUTE FUNCTION cosmoformer.queue_pr_feedback();
CREATE TRIGGER queue_pr_feedback AFTER INSERT OR UPDATE ON cosmoformer.operations FOR EACH ROW EXECUTE FUNCTION cosmoformer.queue_pr_feedback();
CREATE TRIGGER queue_pr_feedback AFTER INSERT OR UPDATE ON cosmoformer.plans FOR EACH ROW EXECUTE FUNCTION cosmoformer.queue_pr_feedback();
CREATE TRIGGER queue_pr_feedback AFTER INSERT OR UPDATE ON cosmoformer.plan_votes FOR EACH ROW EXECUTE FUNCTION cosmoformer.queue_pr_feedback();
CREATE TRIGGER queue_pr_feedback AFTER INSERT OR UPDATE ON cosmoformer.recovery_cases FOR EACH ROW EXECUTE FUNCTION cosmoformer.queue_pr_feedback();
REVOKE ALL ON FUNCTION cosmoformer.pr_source_changed(),cosmoformer.queue_pr_feedback() FROM PUBLIC;
CREATE INDEX runs_pr_heads ON cosmoformer.runs(org_id,source_subject_id,source_facts_revision,created_at,id);
