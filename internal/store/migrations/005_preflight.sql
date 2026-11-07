-- Phase 2 forward correction: the first four migrations remain immutable.
SET search_path = cosmoformer, pg_catalog;
GRANT SELECT, INSERT, UPDATE ON runtime_catalog TO cosmoformer_app;
ALTER TABLE work_items ADD COLUMN assignment_spec jsonb;
ALTER TABLE workers ADD COLUMN tags text[] NOT NULL DEFAULT '{}';
CREATE UNIQUE INDEX ON source_credentials(org_id,repository_id) WHERE enabled;
ALTER TABLE integrations ADD COLUMN refresh_claim uuid;
ALTER TABLE integrations ADD COLUMN refresh_claimed_at timestamptz;
ALTER TABLE integrations ADD COLUMN reconnect_required boolean NOT NULL DEFAULT false;
ALTER TABLE integrations ADD COLUMN hook_uuid text;
CREATE TABLE oauth_states (
 org_id uuid NOT NULL,
 integration_id uuid NOT NULL,
 actor_id uuid NOT NULL REFERENCES accounts(id),
 verifier bytea PRIMARY KEY CHECK(octet_length(verifier)=32),
 session_verifier bytea NOT NULL CHECK(octet_length(session_verifier)=32),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 FOREIGN KEY(org_id,integration_id) REFERENCES integrations(org_id,id)
);
ALTER TABLE oauth_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE oauth_states FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON oauth_states USING(org_id=nullif(current_setting('app.org_id',true),'')::uuid) WITH CHECK(org_id=nullif(current_setting('app.org_id',true),'')::uuid);
GRANT SELECT,INSERT,UPDATE,DELETE ON oauth_states TO cosmoformer_app;
CREATE FUNCTION consume_oauth_state(state_digest bytea,session_digest bytea) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE tenant uuid; result jsonb; previous text:=current_setting('app.org_id',true); actor uuid;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 SELECT account_id INTO actor FROM sessions WHERE verifier=session_digest AND session_identity(session_digest) IS NOT NULL;
 IF actor IS NULL THEN RETURN NULL; END IF;
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  UPDATE oauth_states SET consumed_at=clock_timestamp()
   WHERE verifier=state_digest AND session_verifier=session_digest AND actor_id=actor
   AND consumed_at IS NULL AND expires_at>clock_timestamp()
   AND (EXISTS(SELECT FROM accounts WHERE id=actor AND enabled AND installation_admin) OR EXISTS(SELECT FROM memberships WHERE org_id=tenant AND account_id=actor AND org_admin))
   RETURNING jsonb_build_object('org_id',org_id,'integration_id',integration_id) INTO result;
  IF result IS NOT NULL THEN EXIT; END IF;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
 RETURN result;
END $$;
REVOKE ALL ON FUNCTION consume_oauth_state(bytea,bytea) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION consume_oauth_state(bytea,bytea) TO cosmoformer_app;

CREATE FUNCTION reserve_encryptions(key_fingerprint bytea, amount bigint) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF amount<1 OR amount>4096 THEN RAISE EXCEPTION 'INVALID_QUOTA' USING ERRCODE='23514'; END IF;
 UPDATE crypto_keys SET reserved_encryptions=reserved_encryptions+amount
 WHERE fingerprint=key_fingerprint AND state='active'
 AND fingerprint=(SELECT active_key_fingerprint FROM installation WHERE id)
 AND reserved_encryptions<=4294967296-amount;
 IF NOT FOUND THEN RAISE EXCEPTION 'KEY_UNAVAILABLE' USING ERRCODE='42501'; END IF;
END $$;

