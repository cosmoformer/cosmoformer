SET search_path = cosmoformer, pg_catalog;
GRANT USAGE ON SCHEMA cosmoformer TO cosmoformer_app;
GRANT SELECT ON installation, coordination_gate TO cosmoformer_app;
GRANT UPDATE ON coordination_gate TO cosmoformer_app;
-- Global identity data is accessible only through the bounded functions below.
CREATE FUNCTION require_admin(actor uuid) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 IF NOT EXISTS(SELECT FROM accounts WHERE id=actor AND enabled AND installation_admin) THEN RAISE EXCEPTION 'FORBIDDEN' USING ERRCODE='42501'; END IF;
END $$;
CREATE FUNCTION login_identity(name text) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT jsonb_build_object('id',id,'username',username,'display_name',display_name,'enabled',enabled,'installation_admin',installation_admin,'password_phc',password_phc,'credential_revision',credential_revision,'revision',revision) FROM accounts WHERE username=name;
$$;
CREATE FUNCTION session_identity(hash bytea) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT jsonb_build_object('id',a.id,'username',a.username,'display_name',a.display_name,'installation_admin',a.installation_admin,'session_id',s.id,'csrf_verifier',encode(s.csrf_verifier,'hex'),'expires_at',least(s.absolute_expires_at,s.last_activity_at+interval '1 hour'))
 FROM sessions s JOIN accounts a ON a.id=s.account_id
 WHERE s.verifier=hash AND a.enabled AND a.credential_revision=s.credential_revision AND s.absolute_expires_at>clock_timestamp() AND s.last_activity_at+interval '1 hour'>clock_timestamp();
$$;
CREATE FUNCTION create_session(actor uuid, expected bigint, hash bytea, csrf bytea) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF NOT EXISTS(SELECT FROM accounts WHERE id=actor AND credential_revision=expected AND enabled AND password_phc IS NOT NULL) THEN RETURN false; END IF;
 DELETE FROM sessions WHERE absolute_expires_at<=clock_timestamp() OR last_activity_at+interval '1 hour'<=clock_timestamp();
 -- Bound sessions per account without allowing unbounded successful-login growth.
 DELETE FROM sessions WHERE id IN (SELECT id FROM sessions WHERE account_id=actor ORDER BY created_at DESC OFFSET 19);
 INSERT INTO sessions(account_id,credential_revision,verifier,csrf_verifier) VALUES(actor,expected,hash,csrf);
 RETURN true;
END $$;
CREATE FUNCTION session_activity(hash bytea) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 IF session_identity(hash) IS NULL THEN RETURN false; END IF;
 UPDATE sessions SET last_activity_at=clock_timestamp() WHERE verifier=hash AND absolute_expires_at>clock_timestamp() AND last_activity_at+interval '1 hour'>clock_timestamp();
 RETURN FOUND;
END $$;
CREATE FUNCTION session_logout(hash bytea) RETURNS void LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$ DELETE FROM sessions WHERE verifier=hash; $$;
CREATE FUNCTION auth_rate(subject_hash bytea, outcome text) RETURNS integer LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE r auth_throttles; n timestamptz:=clock_timestamp();
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 DELETE FROM auth_throttles WHERE updated_at<n-interval '30 minutes';
 SELECT * INTO r FROM auth_throttles WHERE subject=subject_hash;
 IF r.blocked_until>n THEN RETURN ceil(extract(epoch FROM r.blocked_until-n))::integer; END IF;
 IF outcome='success' THEN DELETE FROM auth_throttles WHERE subject=subject_hash; RETURN 0; END IF;
 IF outcome='check' THEN RETURN 0; END IF;
 IF outcome<>'failure' THEN RAISE EXCEPTION 'invalid throttle outcome'; END IF;
 IF r.subject IS NULL THEN
  IF (SELECT count(*) FROM auth_throttles)>=10000 THEN RETURN 60; END IF;
  INSERT INTO auth_throttles(subject,failures,window_at) VALUES(subject_hash,1,n);
 ELSE
  UPDATE auth_throttles SET failures=CASE WHEN window_at<n-interval '15 minutes' THEN 1 ELSE failures+1 END,
   window_at=CASE WHEN window_at<n-interval '15 minutes' THEN n ELSE window_at END,updated_at=n,
   blocked_until=CASE WHEN window_at>=n-interval '15 minutes' AND failures+1>=5 THEN n+interval '15 minutes' ELSE NULL END
   WHERE subject=subject_hash;
 END IF;
 RETURN 0;
