# Local source-preflight procedures

This guide describes the implemented account and source-preflight slices. There is no native backend initialization, state download, provider execution, AWS role assumption, infrastructure plan or apply path. Company adoption, production packaging, native compatibility and full recovery remain later work.

## Verify locally

Run the README setup and test prerequisites, then:

```sh
make test-db-start
make check
make test-db-stop
```

`test-unit` includes Go race checks and static checks. `test-integration` runs against generated, marker-verified PostgreSQL databases using the real restricted runtime role. It covers tenant isolation, version-4/5-to-6 forward migrations, permanent ownership races, source fencing, encryption quotas, OAuth session binding, single-flight refresh, signed webhook deduplication and the local worker API journey. The worker journey replaces only external Git I/O; the separate Linux suite exercises actual Git and process handling.

`test-source` builds the pinned Linux image, verifies its native executable and Git package identity and runs loopback-only Git HTTPS tests with `--network none`. It mounts no database DSN, cloud credential, home directory or Docker socket. Downloads occur during the explicit build, with pinned base images, signed Debian snapshot metadata and exact archive checksums. The native executable is invoked only for version identification. Passing this suite does not approve an S3/DynamoDB backend profile or an AWS provider version for execution.

`test-web` checks the generated contract, TypeScript, frontend tests and coverage, the static build and the Chromium/Firefox account journeys. It requires no cloud credentials. A selected suite fails on missing prerequisites; it does not fall back to a development/application database.

## Schema and encryption key

Schema version 5 is a forward correction to the original version-4 foundation. It adds the narrow backend-binding, worker-identity, encryption-quota and OAuth-state helpers, immutable assignment snapshots, independent request receipts, worker tags and source-integration bookkeeping. The original four migrations remain unchanged. Serving never migrates implicitly.

Schema version 6 corrects evidence metadata before native planning is implemented. Migration 006 leaves 001–005 unchanged. Artifacts now have uploading/finalized/deleting/deleted/unavailable states, bucket identity, nullable incomplete-upload metadata, required exact version/hash/size at finalization, and deletion tombstones. Finalized object identity is immutable. Artifacts retain their original attempt and its owning operation; plan associations must belong to that same operation. This permits an assembling plan to rebind to a replacement preparation attempt while retaining immutable evidence from the failed attempt. Reviewed manifests, attempt attestations and log artifacts still require the exact owning attempt. Logs require either an artifact or an explicit gap.

A failed upload with a known exact version can enter deleting after its owning attempt closes, its holds are released and an exact-version cleanup claim exists. It can then receive a deletion tombstone without fabricating a finalization timestamp, hash or byte count. Claimed object identity is immutable, including for unfinalized uploads; deleting evidence cannot become usable again. This schema path does not implement cleanup eligibility scheduling or object deletion.

Holds reference an existing same-tenant plan, accepted operation, attempt, or recovery case, or an installation-wide backup capture. Active uniqueness includes the reason; released history remains. Backup captures record cutoff, digest and nonnegative artifact count, are owner-only, and retain tenant-scoped holds readable but not writable by the runtime. Cleanup claims reference the artifact's exact version, record retry outcomes, and block new holds. Evidence mutations take the existing coordination gate; held artifacts cannot be claimed or marked deleting/deleted. Startup checks the version and evidence protection boundaries. These are schema safeguards; automatic hold acquisition, retention eligibility, artifact storage, cleanup and backup commands remain unimplemented.

Migration 006 refuses a database containing version-5 artifacts or backup sets with `EVIDENCE_METADATA_RECONCILIATION_REQUIRED`. Version 5 lacks bucket and installation capture metadata; neither can be safely inferred. The migration visits each tenant under FORCE RLS and fails transactionally without advancing the version. Preserve the database and reconcile verified metadata through a separately reviewed migration before retrying; do not delete evidence to bypass this guard. Databases containing account/source/preflight/binding records without these legacy evidence rows upgrade directly. No backup capture or evidence deletion occurs during migration.

For a local installation that will store managed secrets, create a fresh raw 32-byte key outside Git and keep a separate protected backup. For example, from the repository root:

```sh
openssl rand -out .build/dev/encryption.key 32
chmod 600 .build/dev/encryption.key
COSMOFORMER_MIGRATION_DSN_FILE="$PWD/.build/dev/owner.dsn" \
  .build/tools/go/bin/go run ./cmd/cosmoformer migrate
COSMOFORMER_MIGRATION_DSN_FILE="$PWD/.build/dev/owner.dsn" \
  COSMOFORMER_ENCRYPTION_KEY_FILE="$PWD/.build/dev/encryption.key" \
  .build/tools/go/bin/go run ./cmd/cosmoformer register-key
```

Do not run the key-generation example over an existing key. `register-key` accepts only the migration owner and an installation without a registered key or prior key ledger. It never prints or stores key bytes in PostgreSQL. Replacing an existing key requires the later offline rotation workflow; this command refuses replacement.

Supply that same `COSMOFORMER_ENCRYPTION_KEY_FILE` when serving. Startup checks that its fingerprint matches the registered database identity. Account-only development can omit it; managed credential writes then fail closed. File contents must be exactly 32 raw bytes in a regular file readable only by its owner. Binary bytes are not text-trimmed.

Encryption authenticates the tenant, record, purpose and semantic revision. Browser API `{org}` and organization-scoped `{id}` parameters require lowercase hyphenated UUIDs, matching the database identity used for decryption. Uppercase, braced and compact spellings are rejected with 422 before encrypted settings can be written. Online writers durably reserve 4096-use blocks before encryption, consume one slot for every seal, burn unused slots on process exit and refuse reservations beyond the per-key limit. Failed writes still consume their reserved usage. Browser receipts contain neither credential values nor a fast hash of their submitted plaintext. Repeating a once-visible credential/secret write requires a new explicit change.

## Configure source checks

Use **Sources & workers** as an organization administrator:

1. Add a Bitbucket connection and register an exact canonical HTTPS repository URL, immutable Bitbucket repository UUID, project and tracked branch.
2. Add worker pools and explicit project/environment resource permissions. S3 permissions use the exact partition/bucket namespace and object key, including the lock object. DynamoDB permissions use the exact existing table ARN. Saving these references does not inspect, create or modify AWS resources.
3. Save the existing execution role ARN/account reference. Source preflight records it but never creates an AWS session.
4. Approve the exact built worker engine/version/image identity as an installation administrator. Enroll a worker into an allowed pool, save its credential privately and explicitly resume it.
5. Sync tracked inventory. Git cannot grant access to another project, environment, worker pool or secret. Configured teams still need Planner permission to request a source check.

For private Git, the scoped API accepts a repository read-only credential at `POST /api/v1/organizations/{org}/repositories/{id}/source-credentials` with `username`, `token` and `expected_revision` (zero for the first credential, otherwise the enabled credential revision from configuration metadata). Replacement disables the previous credential permanently. The value is encrypted and never readable through the browser. Managed secret replacement/enablement uses `PATCH /api/v1/organizations/{org}/secrets/{id}` with the expected revision and never returns the existing value; every accepted change advances semantic identity. Repository credential replacements advance the active record revision; stale expected revisions conflict even when a replacement receives a new credential ID. Workers request it only for their assigned repository/attempt and semantic revision. Broad OAuth credentials stay on the platform.

The worker's credential helper uses a private local socket and an owner-only attempt context containing identities and repository URLs. The supervising agent validates scope and registers returned credential values before giving them to Git. No platform bearer path, token URL, plaintext disk cache or direct helper-to-platform access is supplied to Git. Source stderr is discarded; retained preflight metadata is checked for known credential values before submission. This does not claim protection against arbitrary transformations by trusted repository code or a compromised worker host.

## Supported source subset

Use one root `cosmoformer.yaml`, version 1. Declarations require stable UUIDs, an explicit root, tracked branch, exact engine version, workspace and worker pool. Optional lists have explicit empty defaults; approval minimum defaults to one. Unknown/duplicate keys, multiple documents, anchors/aliases, custom tags, interpolation and files above 1 MiB are rejected. Administrator approval floors cannot be lowered by Git.

