CREATE SCHEMA IF NOT EXISTS cosmoformer AUTHORIZATION cosmoformer_owner;
REVOKE ALL ON SCHEMA cosmoformer FROM PUBLIC;
SET search_path = cosmoformer, pg_catalog;
CREATE TABLE installation (

 id boolean PRIMARY KEY DEFAULT true CHECK(id),
  bootstrap_completed boolean NOT NULL DEFAULT false,

 maintenance boolean NOT NULL DEFAULT false,
  authority_highwater timestamptz NOT NULL DEFAULT now(),

 clock_unreliable boolean NOT NULL DEFAULT false,
  active_key_fingerprint bytea CHECK(octet_length(active_key_fingerprint)=32)

);
INSERT INTO installation(id) VALUES(true);
CREATE TABLE coordination_gate (
  id boolean PRIMARY KEY DEFAULT true CHECK(id)
);
INSERT INTO coordination_gate(id) VALUES(true);
CREATE TABLE crypto_keys(
  fingerprint bytea PRIMARY KEY CHECK(octet_length(fingerprint)=32),
  reserved_encryptions bigint NOT NULL DEFAULT 0 CHECK(reserved_encryptions BETWEEN 0 AND 4294967296),
  state text NOT NULL CHECK(state IN ('active','retired','rotation')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE accounts (

 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  username text NOT NULL UNIQUE CHECK(username ~ '^[a-z][a-z0-9._-]{2,63}$'),

 display_name text NOT NULL CHECK(char_length(display_name) BETWEEN 1 AND 100),
  enabled boolean NOT NULL DEFAULT true,

 installation_admin boolean NOT NULL DEFAULT false,
  password_phc text,
  credential_revision bigint NOT NULL DEFAULT 1 CHECK(credential_revision>0),

 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  created_at timestamptz NOT NULL DEFAULT now()

);
CREATE TABLE sessions (

 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES accounts(id),

 verifier bytea NOT NULL UNIQUE CHECK(octet_length(verifier)=32),
  csrf_verifier bytea NOT NULL CHECK(octet_length(csrf_verifier)=32),

 credential_revision bigint NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  last_activity_at timestamptz NOT NULL DEFAULT now(),

 absolute_expires_at timestamptz NOT NULL DEFAULT now()+interval '12 hours'

);
CREATE INDEX ON sessions(account_id); CREATE INDEX ON sessions(absolute_expires_at);
CREATE TABLE account_proofs (

 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES accounts(id),

 purpose text NOT NULL CHECK(purpose IN ('setup','reset')),
  verifier bytea NOT NULL UNIQUE CHECK(octet_length(verifier)=32),

 credential_revision bigint NOT NULL,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(account_id,purpose)

);
CREATE INDEX ON account_proofs(expires_at);
CREATE TABLE auth_throttles (
  subject bytea PRIMARY KEY CHECK(octet_length(subject)=32),
  failures integer NOT NULL CHECK(failures>=0),
  window_at timestamptz NOT NULL,
  blocked_until timestamptz,
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE organizations(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 100),
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE memberships(
  org_id uuid NOT NULL REFERENCES organizations(id),
  account_id uuid NOT NULL REFERENCES accounts(id),
  org_admin boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(org_id,account_id)
);
CREATE TABLE invitations(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES organizations(id),
  account_id uuid NOT NULL REFERENCES accounts(id),
  org_admin boolean NOT NULL DEFAULT false,
  accepted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id,id)
);
CREATE UNIQUE INDEX ON invitations(org_id,account_id) WHERE accepted_at IS NULL;
CREATE TABLE teams(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES organizations(id),
  name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 100),
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id,id),
  UNIQUE(org_id,name)
);
CREATE TABLE team_members(
  org_id uuid NOT NULL,
  team_id uuid NOT NULL,
  account_id uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(org_id,team_id,account_id),
  FOREIGN KEY(org_id,team_id) REFERENCES teams(org_id,id),
  FOREIGN KEY(org_id,account_id) REFERENCES memberships(org_id,account_id)
);
CREATE TABLE projects(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES organizations(id),
  name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 100),
  archived boolean NOT NULL DEFAULT false,
  minimum_approvals integer NOT NULL DEFAULT 1 CHECK(minimum_approvals BETWEEN 1 AND 100),
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id,id)
);
CREATE TABLE environments(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL,
  project_id uuid NOT NULL,
  name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 100),
  archived boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id,id),
  UNIQUE(org_id,project_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id)
);
CREATE TABLE grants(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL,
  team_id uuid NOT NULL,
  project_id uuid NOT NULL,
  environment_id uuid,
  revoked_at timestamptz,
  role text NOT NULL CHECK(role IN ('viewer','planner','deployer','administrator','approver','raw_evidence_read')),
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,team_id) REFERENCES teams(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  FOREIGN KEY(org_id,project_id,environment_id) REFERENCES environments(org_id,project_id,id)
);
CREATE TABLE runtime_catalog(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  engine text NOT NULL CHECK(engine IN ('terraform','opentofu')),
  version text NOT NULL,
  architecture text NOT NULL,
  image_digest text NOT NULL,
  approved boolean NOT NULL DEFAULT false,
  withdrawn boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(engine,version,architecture,image_digest)
);
CREATE TABLE integrations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  name text NOT NULL,
  provider text NOT NULL CHECK(provider='bitbucket_cloud'),
  encrypted_credentials bytea,
  semantic_revision bigint NOT NULL DEFAULT 1,
  enabled boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id)
);
CREATE TABLE repositories (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid NOT NULL,
  integration_id uuid NOT NULL,
  external_id text NOT NULL,
  url text NOT NULL,
  tracked_branch text NOT NULL,
  inventory_commit text,
  enabled boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  FOREIGN KEY(org_id,integration_id) REFERENCES integrations(org_id,id),
  UNIQUE(org_id,id,project_id)
);
CREATE TABLE worker_pools (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  name text NOT NULL,
  enabled boolean NOT NULL DEFAULT true,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id)
);
CREATE TABLE source_credentials (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid,
  repository_id uuid NOT NULL,
  encrypted_value bytea NOT NULL,
  semantic_revision bigint NOT NULL DEFAULT 1,
  enabled boolean NOT NULL DEFAULT true,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  FOREIGN KEY(org_id,repository_id) REFERENCES repositories(org_id,id)
);
CREATE TABLE secrets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid,
  name text NOT NULL,
  encrypted_value bytea NOT NULL,
  semantic_revision bigint NOT NULL DEFAULT 1,
  enabled boolean NOT NULL DEFAULT true,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  UNIQUE NULLS NOT DISTINCT(org_id,project_id,name)
);
CREATE TABLE resource_grants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid NOT NULL,
  environment_id uuid,
  kind text NOT NULL CHECK(kind IN ('repository','pool','secret','s3_object','ddb_table')),
  resource_id uuid,
  namespace text COLLATE "C",
  resource_key text COLLATE "C",
  enabled boolean NOT NULL DEFAULT true,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  FOREIGN KEY(org_id,project_id,environment_id) REFERENCES environments(org_id,project_id,id)
);
CREATE TABLE aws_connections (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid NOT NULL,
  environment_id uuid,
  role_arn text NOT NULL,
  expected_account text NOT NULL,
  enabled boolean NOT NULL DEFAULT false,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,project_id) REFERENCES projects(org_id,id),
  FOREIGN KEY(org_id,project_id,environment_id) REFERENCES environments(org_id,project_id,id)
);
CREATE UNIQUE INDEX ON aws_connections(org_id,project_id,environment_id) NULLS NOT DISTINCT WHERE enabled;
CREATE TABLE workers (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  pool_id uuid NOT NULL,
  name text NOT NULL,
  verifier bytea NOT NULL CHECK(octet_length(verifier)=32),
  generation bigint NOT NULL DEFAULT 0,
  agent_instance uuid,
  paused boolean NOT NULL DEFAULT true,
  runtime_id uuid REFERENCES runtime_catalog(id),
  capabilities jsonb NOT NULL DEFAULT '{}',
  last_contact_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,pool_id) REFERENCES worker_pools(org_id,id)
);
CREATE TABLE stacks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  project_id uuid NOT NULL,
  environment_id uuid NOT NULL,
  repository_id uuid NOT NULL,
  declaration_id uuid NOT NULL,
  display_name text NOT NULL,
  archived boolean NOT NULL DEFAULT false,
  effective_revision_id uuid,
  assessment text NOT NULL DEFAULT 'not_assessed' CHECK(assessment IN ('not_assessed','compatible','incompatible')),
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  UNIQUE(org_id,repository_id,declaration_id),
  UNIQUE(org_id,project_id,id),
  FOREIGN KEY(org_id,project_id,environment_id) REFERENCES environments(org_id,project_id,id),
  FOREIGN KEY(org_id,repository_id,project_id) REFERENCES repositories(org_id,id,project_id)
);
CREATE TABLE stack_revisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  stack_id uuid NOT NULL,
  commit text NOT NULL,
  parsed_config jsonb NOT NULL,
  effective_config jsonb NOT NULL,
  semantic_digest bytea NOT NULL CHECK(octet_length(semantic_digest)=32),
  inventory_revision bigint NOT NULL,
  UNIQUE(org_id,id),
  UNIQUE(org_id,stack_id,id),
  FOREIGN KEY(org_id,stack_id) REFERENCES stacks(org_id,id)
);
ALTER TABLE stacks ADD FOREIGN KEY(org_id,id,effective_revision_id) REFERENCES stack_revisions(org_id,stack_id,id);

CREATE UNIQUE INDEX ON grants(org_id,team_id,project_id,environment_id,role) NULLS NOT DISTINCT WHERE revoked_at IS NULL;