END $$;
CREATE FUNCTION list_accounts(actor uuid) RETURNS jsonb LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 PERFORM require_admin(actor);
 RETURN coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'username',username,'display_name',display_name,'enabled',enabled,'installation_admin',installation_admin,'pending',password_phc IS NULL,'revision',revision) ORDER BY username) FROM accounts),'[]');
END $$;
CREATE FUNCTION create_account(actor uuid,name text,label text) RETURNS uuid LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE target uuid;
BEGIN
 PERFORM require_admin(actor);
 INSERT INTO accounts(username,display_name) VALUES(name,label) RETURNING id INTO target;
 INSERT INTO identity_audit_events(actor_id,action,subject_id) VALUES(actor,'account.create',target);
 RETURN target;
END $$;
CREATE FUNCTION update_account(actor uuid,target uuid,expected bigint,new_enabled boolean,new_admin boolean,label text) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE old accounts;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE; PERFORM require_admin(actor);
 SELECT * INTO old FROM accounts WHERE id=target;
 IF old.id IS NULL OR old.revision<>expected THEN RETURN false; END IF;
 IF old.enabled AND old.installation_admin AND (NOT new_enabled OR NOT new_admin) AND NOT EXISTS(SELECT FROM accounts WHERE id<>target AND enabled AND installation_admin) THEN RAISE EXCEPTION 'LAST_ADMIN' USING ERRCODE='23514'; END IF;
 UPDATE accounts SET enabled=new_enabled,installation_admin=new_admin,display_name=label,revision=revision+1,credential_revision=credential_revision+CASE WHEN enabled<>new_enabled THEN 1 ELSE 0 END WHERE id=target;
 IF NOT new_enabled THEN DELETE FROM sessions WHERE account_id=target; DELETE FROM account_proofs WHERE account_id=target; END IF;
 INSERT INTO identity_audit_events(actor_id,action,subject_id) VALUES(actor,'account.update',target);
 RETURN true;
END $$;
CREATE FUNCTION issue_proof(actor uuid,target uuid,kind text,hash bytea) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE a accounts;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE; PERFORM require_admin(actor);
 SELECT * INTO a FROM accounts WHERE id=target;
 IF a.id IS NULL OR NOT a.enabled OR kind NOT IN ('setup','reset') OR (kind='setup' AND a.password_phc IS NOT NULL) OR (kind='reset' AND a.password_phc IS NULL) THEN RETURN false; END IF;
 DELETE FROM account_proofs WHERE account_id=target AND purpose=kind;
 INSERT INTO account_proofs(account_id,purpose,verifier,credential_revision,expires_at) VALUES(target,kind,hash,a.credential_revision,clock_timestamp()+CASE WHEN kind='setup' THEN interval '24 hours' ELSE interval '30 minutes' END);
 INSERT INTO identity_audit_events(actor_id,action,subject_id) VALUES(actor,'account.'||kind||'_code',target);
 RETURN true;
END $$;
CREATE FUNCTION consume_proof(hash bytea,kind text,new_phc text) RETURNS boolean LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
DECLARE p account_proofs;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 SELECT * INTO p FROM account_proofs WHERE verifier=hash AND purpose=kind AND expires_at>clock_timestamp();
 IF p.id IS NULL THEN RETURN false; END IF;
 UPDATE accounts SET password_phc=new_phc,credential_revision=credential_revision+1,revision=revision+1 WHERE id=p.account_id AND enabled AND credential_revision=p.credential_revision;
 IF NOT FOUND THEN RETURN false; END IF;
 DELETE FROM account_proofs WHERE account_id=p.account_id; DELETE FROM sessions WHERE account_id=p.account_id;
 DELETE FROM auth_throttles WHERE subject=(SELECT sha256(convert_to('login/'||username,'UTF8')) FROM accounts WHERE id=p.account_id);
 INSERT INTO identity_audit_events(action,subject_id) VALUES('account.'||kind||'_complete',p.account_id);
 RETURN true;
END $$;
CREATE FUNCTION account_directory(actor uuid) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT jsonb_build_object('organizations',coalesce((SELECT jsonb_agg(jsonb_build_object('id',o.id,'name',o.name,'org_admin',coalesce(m.org_admin,false) OR a.installation_admin) ORDER BY o.name) FROM organizations o JOIN accounts a ON a.id=actor LEFT JOIN memberships m ON m.org_id=o.id AND m.account_id=actor WHERE m.account_id=actor OR a.installation_admin),'[]'),
 'invitations',coalesce((SELECT jsonb_agg(jsonb_build_object('id',i.id,'org_id',o.id,'name',o.name) ORDER BY i.created_at) FROM invitations i JOIN organizations o ON o.id=i.org_id WHERE i.account_id=actor AND i.accepted_at IS NULL),'[]'));