CREATE FUNCTION worker_identity(token_digest bytea) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE tenant uuid; found_worker jsonb; previous text:=current_setting('app.org_id',true);
BEGIN
 IF octet_length(token_digest)<>32 THEN RETURN NULL; END IF;
 -- FORCE RLS remains enabled. This narrow lookup returns only the matching
 -- bearer identity, never a tenant or worker directory.
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  SELECT jsonb_build_object('id',id,'org_id',org_id,'pool_id',pool_id)
    INTO found_worker FROM workers WHERE verifier=token_digest;
  IF found_worker IS NOT NULL THEN
   PERFORM set_config('app.org_id',coalesce(previous,''),true);
   RETURN found_worker;
  END IF;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
 RETURN NULL;
END $$;

CREATE FUNCTION bind_backend(tenant uuid,stack uuid,part text,buck text,obj text,lock_profile text,table_arn text DEFAULT NULL) RETURNS jsonb
LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE target uuid; binding uuid; old_target uuid; revision_id uuid; item text;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF tenant IS DISTINCT FROM nullif(current_setting('app.org_id',true),'')::uuid THEN
  RAISE EXCEPTION 'FORBIDDEN' USING ERRCODE='42501';
 END IF;
 SELECT effective_revision_id INTO revision_id FROM stacks WHERE org_id=tenant AND id=stack AND NOT archived;
 IF revision_id IS NULL THEN RAISE EXCEPTION 'UNAVAILABLE' USING ERRCODE='23514'; END IF;
 IF part NOT IN ('aws','aws-cn','aws-us-gov') OR buck !~ '^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$'
 OR obj='' OR octet_length(obj)>1024 OR lock_profile NOT IN ('s3','dynamodb','dual')
 OR (lock_profile IN ('dynamodb','dual') AND (table_arn IS NULL OR table_arn !~ ('^arn:'||part||':dynamodb:[a-z0-9-]+:[0-9]{12}:table/[A-Za-z0-9_.-]+$')))
 OR (lock_profile='s3' AND table_arn IS NOT NULL) THEN RAISE EXCEPTION 'INVALID_BACKEND' USING ERRCODE='23514'; END IF;
 SELECT id INTO target FROM backend_targets WHERE partition=part AND bucket=buck AND effective_key=obj;
 SELECT id,target_id INTO binding,old_target FROM stack_bindings WHERE org_id=tenant AND stack_id=stack;
 IF binding IS NOT NULL THEN
  IF target IS DISTINCT FROM old_target THEN RAISE EXCEPTION 'BINDING_CONFLICT' USING ERRCODE='23505'; END IF;
 ELSE
  IF target IS NOT NULL THEN RAISE EXCEPTION 'BINDING_CONFLICT' USING ERRCODE='23505'; END IF;
  INSERT INTO backend_targets(partition,bucket,effective_key) VALUES(part,buck,obj) RETURNING id INTO target;
  INSERT INTO stack_bindings(org_id,stack_id,target_id,profile,bound_revision_id)
   VALUES(tenant,stack,target,jsonb_build_object('lock_profile',lock_profile,'table_arn',table_arn),revision_id) RETURNING id INTO binding;
 END IF;
 INSERT INTO backend_resource_claims(target_id,kind,namespace,resource_key,purpose)
 VALUES(target,'s3_object',part||'/'||buck,obj,'state'),(target,'s3_object',part||'/'||buck,obj||'.tflock','lock')
 ON CONFLICT(kind,namespace,resource_key) DO UPDATE SET target_id=backend_resource_claims.target_id
 WHERE backend_resource_claims.target_id=excluded.target_id;
 IF EXISTS(SELECT FROM backend_resource_claims WHERE kind='s3_object' AND namespace=part||'/'||buck AND resource_key IN(obj,obj||'.tflock') AND target_id<>target)
 THEN RAISE EXCEPTION 'BINDING_CONFLICT' USING ERRCODE='23505'; END IF;
 IF table_arn IS NOT NULL THEN
  item:=buck||'/'||obj;
  IF octet_length(item)+4>2048 THEN RAISE EXCEPTION 'INVALID_BACKEND' USING ERRCODE='23514'; END IF;
  INSERT INTO backend_resource_claims(target_id,kind,namespace,resource_key,purpose)
   VALUES(target,'ddb_item',table_arn,item,'lock'),(target,'ddb_item',table_arn,item||'-md5','checksum')
   ON CONFLICT(kind,namespace,resource_key) DO UPDATE SET target_id=backend_resource_claims.target_id
   WHERE backend_resource_claims.target_id=excluded.target_id;
  IF EXISTS(SELECT FROM backend_resource_claims WHERE kind='ddb_item' AND namespace=table_arn AND resource_key IN(item,item||'-md5') AND target_id<>target)
  THEN RAISE EXCEPTION 'BINDING_CONFLICT' USING ERRCODE='23505'; END IF;
 END IF;
 RETURN jsonb_build_object('binding_id',binding,'target_id',target);
