SET search_path = cosmoformer, pg_catalog;
CREATE TABLE artifacts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  attempt_id uuid NOT NULL,
  plan_id uuid,
  kind text NOT NULL,
  upload_id uuid NOT NULL,
  object_key text COLLATE "C" NOT NULL,
  version_id text NOT NULL,
  sha256 bytea NOT NULL CHECK(octet_length(sha256)=32),
  byte_count bigint NOT NULL CHECK(byte_count>=0),
  state text NOT NULL CHECK(state IN ('pending','finalized','deleted')),
  finalized_at timestamptz,
  deleted_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,attempt_id) REFERENCES attempts(org_id,id),
  FOREIGN KEY(org_id,plan_id) REFERENCES plans(org_id,id),
  UNIQUE(org_id,attempt_id,upload_id)
);
ALTER TABLE attempts ADD FOREIGN KEY(org_id,pre_init_spec_artifact_id) REFERENCES artifacts(org_id,id), ADD FOREIGN KEY(org_id,sealed_manifest_artifact_id) REFERENCES artifacts(org_id,id);
ALTER TABLE plans ADD FOREIGN KEY(org_id,manifest_artifact_id) REFERENCES artifacts(org_id,id);
CREATE TABLE backup_sets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  label text NOT NULL,
  state text NOT NULL CHECK(state IN ('preparing','sealed','released')),
  manifest_digest bytea,
  sealed_at timestamptz,
  released_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id)
);
CREATE TABLE artifact_holds (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  artifact_id uuid NOT NULL,
  purpose text NOT NULL CHECK(purpose IN ('active_attempt','pending_plan','ready_plan','recovery','backup','download')),
  subject_id uuid NOT NULL,
  released_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,artifact_id) REFERENCES artifacts(org_id,id)
);
CREATE UNIQUE INDEX ON artifact_holds(org_id,artifact_id,purpose,subject_id) WHERE released_at IS NULL;
CREATE TABLE cleanup_claims (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  artifact_id uuid NOT NULL UNIQUE,
  exact_version text NOT NULL,
  claim_id uuid NOT NULL UNIQUE,
  claimed_at timestamptz NOT NULL,
  completed_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,artifact_id) REFERENCES artifacts(org_id,id)
);
CREATE TABLE log_segments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  attempt_id uuid NOT NULL,
  artifact_id uuid,
  stream text NOT NULL,
  sequence bigint NOT NULL CHECK(sequence>=0),
  first_offset bigint NOT NULL CHECK(first_offset>=0),
  last_offset bigint NOT NULL CHECK(last_offset>=first_offset),
  gap boolean NOT NULL DEFAULT false,
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,attempt_id) REFERENCES attempts(org_id,id),
  FOREIGN KEY(org_id,artifact_id) REFERENCES artifacts(org_id,id),
  UNIQUE(org_id,attempt_id,stream,sequence)
);
CREATE TABLE module_entries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid,
  repository_id uuid NOT NULL,
  commit text NOT NULL,
  subdirectory text NOT NULL,
  name text NOT NULL,
  instructions text NOT NULL,
  example_links jsonb NOT NULL,
  archived boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  FOREIGN KEY(org_id,repository_id) REFERENCES repositories(org_id,id)
);
CREATE TABLE stack_dependencies(
  org_id uuid NOT NULL,
  stack_id uuid NOT NULL,
  depends_on_id uuid NOT NULL,
  PRIMARY KEY(org_id,stack_id,depends_on_id),
  CHECK(stack_id<>depends_on_id),
  FOREIGN KEY(org_id,stack_id) REFERENCES stacks(org_id,id),
  FOREIGN KEY(org_id,depends_on_id) REFERENCES stacks(org_id,id)
);
CREATE TABLE webhook_receipts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  integration_id uuid NOT NULL,
  delivery_id text NOT NULL,
  repository_id uuid NOT NULL,
  body_digest bytea NOT NULL CHECK(octet_length(body_digest)=32),
  accepted_at timestamptz NOT NULL,
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,integration_id) REFERENCES integrations(org_id,id),
  FOREIGN KEY(org_id,repository_id) REFERENCES repositories(org_id,id),
  UNIQUE(org_id,integration_id,delivery_id)
);
CREATE TABLE outbox (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  kind text NOT NULL,
  subject_id uuid NOT NULL,
  payload jsonb NOT NULL,
  status text NOT NULL CHECK(status IN ('pending','delivered','failed')),
  attempt_count bigint NOT NULL DEFAULT 0 CHECK(attempt_count>=0),
  next_attempt_at timestamptz NOT NULL,
  delivered_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id)
);
CREATE TABLE idempotency_records (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  actor_id uuid NOT NULL REFERENCES accounts(id),
  action text NOT NULL,
  subject_id uuid NOT NULL,
  key text NOT NULL,
  request_digest bytea NOT NULL CHECK(octet_length(request_digest)=32),
  result jsonb NOT NULL,
  expires_at timestamptz NOT NULL,
  UNIQUE(org_id,id),
  UNIQUE(org_id,actor_id,action,subject_id,key)
);
CREATE TABLE audit_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  actor_id uuid REFERENCES accounts(id),
  action text NOT NULL,
  subject_id uuid NOT NULL,
  facts jsonb NOT NULL DEFAULT '{}',
  UNIQUE(org_id,id)
);
CREATE TABLE identity_audit_events(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor_id uuid REFERENCES accounts(id),
  action text NOT NULL,
  subject_id uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ON projects(org_id,created_at,id);
CREATE INDEX ON environments(org_id,created_at,id);
CREATE INDEX ON repositories(org_id,created_at,id);
CREATE INDEX ON stacks(org_id,created_at,id);
CREATE INDEX ON module_entries(org_id,created_at,id);
CREATE INDEX ON runs(org_id,stack_id,created_at,id);
CREATE INDEX ON operations(org_id,status,priority,accepted_at,id);
CREATE INDEX ON outbox(org_id,status,next_attempt_at);
CREATE INDEX ON artifact_holds(org_id,artifact_id) WHERE released_at IS NULL;
CREATE INDEX ON grants(org_id,team_id,project_id);
CREATE INDEX ON team_members(org_id,account_id);
CREATE TABLE browser_requests(
  actor_id uuid NOT NULL REFERENCES accounts(id),
  key text NOT NULL,
  request_digest bytea NOT NULL CHECK(octet_length(request_digest)=32),
  result jsonb NOT NULL,
  once_visible boolean NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(actor_id,key)
);
ALTER TABLE artifacts ADD UNIQUE(org_id,attempt_id,id);
ALTER TABLE attempts ADD FOREIGN KEY(org_id,id,pre_init_spec_artifact_id) REFERENCES artifacts(org_id,attempt_id,id), ADD FOREIGN KEY(org_id,id,sealed_manifest_artifact_id) REFERENCES artifacts(org_id,attempt_id,id);
ALTER TABLE plans ADD FOREIGN KEY(org_id,attempt_id,manifest_artifact_id) REFERENCES artifacts(org_id,attempt_id,id);