$$;
-- Unscoped lookup helpers above bypass only their narrowly selected directory
-- tables as owner. All other tenant tables FORCE RLS, including for their owner.
DO $$ DECLARE r record; BEGIN
 FOR r IN SELECT table_name FROM information_schema.columns WHERE table_schema='cosmoformer' AND column_name='org_id' LOOP
  EXECUTE format('ALTER TABLE cosmoformer.%I ENABLE ROW LEVEL SECURITY',r.table_name);
  IF r.table_name NOT IN ('memberships','invitations') THEN EXECUTE format('ALTER TABLE cosmoformer.%I FORCE ROW LEVEL SECURITY',r.table_name); END IF;
  EXECUTE format('CREATE POLICY tenant_scope ON cosmoformer.%I USING (org_id=nullif(current_setting(''app.org_id'',true),'''')::uuid) WITH CHECK (org_id=nullif(current_setting(''app.org_id'',true),'''')::uuid)',r.table_name);
  EXECUTE format('GRANT SELECT,INSERT,UPDATE,DELETE ON cosmoformer.%I TO cosmoformer_app',r.table_name);
 END LOOP;
END $$;
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON organizations USING(id=nullif(current_setting('app.org_id',true),'')::uuid) WITH CHECK(id=nullif(current_setting('app.org_id',true),'')::uuid);
GRANT SELECT,INSERT,UPDATE ON organizations TO cosmoformer_app;
-- Append-only evidence/history; permanent namespace custody has no app DELETE.
REVOKE UPDATE,DELETE ON audit_events,approvals,source_observations,stack_revisions,runs,attempt_messages,log_segments,webhook_receipts FROM cosmoformer_app;
REVOKE UPDATE,DELETE ON stack_bindings FROM cosmoformer_app;
REVOKE ALL ON backend_targets,backend_resource_claims FROM cosmoformer_app;
CREATE FUNCTION member_labels() RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT coalesce(jsonb_agg(jsonb_build_object('id',a.id,'username',a.username,'display_name',a.display_name,'org_admin',m.org_admin,'revision',m.revision) ORDER BY a.username),'[]') FROM memberships m JOIN accounts a ON a.id=m.account_id WHERE m.org_id=nullif(current_setting('app.org_id',true),'')::uuid;
$$;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA cosmoformer FROM PUBLIC;
GRANT EXECUTE ON FUNCTION member_labels(),login_identity(text),session_identity(bytea),create_session(uuid,bigint,bytea,bytea),session_activity(bytea),session_logout(bytea),auth_rate(bytea,text),list_accounts(uuid),create_account(uuid,text,text),update_account(uuid,uuid,bigint,boolean,boolean,text),issue_proof(uuid,uuid,text,bytea),consume_proof(bytea,text,text),account_directory(uuid) TO cosmoformer_app;
CREATE FUNCTION browser_request(actor uuid,request_key text) RETURNS jsonb LANGUAGE sql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
 SELECT jsonb_build_object('digest',encode(request_digest,'hex'),'result',result,'once_visible',once_visible) FROM browser_requests WHERE actor_id=actor AND key=request_key;
$$;
CREATE FUNCTION save_browser_request(actor uuid,request_key text,digest bytea,response jsonb,once_only boolean) RETURNS void LANGUAGE plpgsql SECURITY DEFINER SET search_path=pg_catalog,cosmoformer AS $$
BEGIN
 DELETE FROM browser_requests WHERE created_at<clock_timestamp()-interval '24 hours';
 IF (SELECT count(*) FROM browser_requests)>=100000 THEN RAISE EXCEPTION 'request capacity exceeded' USING ERRCODE='53300'; END IF;
 INSERT INTO browser_requests(actor_id,key,request_digest,result,once_visible) VALUES(actor,request_key,digest,CASE WHEN once_only THEN '{}'::jsonb ELSE response END,once_only);
END $$;
REVOKE ALL ON FUNCTION browser_request(uuid,text),save_browser_request(uuid,text,bytea,jsonb,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION browser_request(uuid,text),save_browser_request(uuid,text,bytea,jsonb,boolean) TO cosmoformer_app;
ALTER DEFAULT PRIVILEGES FOR ROLE cosmoformer_owner IN SCHEMA cosmoformer REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