END $$;
REVOKE ALL ON FUNCTION reserve_encryptions(bytea,bigint),worker_identity(bytea),bind_backend(uuid,uuid,text,text,text,text,text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION reserve_encryptions(bytea,bigint),worker_identity(bytea),bind_backend(uuid,uuid,text,text,text,text,text) TO cosmoformer_app;

CREATE TABLE worker_requests (
 org_id uuid NOT NULL,
 worker_id uuid NOT NULL,
 request_id uuid NOT NULL,
 generation bigint NOT NULL,
 agent_instance uuid NOT NULL,
 request_digest bytea NOT NULL CHECK(octet_length(request_digest)=32),
 response jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(org_id,worker_id,request_id),
 FOREIGN KEY(org_id,worker_id) REFERENCES workers(org_id,id)
);
ALTER TABLE worker_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE worker_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON worker_requests USING(org_id=nullif(current_setting('app.org_id',true),'')::uuid) WITH CHECK(org_id=nullif(current_setting('app.org_id',true),'')::uuid);
GRANT SELECT,INSERT ON worker_requests TO cosmoformer_app;
CREATE INDEX ON workers(verifier);
CREATE FUNCTION requester_enabled(actor uuid) RETURNS boolean LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$ SELECT EXISTS(SELECT FROM accounts WHERE id=actor AND enabled); $$;
REVOKE ALL ON FUNCTION requester_enabled(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION requester_enabled(uuid) TO cosmoformer_app;
ALTER TABLE outbox ADD COLUMN delivery_key text;
ALTER TABLE outbox ADD COLUMN claim_id uuid;
ALTER TABLE outbox ADD COLUMN claimed_at timestamptz;
CREATE UNIQUE INDEX ON outbox(org_id,kind,subject_id,delivery_key);
CREATE TABLE feedback_comments (
 org_id uuid NOT NULL,
 repository_id uuid NOT NULL,
 pr_number bigint NOT NULL CHECK(pr_number>0),
 comment_id text NOT NULL,
 PRIMARY KEY(org_id,repository_id,pr_number),
 FOREIGN KEY(org_id,repository_id) REFERENCES repositories(org_id,id)
);
ALTER TABLE feedback_comments ENABLE ROW LEVEL SECURITY;
ALTER TABLE feedback_comments FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON feedback_comments USING(org_id=nullif(current_setting('app.org_id',true),'')::uuid) WITH CHECK(org_id=nullif(current_setting('app.org_id',true),'')::uuid);
GRANT SELECT,INSERT,UPDATE ON feedback_comments TO cosmoformer_app;
CREATE FUNCTION source_reconciliation_tenants() RETURNS SETOF uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE tenant uuid; previous text:=current_setting('app.org_id',true);
BEGIN
 FOR tenant IN SELECT id FROM organizations LOOP
  PERFORM set_config('app.org_id',tenant::text,true);
  IF EXISTS(SELECT FROM repositories WHERE enabled) THEN RETURN NEXT tenant; END IF;
 END LOOP;
 PERFORM set_config('app.org_id',coalesce(previous,''),true);
END $$;
REVOKE ALL ON FUNCTION source_reconciliation_tenants() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION source_reconciliation_tenants() TO cosmoformer_app;
