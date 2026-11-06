SET search_path = cosmoformer, pg_catalog;
CREATE TABLE backend_targets(
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  partition text COLLATE "C" NOT NULL,
  bucket text COLLATE "C" NOT NULL,
  effective_key text COLLATE "C" NOT NULL,
  state_epoch bigint NOT NULL DEFAULT 0 CHECK(state_epoch>=0),
  UNIQUE(partition,bucket,effective_key)
);
CREATE TABLE stack_bindings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  stack_id uuid NOT NULL UNIQUE,
  target_id uuid NOT NULL UNIQUE REFERENCES backend_targets(id),
  profile jsonb NOT NULL,
  bound_revision_id uuid NOT NULL,
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,stack_id) REFERENCES stacks(org_id,id),
  FOREIGN KEY(org_id,stack_id,bound_revision_id) REFERENCES stack_revisions(org_id,stack_id,id)
);
CREATE TABLE backend_resource_claims(
  target_id uuid NOT NULL REFERENCES backend_targets(id),
  kind text NOT NULL CHECK(kind IN ('s3_object','ddb_item')),
  namespace text COLLATE "C" NOT NULL,
  resource_key text COLLATE "C" NOT NULL,
  purpose text NOT NULL CHECK(purpose IN ('state','lock','checksum')),
  PRIMARY KEY(kind,namespace,resource_key)
);
CREATE TABLE source_subjects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  repository_id uuid NOT NULL,
  kind text NOT NULL CHECK(kind IN ('pr','branch')),
  external_key text NOT NULL,
  facts jsonb NOT NULL,
  facts_revision bigint NOT NULL DEFAULT 1,
  next_ticket bigint NOT NULL DEFAULT 1,
  accepted_ticket bigint NOT NULL DEFAULT 0,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,repository_id) REFERENCES repositories(org_id,id),
  UNIQUE(org_id,repository_id,kind,external_key)
);
CREATE TABLE source_observations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  subject_id uuid NOT NULL,
  ticket bigint NOT NULL,
  baseline_facts_revision bigint NOT NULL,
  resulting_facts_revision bigint NOT NULL,
  facts jsonb NOT NULL,
  platform_instance uuid NOT NULL,
  read_started_tick bigint NOT NULL,
  received_tick bigint NOT NULL,
  observed_at timestamptz NOT NULL,
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,subject_id) REFERENCES source_subjects(org_id,id),
  UNIQUE(org_id,subject_id,ticket)
);
CREATE TABLE runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  stack_id uuid NOT NULL,
  source_subject_id uuid NOT NULL,
  source_facts_revision bigint NOT NULL,
  source_commit text NOT NULL,
  purpose text NOT NULL CHECK(purpose IN ('pr_preview','pr_deploy','tracked','recovery')),
  requester_id uuid REFERENCES accounts(id),
  automation_repository_id uuid,
  UNIQUE(org_id,id),
  UNIQUE(org_id,stack_id,id),
  FOREIGN KEY(org_id,stack_id) REFERENCES stacks(org_id,id),
  FOREIGN KEY(org_id,source_subject_id) REFERENCES source_subjects(org_id,id),
  FOREIGN KEY(org_id,automation_repository_id) REFERENCES repositories(org_id,id),
  CHECK((requester_id IS NULL)<>(automation_repository_id IS NULL))
);
CREATE TABLE operations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  run_id uuid NOT NULL,
  stack_id uuid NOT NULL,
  kind text NOT NULL CHECK(kind IN ('plan','apply')),
  plan_id uuid,
  status text NOT NULL CHECK(status IN ('queued','preparing','executing','finalizing','succeeded','failed','cancelled','superseded','retired','needs_attention')),
  cancel_requested boolean NOT NULL DEFAULT false,
  priority text NOT NULL DEFAULT 'medium' CHECK(priority IN ('low','medium','high')),
  accepted_at timestamptz NOT NULL,
  ordering_at timestamptz NOT NULL,
  requester_id uuid REFERENCES accounts(id),
  automation_repository_id uuid,
  terminal_reason text,
  retired_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  UNIQUE(org_id,stack_id,id),
  FOREIGN KEY(org_id,stack_id,run_id) REFERENCES runs(org_id,stack_id,id),
  FOREIGN KEY(org_id,automation_repository_id) REFERENCES repositories(org_id,id),
  CHECK((kind='plan' AND plan_id IS NULL) OR (kind='apply' AND plan_id IS NOT NULL)),
  CHECK((requester_id IS NULL)<>(automation_repository_id IS NULL))
);
CREATE UNIQUE INDEX ON operations(org_id,plan_id) WHERE kind='apply' AND status IN ('queued','preparing','executing','finalizing','needs_attention');
CREATE TABLE work_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  operation_id uuid NOT NULL,
  phase text NOT NULL CHECK(phase IN ('preflight','execution')),
  status text NOT NULL CHECK(status IN ('queued','claimed','done','retired')),
  resolved_preflight jsonb,
  attempt_count bigint NOT NULL DEFAULT 0 CHECK(attempt_count>=0),
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  UNIQUE(org_id,operation_id,phase),
  UNIQUE(org_id,operation_id,id),
  FOREIGN KEY(org_id,operation_id) REFERENCES operations(org_id,id)
);
CREATE TABLE attempts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  work_item_id uuid NOT NULL,
  operation_id uuid NOT NULL,
  stack_id uuid NOT NULL,
  target_id uuid REFERENCES backend_targets(id),
  worker_id uuid NOT NULL,
  generation bigint NOT NULL CHECK(generation>=0),
  agent_instance uuid NOT NULL,
  state text NOT NULL CHECK(state IN ('claimed','preparing','ready','permitted','running','finalizing','uncertain','terminal')),
  outcome text CHECK(outcome IN ('success','failure','cancelled','preparation_failed','not_executed','unknown')),
  command_ordinal bigint NOT NULL DEFAULT 0 CHECK(command_ordinal>=0),
  ever_permitted boolean NOT NULL DEFAULT false,
  budget_platform_instance uuid NOT NULL,
  accepted_tick bigint NOT NULL,
  deadline_tick bigint NOT NULL,
  cap_seconds bigint NOT NULL CHECK(cap_seconds>0),
  display_deadline timestamptz NOT NULL,
  pre_init_spec_digest bytea,
  pre_init_spec_artifact_id uuid,
  sealed_manifest_digest bytea,
  sealed_manifest_artifact_id uuid,
  manifest_stage text NOT NULL DEFAULT 'pre_init' CHECK(manifest_stage IN ('pre_init','sealed')),
  closed_at timestamptz,
  stop_proof jsonb,
  result_digest bytea,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  UNIQUE(org_id,stack_id,id),
  UNIQUE(org_id,id,worker_id,generation,agent_instance),
  FOREIGN KEY(org_id,operation_id,work_item_id) REFERENCES work_items(org_id,operation_id,id),
  FOREIGN KEY(org_id,stack_id,operation_id) REFERENCES operations(org_id,stack_id,id),
  FOREIGN KEY(org_id,worker_id) REFERENCES workers(org_id,id),
  CHECK((state='terminal')=(closed_at IS NOT NULL)),
  CHECK((closed_at IS NULL)=(outcome IS NULL)),
  CHECK(pre_init_spec_digest IS NULL OR octet_length(pre_init_spec_digest)=32),
  CHECK(sealed_manifest_digest IS NULL OR octet_length(sealed_manifest_digest)=32)
);
CREATE UNIQUE INDEX ON attempts(org_id,work_item_id) WHERE closed_at IS NULL;
CREATE TABLE worker_reservations(
  org_id uuid NOT NULL,
  worker_id uuid PRIMARY KEY,
  attempt_id uuid NOT NULL UNIQUE,
  generation bigint NOT NULL,
  agent_instance uuid NOT NULL,
  FOREIGN KEY(org_id,attempt_id,worker_id,generation,agent_instance) REFERENCES attempts(org_id,id,worker_id,generation,agent_instance)
);
CREATE TABLE backend_reservations(
  org_id uuid NOT NULL,
  target_id uuid PRIMARY KEY REFERENCES backend_targets(id),
  attempt_id uuid NOT NULL UNIQUE,
  stack_id uuid NOT NULL,
  FOREIGN KEY(org_id,stack_id,attempt_id) REFERENCES attempts(org_id,stack_id,id)
);
CREATE TABLE start_permissions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  attempt_id uuid NOT NULL,
  ordinal bigint NOT NULL CHECK(ordinal>0),
  command text NOT NULL,
  command_digest bytea NOT NULL CHECK(octet_length(command_digest)=32),
  platform_instance uuid NOT NULL,
  expires_tick bigint NOT NULL,
  issued_at timestamptz NOT NULL,
  display_expires_at timestamptz NOT NULL,
  facts_revision bigint NOT NULL,
  observation_id uuid,
  status text NOT NULL CHECK(status IN ('issued','started','stopped','not_executed')),
  event_digest bytea,
  started_at timestamptz,
  stopped_at timestamptz,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,attempt_id) REFERENCES attempts(org_id,id),
  FOREIGN KEY(org_id,observation_id) REFERENCES source_observations(org_id,id),
  UNIQUE(org_id,attempt_id,ordinal)
);
CREATE TABLE plans (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  operation_id uuid NOT NULL UNIQUE,
  attempt_id uuid NOT NULL,
  stack_id uuid NOT NULL,
  target_id uuid NOT NULL REFERENCES backend_targets(id),
  status text NOT NULL CHECK(status IN ('assembling','ready','invalidated','expired','unsupported','recovery_only')),
  provenance text NOT NULL CHECK(provenance IN ('normal','recovery')),
  recovery_case_id uuid,
  source_facts_revision bigint NOT NULL,
  state_epoch bigint NOT NULL,
  manifest_artifact_id uuid,
  manifest_digest bytea,
  rule_version text NOT NULL,
  profile_id text NOT NULL,
  ready_at timestamptz,
  expires_at timestamptz,
  invalidation_reason text,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,stack_id,operation_id) REFERENCES operations(org_id,stack_id,id),
  FOREIGN KEY(org_id,stack_id,attempt_id) REFERENCES attempts(org_id,stack_id,id),
  CHECK((provenance='recovery')=(recovery_case_id IS NOT NULL)),
  CHECK(manifest_digest IS NULL OR octet_length(manifest_digest)=32)
);
ALTER TABLE operations ADD FOREIGN KEY(org_id,plan_id) REFERENCES plans(org_id,id);
CREATE TABLE recovery_cases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  stack_id uuid NOT NULL,
  target_id uuid NOT NULL REFERENCES backend_targets(id),
  origin_attempt_id uuid,
  status text NOT NULL CHECK(status IN ('open','cleared')),
  evidence_revision bigint NOT NULL DEFAULT 1,
  evidence jsonb NOT NULL,
  stop_verified boolean NOT NULL DEFAULT false,
  reconciliation_complete boolean NOT NULL DEFAULT false,
  opened_at timestamptz NOT NULL,
  cleared_at timestamptz,
  cleared_by uuid REFERENCES accounts(id),
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,stack_id) REFERENCES stacks(org_id,id),
  FOREIGN KEY(org_id,stack_id,origin_attempt_id) REFERENCES attempts(org_id,stack_id,id)
);
CREATE UNIQUE INDEX ON recovery_cases(target_id) WHERE status='open';
ALTER TABLE plans ADD FOREIGN KEY(org_id,recovery_case_id) REFERENCES recovery_cases(org_id,id);
CREATE TABLE approvals (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  subject_kind text NOT NULL CHECK(subject_kind IN ('plan','recovery')),
  subject_id uuid NOT NULL,
  subject_revision bigint NOT NULL,
  account_id uuid NOT NULL REFERENCES accounts(id),
  action text NOT NULL CHECK(action IN ('approve','revoke')),
  source_observation_id uuid,
  accepted_at timestamptz NOT NULL,
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,source_observation_id) REFERENCES source_observations(org_id,id)
);
CREATE TABLE plan_votes(
  org_id uuid NOT NULL,
  plan_id uuid NOT NULL,
  account_id uuid NOT NULL REFERENCES accounts(id),
  last_event_id uuid NOT NULL,
  approved boolean NOT NULL,
  subject_revision bigint NOT NULL,
  PRIMARY KEY(org_id,plan_id,account_id),
  FOREIGN KEY(org_id,plan_id) REFERENCES plans(org_id,id),
  FOREIGN KEY(org_id,last_event_id) REFERENCES approvals(org_id,id)
);
CREATE TABLE recovery_votes(
  org_id uuid NOT NULL,
  case_id uuid NOT NULL,
  account_id uuid NOT NULL REFERENCES accounts(id),
  last_event_id uuid NOT NULL,
  approved boolean NOT NULL,
  evidence_revision bigint NOT NULL,
  PRIMARY KEY(org_id,case_id,account_id),
  FOREIGN KEY(org_id,case_id) REFERENCES recovery_cases(org_id,id),
  FOREIGN KEY(org_id,last_event_id) REFERENCES approvals(org_id,id)
);
CREATE TABLE control_requests (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  attempt_id uuid NOT NULL,
  actor_id uuid REFERENCES accounts(id),
  kind text NOT NULL CHECK(kind IN ('cancel','force_stop','deadline_cancel')),
  command_ordinal bigint,
  confirmed boolean NOT NULL,
  state text NOT NULL CHECK(state IN ('requested','delivered','observed')),
  requested_at timestamptz NOT NULL,
  observed_event_id uuid,
  revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,attempt_id) REFERENCES attempts(org_id,id),
  CHECK(kind<>'force_stop' OR confirmed)
);
CREATE TABLE attempt_messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  org_id uuid NOT NULL REFERENCES cosmoformer.organizations(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  attempt_id uuid NOT NULL,
  message_id uuid NOT NULL,
  sequence bigint NOT NULL CHECK(sequence>=0),
  command_ordinal bigint NOT NULL CHECK(command_ordinal>=0),
  kind text NOT NULL,
  digest bytea NOT NULL CHECK(octet_length(digest)=32),
  payload jsonb NOT NULL,
  UNIQUE(org_id,id),
  FOREIGN KEY(org_id,attempt_id) REFERENCES attempts(org_id,id),
  UNIQUE(org_id,attempt_id,message_id),
  UNIQUE(org_id,attempt_id,sequence)
);
ALTER TABLE control_requests ADD FOREIGN KEY(org_id,observed_event_id) REFERENCES attempt_messages(org_id,id);
-- An owning stack/target chain cannot be crossed by later execution code.
ALTER TABLE stack_bindings ADD UNIQUE(org_id,stack_id,target_id);
ALTER TABLE attempts ADD UNIQUE(org_id,stack_id,id,target_id), ADD FOREIGN KEY(org_id,stack_id,target_id) REFERENCES stack_bindings(org_id,stack_id,target_id);
ALTER TABLE backend_reservations ADD FOREIGN KEY(org_id,stack_id,attempt_id,target_id) REFERENCES attempts(org_id,stack_id,id,target_id);
ALTER TABLE plans ADD UNIQUE(org_id,stack_id,id), ADD FOREIGN KEY(org_id,stack_id,target_id) REFERENCES stack_bindings(org_id,stack_id,target_id);
ALTER TABLE operations ADD FOREIGN KEY(org_id,stack_id,plan_id) REFERENCES plans(org_id,stack_id,id);
CREATE UNIQUE INDEX ON start_permissions(org_id,attempt_id) WHERE status IN ('issued','started');
