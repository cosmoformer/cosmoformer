SET search_path=cosmoformer,pg_catalog,pg_temp;
ALTER TABLE installation ADD COLUMN maintenance_id uuid,
 ADD COLUMN offline boolean NOT NULL DEFAULT false,
 ADD COLUMN restore_id uuid,
 ADD COLUMN restore_reconciled boolean NOT NULL DEFAULT false,
 ADD COLUMN restore_key_required boolean NOT NULL DEFAULT false;
-- Installation metadata, receipts and manifests are host-only. Tenant tables
-- retain FORCE RLS; no global tenant bypass function is introduced.
CREATE TABLE operator_events(
 id uuid PRIMARY KEY, maintenance_id uuid NOT NULL, action text NOT NULL,
 evidence jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE key_rotations(
 id uuid PRIMARY KEY, maintenance_id uuid NOT NULL,
 old_fingerprint bytea NOT NULL CHECK(octet_length(old_fingerprint)=32),
 new_fingerprint bytea NOT NULL REFERENCES crypto_keys(fingerprint),
 reserved_encryptions bigint NOT NULL CHECK(reserved_encryptions>=0),
 committed_at timestamptz, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE backup_artifacts(
 backup_id uuid NOT NULL REFERENCES backup_sets(id), org_id uuid NOT NULL,
 artifact_id uuid NOT NULL, bucket text NOT NULL, object_key text NOT NULL,
 version_id text NOT NULL, sha256 bytea NOT NULL, byte_count bigint NOT NULL,
 PRIMARY KEY(backup_id,org_id,artifact_id),
 FOREIGN KEY(org_id,artifact_id) REFERENCES artifacts(org_id,id)
);
ALTER TABLE backup_artifacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE backup_artifacts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope ON backup_artifacts USING(org_id=nullif(current_setting('app.org_id',true),'')::uuid)
 WITH CHECK(org_id=nullif(current_setting('app.org_id',true),'')::uuid);
REVOKE ALL ON operator_events,key_rotations,backup_artifacts FROM PUBLIC,cosmoformer_app;
-- A durable offline latch fences late app writes (including definer functions)
-- after the operator has stopped the normal writers. Maintenance by itself
-- deliberately still permits authenticated evidence and stop reports.
CREATE FUNCTION operator_write_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF (session_user<>'cosmoformer_owner' OR current_user<>'cosmoformer_owner') AND EXISTS(SELECT FROM installation WHERE offline) THEN
  RAISE EXCEPTION 'OFFLINE_MAINTENANCE' USING ERRCODE='42501';
 END IF;
 RETURN coalesce(NEW,OLD);
END $$;
DO $$ DECLARE t record; BEGIN
 FOR t IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='cosmoformer' AND c.relkind='r' AND c.relname NOT IN ('coordination_gate','schema_version') LOOP
  EXECUTE format('CREATE TRIGGER operator_write_guard BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION operator_write_guard()',t.relname);
 END LOOP;
END $$;
-- A surviving process with a cached quota must never commit old-key ciphertext.
CREATE FUNCTION current_ciphertext_guard() RETURNS trigger LANGUAGE plpgsql SET search_path=pg_catalog,cosmoformer,pg_temp AS $$
DECLARE envelope bytea;
BEGIN
 PERFORM id FROM coordination_gate FOR UPDATE;
 IF TG_TABLE_NAME='integrations' THEN envelope:=NEW.encrypted_credentials; ELSE envelope:=NEW.encrypted_value; END IF;
 IF (session_user<>'cosmoformer_owner' OR current_user<>'cosmoformer_owner') AND envelope IS NOT NULL AND
   (get_byte(envelope,0)<>1 OR substring(envelope FROM 2 FOR 32) IS DISTINCT FROM (SELECT active_key_fingerprint FROM installation)) THEN
  RAISE EXCEPTION 'KEY_UNAVAILABLE' USING ERRCODE='42501';
 END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER current_ciphertext_guard BEFORE INSERT OR UPDATE OF encrypted_value ON secrets FOR EACH ROW EXECUTE FUNCTION current_ciphertext_guard();
CREATE TRIGGER current_ciphertext_guard BEFORE INSERT OR UPDATE OF encrypted_value ON source_credentials FOR EACH ROW EXECUTE FUNCTION current_ciphertext_guard();
CREATE TRIGGER current_ciphertext_guard BEFORE INSERT OR UPDATE OF encrypted_credentials ON integrations FOR EACH ROW EXECUTE FUNCTION current_ciphertext_guard();
REVOKE ALL ON FUNCTION operator_write_guard(),current_ciphertext_guard() FROM PUBLIC;