Preflight inspects HCL or JSON S3 backend declarations and ordered literal partial backend files without `init`. It rejects backend expressions, embedded credentials, custom endpoints, backend-specific assume-role settings and unsupported attributes. Default workspace keys remain byte-exact. Non-default keys use native-compatible path joining; distinct declarations that resolve to the same state or auxiliary lock/checksum object conflict permanently. No raw S3 object inspection or lock migration is performed.

Modules must be contained local directories or authorized Bitbucket HTTPS Git sources pinned to full commit IDs. Remote module reads use the same exact-commit and repository capability boundary. Registry, SSH, floating-ref, submodule and LFS-pointer sources are unsupported. Git hooks, user/system configuration, redirects and smudge filters are disabled. Source files are bounded to 1 MiB each, 64 MiB total and 10,000 files; module traversal is bounded to depth 32 and 256 directories. Escaping symlinks, Git/native administrative paths and special files are refused. Provenance includes sorted paths, Git file modes and exact file/symlink digests.

Secret destinations cannot override worker/AWS/Git/native control variables. `TF_VAR_` destinations must name literal sensitive, non-ephemeral root variables. Preflight carries secret references only and has no execution-input/secret-value delivery route.

## Bitbucket connection and ingress

An administrator configures the OAuth consumer key/secret, webhook signing secret and expected hook UUID in **Bitbucket OAuth settings**, then chooses **Connect Bitbucket**. The callback is the configured application origin plus `/oauth/bitbucket/callback`. State is one-use, expires in ten minutes and is bound to the initiating local administrator/session/integration; it cannot sign in a user or select an arbitrary return URL. Configure only the selected `repository`, `pullrequest:write` and `webhook` OAuth scopes. Returned scope sets are checked before credentials are accepted. See [Bitbucket authentication and scopes](https://developer.atlassian.com/cloud/bitbucket/rest/intro/).

Rotating refreshes use a durable per-integration claim. A concurrent refresh cannot reuse the old refresh token. A failed or interrupted claim requires explicit reconnection; elapsed time is not evidence that retrying a rotating token is safe.

The ingress route is `/webhooks/bitbucket/{org}/{integration}`. It verifies the expected hook UUID, constant-time HMAC-SHA256 over the bounded original body and the registered repository UUID before committing a deduplicated receipt and reconciliation job. A receipt never launches a native command. Supported events are push and PR create/update/fulfill/reject. Reusing a delivery UUID with a different body is a conflict. See [Bitbucket webhook configuration](https://support.atlassian.com/bitbucket-cloud/docs/manage-webhooks/). No external webhook was registered or altered by the local implementation/tests.

The platform services source jobs only while running. It performs five-minute sweeps of tracked branches and known open PRs, coalesces obsolete unclaimed source requests and accepts source facts through ticket/revision fencing. Sweeps repair missed updates and closures for known PRs; discovering a new PR still requires its webhook or an explicit PR request. Incomplete changed-path information conservatively selects all verified declarations. Authenticated changed-source facts are committed before manifest parsing/import, so a missing, invalid or unauthorized new declaration still invalidates old queued/active preflight authority without replacing accepted inventory. Retargeting a PR away from the tracked branch also records the actual destination and revokes prior authority before declining replacement work. Later work creation rechecks the observation ticket, revision, repository configuration and elapsed read budget. Closed/changed source invalidates prior authority; source reversion creates a new revision rather than reviving earlier evidence. Automatic work has an explicit repository automation context, never a Bitbucket actor impersonating a local approver.

Repeating an accepted sync or preflight request with the same idempotency key and body recovers its saved response after checking current authorization. It does not reread Bitbucket or create another source observation; changed request bodies conflict and revoked access is still denied. A new source check requires a new request key.

PR feedback reuses its stored comment ID and looks for a stable marker before recovering an uncertain creation response. Pagination and credential-bearing destinations are bounded. Writes are serialized per PR across platform processes, and jobs for superseded source revisions are discarded before provider I/O. Feedback failures remain visible in **Source delivery**, retry with bounded jitter/backoff and honor supported Retry-After responses; exhausted delivery needs an explicit retry. Feedback does not grant access, count as approval or alter infrastructure.

## Worker identity and outcomes

`make worker-image` builds only a local OpenTofu reference worker image. The image requires Linux amd64, the pinned Git package and a verified exact runtime. It runs without root. A separately buildable Terraform reference download is pinned in `deploy/worker/runtimes.json`; changing the build arguments must still match one of the recorded engine/version/checksum tuples.

The protected `COSMOFORMER_TOKEN_FILE` contains JSON with `worker_id` and `token`, as shown once during enrollment. It must be readable by the worker UID and not by group/others. Configure `COSMOFORMER_PLATFORM_ORIGIN` as an exact HTTPS origin, `COSMOFORMER_RUNTIME_ID`, the operator-verified `COSMOFORMER_IMAGE_DIGEST`, and optionally an absolute `COSMOFORMER_ATTEMPT_DIR`. No database DSN, encryption key or cloud credential is supplied to the worker. The reported image digest is a trusted operator binding, not remote attestation.

Source workers advertise only `preflight`. Assignments have a five-minute nonrenewing elapsed budget; a response cannot extend it. Admission generations and agent instances fence replacements. Poll and ordered transition replays preserve accepted identities; changed accepted data or sequence gaps are rejected. Claim ownership remains separate from last-contact time. A restarted worker cannot take over an unresolved attempt.

“Source check passed” means exact-source inspection and binding were accepted. It establishes neither state contents nor infrastructure safety. Positively reaped preparation failures may be explicitly retried with a fresh attempt. An uncertain process group retains ownership and requires operator stop evidence; timeout, pause, credential revocation and missing acknowledgements never release it. Full stop/recovery operator workflows are not part of this phase. No native state permission endpoint exists yet.

## Isolated native compatibility checks

`make test-native` supplies a separate test-only image for each recorded runtime. The production `worker` target remains the default Dockerfile target and contains no acceptance-test binary or provider mirror. No new platform execution route is introduced.

The suite performs synthetic saved-plan apply against in-container HTTP fixtures only. It checks provider schemas, exact retained locks, native workspace state keys, conditional S3/DynamoDB locking, release after contention, and stale serial/different-lineage rejection. Test-only endpoint, path-style and credential-validation overrides remain rejected by the production source resolver. No real AWS state, repositories, roles or credentials participate.

Declare the quoted twelve-digit `backend_owner_account_id` in each `cosmoformer.yaml` stack. It is separate Cosmoformer metadata; native backend blocks and partial files reject `expected_bucket_owner`. Worker declaration matching, source digests and coordinator backend verification retain and compare the owner independently of native attributes. Never infer it from the execution role account: cross-account state buckets remain possible.

The deterministic `Backend.OwnerSessionPolicy` generator produces a fixed role-session policy with an explicit S3 resource-owner Deny for the backend bucket and its objects. Its Allow statement preserves intersection with the existing role's permissions. Local fixtures model this denial, including an owner change after init; they do not validate live IAM. The future production AWS adapter must attach the policy when assuming the existing approved role, use the constrained session for every native state command, and perform bounded SDK HeadBucket/ExpectedBucketOwner checks. No role is assumed and no AWS resource or policy is changed by this implementation. Real adoption remains gated.

Raw key/prefix validation follows the two tested engine versions before workspace joining. Both reject leading/trailing slashes; Terraform additionally rejects repeated slashes in raw keys, while OpenTofu accepts them. Both permit repeated internal prefix slashes. Default keys retain exact bytes; non-default keys use native path.Join. Unsupported versions require a reviewed validation profile.

For older local declarations, explicitly move owner intent to stack metadata and remove the unsupported native setting, then refresh source and request a new source check. Invalid Terraform keys require an explicit configuration/adoption decision; do not normalize them automatically. Existing permanent state/lock/checksum claims are retained, and old unresolved assignments are not rewritten. No schema migration is needed; the original five migrations remain unchanged. Production execution and actual company compatibility remain unassessed.
