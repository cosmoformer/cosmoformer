# Local deployment packages (milestone 15)

These Linux amd64 references extend the existing single Go coordinator,
PostgreSQL 18.6, compiled React Router/Astryx SPA and long-lived workers. They do
not constitute an installation or establish company compatibility:
`not_assessed` remains the adoption boundary. No availability, capacity, RPO or
RTO is promised. Do not scale the coordinator above one instance.

## Build and verify identities

Use the pinned Go 1.27.3 and Node 26.13.0 tools (`make tools`), Docker with
BuildKit and Compose supporting required-variable interpolation, read-only bind
mounts and `create_host_path: false`. Builds download only the pinned public
inputs; they do not publish anything. `go.sum`, `npm ci --ignore-scripts` and
`web/package-lock.json` govern dependencies. Image tags below are local names,
not immutable deployment authority.

Archive export requires a compatible Buildx builder. The default `docker` driver
on a classic Docker image store cannot export the normalized Docker archive. Use
a `docker-container` builder there; the package helper invokes `docker buildx
build` so the selected builder or explicit `BUILDX_BUILDER` is honored. For
example, CI creates and later removes its own builder with the verified BuildKit
0.29.0 image:

```sh
docker buildx create --name cosmoformer-packaging --driver docker-container \
  --driver-opt image=moby/buildkit:v0.29.0@sha256:0039c1d47e8748b5afea56f4e85f14febaf34452bd99d9552d2daa82262b5cc5
docker buildx inspect --bootstrap cosmoformer-packaging
BUILDX_BUILDER=cosmoformer-packaging make package
# After all builds/rebuilds have finished:
docker buildx rm cosmoformer-packaging
```

An existing compatible Docker Desktop builder also works. See Docker's
[builder selection](https://docs.docker.com/build/builders/#difference-between-docker-build-and-docker-buildx-build)
and [container driver](https://docs.docker.com/build/builders/drivers/docker-container/)
documentation. Builder setup affects local build tooling; it does not launch an
operator installation or publish packages.

```sh
make package
# To retain a single receipt for platform/proxy and both exact worker profiles:
# This builds packages; it does not approve runtimes.
node scripts/package-build.mjs platform proxy worker-opentofu worker-terraform
```

The builder freezes the selected source inputs into `.build/package/source`
before building. Both platform and proxy consume that snapshot; the Go-owned
contract executable generates the API schema and browser validators from those
same Go sources. No local generated contract, build directory, tool cache,
development certificate or `.env` is admitted. New packaging files are allowed
while this milestone is uncommitted; other source additions must be tracked.
Default timestamped BuildKit attestations are disabled for these local image
outputs so independent rebuilds can be compared. Images are first exported to
local Docker archives with `rewrite-timestamp=true` and the source commit's
`SOURCE_DATE_EPOCH`, then loaded into the local daemon. Fresh snapshot mtimes
remain visible to BuildKit's context transfer so same-size dirty edits are detected;
only the exported image timestamps are clamped to the commit epoch. The Dockerfiles explicitly include the epoch in
mutable-stage RUN cache keys; the scratch platform copies its source receipt
before cached stage outputs. See the cache/epoch failure investigation below. This avoids the daemon
exporter's incompatible direct-unpack mode. The [BuildKit exporter reference](https://github.com/moby/buildkit/blob/master/README.md#output)
describes timestamp normalization. A source manifest records HEAD, whether the checkout was dirty, exact file
SHA-256 values and an aggregate digest. A dirty build is identified by that
digest, never described as a pristine commit build. Keep this manifest with the
build receipt. Rebuild from the retained matching tree and lockfiles; compare
the actual binary, contract, SPA and final image identities. A successful build
alone does not prove reproducibility or operational compatibility.

Base identities were resolved from the upstream OCI registry on 2026-11-17.
The Dockerfiles use immutable index digests and assert Linux amd64; Node is
26.13.0, Nginx is the 1.28.3 Alpine content behind the pinned 1.28-alpine index.
The Go, Debian, PostgreSQL and worker engine/archive pins are reused from the
existing reviewed supply. Debian packages come from the fixed 20261105 snapshot.
No digest in the references is a placeholder.

`.build/package/images.json` records loaded Docker image IDs, source identity,
archive SHA-256 values, artifact checksums and the exporter's actual build
metadata/config/manifest identities. The local `*.tar` files are reviewable image
packages, not published images. `reproducibility.json` compares an independent
no-cache rebuild of every target in the complete first receipt (all five for
`make test-package`). Differing source revision, dirty state, epoch, source digest,
artifact/archive bytes or loaded image/manifest/config identities fail verification.
Missing identities, incomplete inventories and empty checksums also fail. Loaded
IDs retain the selected Docker store's semantics: a classic store can expose the
config digest, while the tested containerd store exposes the manifest digest.
Both exporter identities are still required and compared separately.

Each ordinary builder invocation replaces `images.json`; build the complete
desired set in one call. Independent reproduction instead writes to an owned
`.build/package/repro-<UUID>/` directory. Its `first.json`, `second.json`,
`comparison.json`, source manifests, exported archives and build metadata remain
available for diagnosis. A build interrupted before completion can have no
`second.json`; its partial outputs and explicit error are retained. The original
`images.json`, source tree and archives are never replaced by reproduction.
`resources.json` retains only that comparison's named image references on failure.
A cleanup error is recorded alongside the original error rather than masking it.

Runtime checks require all five verified source-bound image IDs; the standalone
browser entrypoint requires platform, proxy and browser-fixture. Missing inputs,
source/revision/dirty-label mismatches or unavailable images produce a package
dependency error before starting fixture services. The browser stage can check a
complete verified original set after a failed comparison; that independent result
cannot change the failed overall command into a pass. Cleanup removes only owned
containers and temporary tags; it never prunes shared caches or builders.
The platform contains `/usr/local/share/cosmoformer/SHA256SUMS` for the executable
and exported contract, `go-build.txt`, the seventeen-file migration inventory,
`source.json` and dependency/project licenses. The proxy retains the same source
manifest and frontend dependency license/integrity inventory. Workers retain
engine/Git/binary runtime identities, Go build/module metadata, source-file and
worker-binary hashes and licenses. Direct legacy `docker build` worker calls
without source arguments explicitly report an unrecorded Git revision; use the
snapshot builder for an operator-verifiable revision binding.

Inspect with `docker image inspect` and extract metadata from a stopped disposable
container with `docker create`/`docker cp`, then remove that container. Verify
hashes independently. A local Docker **image ID**, an OCI index digest and a
platform manifest digest are different identities. For registry references,
verify the Linux amd64 manifest and its image/config identity against your build
receipt. The worker catalogue's `image_digest` is the exact operator-verified
final worker manifest binding. Do not substitute a local image ID, arbitrary
synthetic hash, engine archive hash or mutable tag. Nothing here uploads images.

Compilers, Node/npm, package caches, tests and source files stay in builder/test
stages. The API is a static executable plus CA roots, metadata and licenses in a
scratch image. The Nginx image serves only compiled assets, never raw evidence.
The optional `browser-fixture` target contains synthetic acceptance fixtures and
is **not** a deployment image.

## Protected inputs and reference configuration

Copy each `.env.example` to a protected operator directory outside the repository
and fill every empty value. It contains paths and explicit identities, never
passwords/key bytes. Validate before starting anything:

```sh
docker compose --env-file /operator/platform.env -f deploy/platform/compose.yaml config --quiet
docker compose --env-file /operator/worker.env -f deploy/worker/compose.yaml config --quiet
```

Run validation under an operator environment without unrelated override variables.
Use verified immutable image IDs locally or digest-qualified registry references.
Compose requires explicit values; it does not independently authenticate an image
or validate the operator's source/manifest binding. Protect that verification
record outside the installation database.

The API and Nginx run as UID/GID 10001. Credential/key files must be regular,
owner-only, readable by that UID (normally mode 0400/0600); the encryption key is
exactly 32 raw bytes. A Compose bind mount preserves host permissions; it does
not honor secret ownership remapping. Use native Linux ownership or verified
user-namespace mappings. Never relax permissions to work around a bind error.
Certificates and the TLS key are mounted read-only into Nginx; the private key
must be owner-readable only. Mount only the runtime DSN and active key into the
API, only the individual enrollment token into each worker, and no owner DSN or
encryption key onto workers. No service mounts the Docker socket. Keep migration
credentials, backup keys, SQL preparation inputs and emergency inventories offline
and outside these references.

The platform reference exposes only the Nginx HTTPS listener on the explicitly
chosen private host address. PostgreSQL has no host port; its network is internal.
The API has no TCP listener or host port: Nginx connects through the existing
owner-protected Unix socket, shared only by those two containers. That directory
is mode 0700 and the socket 0600, owned by UID 10001. Nginx mounts it read-only.
This deliberately uses the application's implemented Unix transport trust
contract instead of inventing a TCP forwarded-header trust mode from an earlier
design table. `TRUSTED_PROXY_CIDRS` is not an implemented setting and is not used.
A host with Docker authority or access to that socket is inside the trust boundary.

Nginx uses its real TCP peer for access checks, replaces
`X-Cosmoformer-Client-IP`, removes `Forwarded`/`X-Forwarded-*`, and does not enable
`real_ip`. The Go listener accepts client-address metadata only over the private
Unix connection; ordinary TCP requests cannot claim it. CSRF and OAuth authority
remain the exact configured `COSMOFORMER_ORIGIN`, independent of Host/forwarded
headers. An upstream load balancer changes the observed peer: configure and test
its complete trust/access contract before adoption; this reference does not trust
its client headers. Docker Desktop/NAT may collapse clients into a bridge peer;
Linux host/VPN routing must be measured before selecting allowlists or relying
on per-client authentication throttles.

## TLS, access and HTTP behavior

Replace the default deny-all `private-access.conf` with an owner-reviewed
read-only file of specific approved client CIDRs followed by `deny all;`. Do not
use `allow all;` in an installation. Keep UI/login, `/api`, `/worker`, `/oauth`
and `/health` restricted to approved engineer/worker network paths.

Public Bitbucket delivery needs a **separate network rule**, reviewed outside
this package: permit public HTTPS only through an ingress able to distinguish
paths. Nginx includes a public exception solely for
`POST /webhooks/bitbucket/<organization-UUID>/<integration-UUID>`, with anchored
canonical UUIDs, a 1 MiB body bound and Go's signed event verification. All other
webhook paths/methods retain the private restriction or are denied. The default
private host binding grants no public network reachability. A layer-3 firewall
port opening alone cannot distinguish webhook paths. Test other methods,
prefixes, encoded paths and near-misses against the actual ingress. Do not open
`/webhooks` wholesale or bypass event signatures. No public rule, DNS entry or
external webhook is created by these references.

Supply TLS certificates through your existing protected issuance/renewal process.
There is no ACME client or generated production certificate. Validate the new
certificate/chain/key and file ownership, run `nginx -t` inside the running proxy,
then `nginx -s reload`; a failed reload retains the previous workers. File bind
mounts retain their old inode across host-side atomic replacement: recreate the
proxy with the renewed inputs (after validation) or use an independently reviewed
stable certificate-directory mount/reload procedure. Verify the served chain and
expiry. Keep certificate keys separate from the application encryption key.

Packaged application requests explicitly trust the generated fixture certificate.
Separate checks require rejection of untrusted, wrong-hostname and expired
certificates through the actual Nginx image, including expiry of an explicitly
trusted certificate. Browser journeys ignore the temporary certificate's trust
error to test application/CSP behavior; TLS trust is verified separately. These
synthetic checks do not establish real issuance, renewal or host certificate custody.

All API, worker, webhook, OAuth and health namespaces retain upstream status/body,
including unknown routes and errors. Only the declared browser paths receive SPA
fallback. Missing assets return 404. Hashed `/assets/` successes are immutable;
HTML stays `no-store`, as do protected API/evidence responses. Nginx does not log
request URIs/query strings, cookies or bodies. Request-bearing error logging is disabled too: even Nginx critical upstream
errors append full request URIs. Configuration checks still report their result
to the invoking CLI. Use independent safe health monitoring; runtime request
diagnostics are deliberately unavailable rather than logging OAuth query values.

The CSP is generated from each production HTML's exact static script bytes.
It permits same-origin modules, fonts/styles/assets, and SHA-256 hashes for the
React Router bootstrap scripts. There is no script `unsafe-inline` or
`unsafe-eval`. Astryx/React style attributes are explicitly allowed by
`style-src-attr 'unsafe-inline'`; inline style elements remain disallowed. This is
an intentional style-only concession, not permission for inline script handlers.
The optional Astryx fonts stylesheet imports Google Fonts, so it is omitted; the
existing theme's system-font fallbacks are used without an external request.
CSP violations fail packaged browser journeys, including separately created
engineer contexts. A separate synthetic blocked-script probe tests the monitor
across navigation in both browser engines.

Browser control bodies are 64 KiB; worker control/webhook bodies are 1 MiB.
The exact worker artifact upload path permits 67,108,865 bytes at the transport
boundary (64 MiB plus the application's overflow sentinel); per-kind plan/log
bounds and identity checks remain enforced by Go. Proxy request/response buffering
and caching are disabled for upstream traffic; authenticated bodies do not spill
into Nginx temporary files. Go's 15-second request read and 30-second write limits
remain in force. Nginx has a 35-second read/send timeout for the 20-second worker
poll. These are pilot bounds, not a throughput promise. Slow artifact transfers
may fail these limits; validate actual network conditions without widening
application authority or bypassing bounds.

## Prepare, migrate and bootstrap explicitly

1. Independently prepare a persistent PostgreSQL 18.6 volume with native UID/GID
   70 ownership and data at `/var/lib/postgresql/18/docker`. Use the pinned image's
   `initdb` under an explicit operator procedure, protected superuser password
   file and reviewed `pg_hba.conf`/network authentication. No trust authentication
   belongs in an installation. The reference runs `postgres` directly, bypassing
   its initialization entrypoint: an absent/unprepared volume fails. Database
   durability, volume capacity, filesystem protection and backups are operator
   responsibilities; container memory limits do not bound persistent disk usage.
2. Create `cosmoformer_owner` and `cosmoformer_app` roles and the application DB
   using protected operator SQL. Owner: LOGIN, NOSUPERUSER, NOCREATEDB,
   NOCREATEROLE, NOBYPASSRLS. Runtime: the same restrictions plus NOINHERIT, with
   no owner membership; it must not own tables or have schema CREATE. Assign DB
   ownership to `cosmoformer_owner` and revoke public schema CREATE. Keep passwords
   in protected files/SQL input, never command arguments or this repository.
3. Start only PostgreSQL. Run the **new verified platform image** with an explicit
   one-shot `migrate --list` and then `migrate`, owner DSN mounted read-only and
   `COSMOFORMER_MIGRATION_DSN_FILE` pointing to it. Mount a private bounded output
   directory only when a host command needs a receipt/code. Connect on the private
   DB network. Check all seventeen embedded migrations and the resulting schema 17.
   No Compose startup dependency runs a migration. An old/pending schema or unsafe
   runtime role fails startup; do not retry partial migrations blindly.
4. On a new installation only, run `register-key` with the protected initial key
   and owner DSN. Keep the key independently under custody. Run `bootstrap-admin`
   with `--username` and `--setup-code-file` in a new protected output directory.
   Preserve the one-use code out of logs; complete `/account/setup` over private
   HTTPS. The reference generates neither a password nor an encryption key.
5. Start one platform and the proxy. Check restricted `/health/live` and
   `/health/ready`, certificate/ACL behavior, current schema/role/key identity and
   authenticated access. Live is process liveness; ready checks DB/schema/role
   compatibility, **not** execution eligibility, maintenance clearance or external
   service compatibility. Inspect maintenance and restore gates separately.
6. Build/select one exact OpenTofu 1.13.8 or Terraform 1.17.1 Linux amd64 profile.
   Independently verify the final image digest, baked native binary/Git identity
   and source receipt. Approve that exact runtime through installation Admin,
   register a paused worker in the intended pool, retain its one-time protected
   credential, configure the expected bootstrap principal, and start the separate
   worker reference. Review grants/admission before explicit resume. Approval,
   enrollment, start and resume are separate decisions.

For the explicit container commands below, all files/directories must already
exist with the ownership and protections above. `migration.dsn` is a separate
owner credential; `/operator/codes` is a private bounded output directory owned
by UID 10001. The normal `platform` service never mounts that owner credential.
Run each lifecycle step only after its preceding preparation/check has succeeded:

```sh
compose() { docker compose --env-file /operator/platform.env -f deploy/platform/compose.yaml "$@"; }
compose config --quiet
compose up -d postgres
# Independently establish database readiness and prepared roles first.
compose run --rm --no-deps --volume /operator/private/migration.dsn:/operator/migration.dsn:ro --env COSMOFORMER_MIGRATION_DSN_FILE=/operator/migration.dsn platform migrate --list
compose run --rm --no-deps --volume /operator/private/migration.dsn:/operator/migration.dsn:ro --env COSMOFORMER_MIGRATION_DSN_FILE=/operator/migration.dsn platform migrate
# New installation only, with the independently retained initial key:
compose run --rm --no-deps --volume /operator/private/migration.dsn:/operator/migration.dsn:ro --env COSMOFORMER_MIGRATION_DSN_FILE=/operator/migration.dsn platform register-key
compose run --rm --no-deps --volume /operator/private/migration.dsn:/operator/migration.dsn:ro --volume /operator/codes:/operator/codes:rw --env COSMOFORMER_MIGRATION_DSN_FILE=/operator/migration.dsn platform bootstrap-admin --username admin --setup-code-file /operator/codes/initial-unique-code
compose up -d platform nginx
compose exec nginx nginx -t
# Use the real configured HTTPS origin and its trusted certificate chain:
curl --fail https://your-configured-origin.example/health/ready
```

The final origin is illustrative: replace it with the exact configured private
origin. Starting the database above cannot initialize an empty volume. No command
prints the setup code or key bytes. Bootstrap, migration and initial key
registration are intentionally separate operator actions; do not wrap them in
an automatic restart/retry/deployment entrypoint.

The existing AWS principal and execution adapters remain subject to the operator
guide's implemented-versus-synthetic boundaries. This packaging adds no new AWS
credential broker, host identity provisioner or production compatibility claim.
The platform needs separately verified artifact storage/identity and outbound
Bitbucket/AWS access; these references create neither those services nor IAM.

## Shutdown, upgrades and recovery

Enter maintenance with the current compatible binary, stop admission, request
drain and report outcomes. Independently verify workers, integrations, normal
writers and cleanup I/O stopped before offline maintenance, key rotation or backup
capture. A delivered signal, expired lease, stopped container or readiness result
is not proof that infrastructure execution stopped. Preserve uncertain attempts,
worker/backend reservations, permissions, exact artifacts and recovery cases.

The API receives SIGTERM directly as PID 1 and has a 10-second HTTP shutdown
window inside the reference's 45-second container stop allowance. Nginx uses
SIGQUIT for graceful exit. After an unclean API exit, a stale Unix socket can
block startup. Independently establish that the old coordinator is stopped
before an owner removes that stale pathname; never clear database ownership or
recreate the database volume to work around a socket error. Workers use the existing process-group supervision,
a tiny init for orphan reaping, executable bounded attempt storage and a
330-second stop allowance for the existing 300-second stop margin. Do not grant
SYS_PTRACE, privileged mode, a Docker socket or host PID namespace. A host/runtime
that cannot meet the existing descendant accounting contract must block work;
container exit alone never clears unresolved ownership. The long-lived worker
and its native children share UID 10001. Read-only mounts and an empty child
environment do not isolate its enrollment token from malicious same-UID code;
worker hosts, accepted repository execution and provider supply remain trusted
boundaries. This package adds no hostile multi-tenant process sandbox. Attempt tmpfs is volatile:
recreating a worker loses local preparation bytes but grants no right to re-admit
uncertain work or replay Apply. Inspect and reconcile through the existing recovery
workflow first. Force-killing after the grace period keeps uncertain work uncertain.

For an upgrade: maintenance and drain, independent stop/cleanup verification,
validated backup custody, stop the one platform, verify new source/binary/image
identities, run explicit owner migrations, inspect maintenance/key/schema gates,
and resume only after the operator's explicit maintenance exit and worker review.
Never run two coordinator versions together. Never automate schema rollback or
call restarting an older binary/database recovery. Failed upgrades stay stopped
for forward repair or the coordinated restore/reconciliation procedure.

The authoritative procedures remain in the [operator guide](operator-guide.md):
maintenance/offline evidence, key quota and custody, exact backup-hold capture,
external backup inventory, restore-begin/reconcile, fresh-key rotation, clock
reset, historical artifact availability and emergency handback. Preserve restore
gates and cleanup quiescence. No script here supersedes independent external
stop evidence or promises backup/PITR performance.

## Verification scope

`make test-package` builds all five targets, exercises a disposable Git history
across two commit epochs, compares repeated warm-cache and two independent
no-cache builds, verifies changed-input/dirty receipts, and runs both container checks and
the packaged browser suite under the existing Node coverage runner. It validates real image/runtime boundaries, the packaged
executable against a new marked disposable PostgreSQL cluster, HTTPS routing,
authentication, bounds, cache/CSP headers, worker long polling, missing inputs,
forwarding spoofing, shutdown/restart and upstream failures. Its test harness uses
generated synthetic inputs and a test-only local network namespace; it is not a
production provisioning command. `.build/package/verification.json` records the
successful check inventory and identities. Failure must be investigated, not
reported as a skipped success.

`make test-package-browser` optionally runs only the browser portion: it builds the separate synthetic browser-fixture target
and runs the complete Chromium and Firefox journeys through the **same packaged
Nginx/SPA/CSP**. The fixture uses real Go API/PostgreSQL code and controlled
Bitbucket/native/evidence adapters, not a production artifact service. This is
distinct from `make test-web`'s Vite preview journeys and from the executable
container checks. The helper's private test receipt identifies only resources
that it owns; cleanup failures retain that receipt for investigation.

Both browser entrypoints create their credential output exclusively. An existing
file causes startup to fail without replacement or deletion. Stop its owning
fixture and establish that any interrupted run has ended before cleaning its
output; do not remove another active fixture's credentials to force a new run.

Configuration validation, image builds, actual container execution and actual
browser journeys are separate evidence. Synthetic local checks do not test a
Linux production host's routing/user namespaces, real TLS issuance/renewal,
external ingress/webhook routing, company repositories, IAM/artifact custody,
physical backup/WAL restoration, capacity or operational recovery time. Adoption
remains milestone 16.

The packaging LCOV report is a required sixth input to the existing overall
coverage gate. CI requires the packaging job and its checksummed coverage bundle;
missing/failed/skipped jobs or reports cannot reduce the collected inventory.
No coverage thresholds or source exclusions have been relaxed.

## Measured verification — 2026-11-17/18

Aggregate Go coverage, all six Sonar coverage inputs, all five package targets,
independent platform/proxy reproducibility, container checks and both browser
entrypoints were regenerated after the schema-17 authentication fix and final
requirements review. The current
coverage-source identity is
`16995227202e20f4b3075e743edf247a16cea871552f55516b0cffcc739ff6a0`.
The package receipt uses its own broader build-input digest,
`da6520d4825149d53f43ea55a67ddebd00d41facec91091194376c1dba8ba436`.
The final review corrected TLS verification, browser fixture credential ownership
and failed-Go diagnostic retention, as detailed below. Both native matrices and
the standalone recovery workflow were subsequently rerun against the same final
source on 2026-11-18; their results are included below.

These are local results on a Mac arm64 host with Linux amd64 containers/emulation:
Docker Desktop 4.73.0, Engine 29.4.3, Buildx 0.33.0-desktop.1, BuildKit 0.29.0 and
Compose 5.1.3. Go 1.27.3, Node 26.13.0 and Playwright 1.64.1 use the repository
pins. Verification ran before commit: the receipts record base
`b27bb3f4b2fc3e21b5cbd3ae18aee44e1d18a204`, `dirty: true`, and the exact build-input
digest. These are pre-commit build receipts, not builds of the later review
commit. Do not describe the resulting images as a pristine build of the base.

| Verification | Observed result |
| --- | --- |
| Platform and worker Compose references | Both parsed with synthetic explicit inputs; both rejected missing inputs. No complete operator installation was launched through Compose. Runtime containers used the corresponding security constraints and isolated test inputs. |
| Image builds | Platform, proxy, browser-only fixture, OpenTofu worker and Terraform worker built successfully. Production API/proxy filesystem inspection excluded compilers, package caches, source and test fixtures; runtime metadata/licenses were retained. |
| Independent reproducibility | Fresh no-cache platform/proxy builds matched the source digest, executable/contract/SPA checksums, archive SHA-256 values and both image identities. Exact first/second identities and layers are in `.build/package/reproducibility.json`. |
| Packaged execution | **62 checks passed**, including trusted/untrusted/wrong-hostname/expired TLS certificates, real setup/sign-in, routing/errors, deep links, caching/CSP, request bounds, real authenticated 20-second polling, forwarding spoofing, OAuth-query logging protection, ACL/webhook isolation, both worker identities, missing-input failures, graceful API shutdown and uncertain-ownership preservation across two packaged-executable restarts. |
| PostgreSQL reference constraints | A prepared persistent test volume restarted under UID/GID 70 with read-only root, dropped capabilities, bounded temporary storage and no published host port; schema 17 remained present. All 17 embedded migrations were verified. An unprepared data directory failed without initialization. |
| Packaged browsers | **50/50 application journeys plus 2/2 synthetic CSP-detector regressions passed** across Chromium and Firefox through packaged Nginx; no observed application CSP violations. The detector regressions deliberately block a script and verify reporting after navigation. Includes real retained Changes, administrative/authentication pages and evidence-content denial/authorized download with exact retained hash, size and `no-store`. Other journeys deliberately inject bounded responses to exercise UI failure states. |
| Existing frontend entrypoint | Type checks, production build, **250/250 tests in 20 files**, and **52/52 Chromium/Firefox Vite-preview checks passed** (50 application journeys and two detector regressions). Coverage: statements 95.07%, branches 85.91%, functions 92.21%, lines 99.18%; existing thresholds unchanged. |
| Go unit/race and vet | Fresh aggregate backend `-race -count=1` run: **290 top-level passes across 21 tested packages**, no skips/failures, including schema-17 regressions and the actual PostgreSQL dump/restore exercise. Ordinary and integration-tag vet passed against the same source. |
| PostgreSQL integration | Included in the fresh backend run. The complete acceptance inventory was then verified in four bounded groups: **106 discovered, 101 passed, five expected opt-in skips**, no omissions or failures. Each group used the existing Make entrypoint and its owned DSN, marker and container context. |
| Opt-in reconciliation | `TestBrowserServer` and `TestPackagedRestartPreservesUncertainOwnership` were executed by the fresh browser and packaged checks. Both `TestNativeEngineCompatibility` matrices executed afresh on 2026-11-18. Fixture-supply and GNU Make recipe prerequisites executed and passed for both engines; ordinary content-matching worker/toolchain layers were cached. |
| Linux supervision/source | All four baked source, worker, clock and command suites passed under init, read-only root, dropped capabilities, bounded writable paths and no external network. |
| Native profiles (2026-11-18 final-source rerun) | Both OpenTofu 1.13.8 and Terraform 1.17.1 matrices passed with null 3.2.4 and AWS 6.41.0; each verified all **126 analysis cases** against its actual native AWS schema, plus existing backend/state/lock scenarios. Zero runtime failures or skips. |
| Standalone recovery (2026-11-18 final-source rerun) | **Nine top-level race tests passed**, zero failures/skips, package time 14.439 seconds. Synthetic key rotation, backup protection, clock reset and actual PostgreSQL dump/restore passed with its own disposable marked database. The dump/restore exercise also passed in the preceding schema-17 backend run. |
| Scripts/runtime/contract | **137/137 script tests passed**; both actual local-storage/seed runtime coverage checks and the standalone contract-generation coverage test passed. The packaging coverage runner passed both its container/reproducibility and browser tests and emitted the sixth LCOV report. |
| Aggregate Go coverage | Fresh backend run: **290 top-level passes across 21 packages**, no skips/failures. Four acceptance groups verified the complete **106-test inventory: 101 passes, five expected opt-in skips**; longest group 561.283 seconds. Host/child-process and Linux profile union: **84.55% (10,539/12,464 statements)**, passing the unchanged strict >80% threshold. |
| Overall Sonar coverage | All six fresh reports imported with the exact working-tree identity. **Combined: 81.3217% (16,932/20,821); lines: 80.5705% (14,800/18,369)**, both strictly >80%. Go, JavaScript and TypeScript were analysed without parser or coverage-import failures. |
| Sonar static review | Quality gate **OK**, duplication 0.3%, seven open static labels, zero hotspots. The five packaging-helper labels were source-reviewed: intended anchored regex/string ordering, trusted operator Docker lookup, and a container-private negative-test path; no confirmed boundary violation. The other two labels concern existing keyboard-scrollable UI regions. This disposable first analysis has no meaningful new-code baseline; gate status alone is not evidence of zero findings. |
| Security audit | Preceding full standard source-audit workflow scoped to the milestone 15 changes, before the follow-up fixes below: independent source hunters, two clean coverage critics, candidate and final-record verification. That historical audit reported **no confirmed vulnerability**. Its conditional authentication-throttle capacity lead was subsequently reproduced and corrected by schema 17, as recorded below. This is partial repository coverage, not a whole-product assurance. Reports are retained separately in security-audit run 7. |
| Cleanup | Owned package containers, volumes, networks, comparison tags, marked host test database and disposable Sonar service were removed after report capture. Local image archives, images and ignored verification receipts remain for review. |

The initial monolithic acceptance run reached its unchanged 30-minute timeout
while making progress in race-instrumented Argon2 on arm64; its timeout stack
showed ordinary password hashing, not an ownership/database deadlock. The full
inventory subsequently passed in the four groups above without lowering hashing
parameters, assertions, coverage thresholds or prerequisites. Group results and
exact test names are retained in `.build/m15-acceptance-results.json`. The local
coverage entrypoint now reuses CI's deterministic backend/four-group execution,
verifies each exact terminal test inventory, waits for all groups on failure, and
refuses stale or incomplete profile merges. The final Go/HTML reports are
invalidated before Linux prerequisites and before host collection, so a failed
rerun cannot leave a previous successful aggregate behind. Fresh coverage group results are in
`.build/m15-final-go-results.json`. Race detection, atomic instrumentation,
child-process counters, prerequisites and strict thresholds remain enabled.

The focused packaging review found and corrected: missing private-socket directory
ownership in the scratch image; implicit Nginx temporary directories incompatible
with read-only root; Astryx's external Google Fonts import; request URI disclosure
in Nginx critical upstream errors; unnormalized image layer timestamps; and a
license-collector error on the unversioned main module. The same-UID worker token
boundary described above remains an explicit execution-isolation limitation.

The full GitHub Actions workflow was **not rerun**. Its local aggregate Go and
overall Sonar coverage checks were rerun successfully against this exact working
tree, including packaging's required sixth coverage report. The source receipt
is `.build/coverage/started.json`; exact counts, source digest, task and analysis
identities are in `.build/coverage/overall.json`, with the static review and
triage in `.build/coverage/sonar/report.md`. These are local results; no hosted
analysis, release, publication or operator deployment was performed.

The follow-up full-diff review corrected the following verification/cleanup issues:

- Additional engineer browser contexts now share the packaged CSP collector and
  receive automatic teardown. Two real-browser synthetic probes verify that a
  blocked script is reported across navigation in Chromium and Firefox.
- Reproducibility now fails on differing archive, manifest or config identities,
  missing identities and empty checksum sets; an old successful comparison is
  removed before a new attempt. Mutation regressions exercise those failures.
- Go coverage invalidates its final aggregate/HTML before Linux prerequisites
  and host execution. Invalid Go producer IDs cannot delete another producer's
  reports or generated type context. Regression checks include a failing Make
  prerequisite and an interrupted/failed host collection.
- Packaged readiness requires exactly the real HTTP 200 `ok` health response;
  redirects and SPA bodies cannot signal readiness.
- Browser-startup signal handlers are installed before resource creation and
  cancellation leaves existing browser credentials alone until this run writes
  its own file. A real early-SIGTERM exercise exited cleanly and removed all
  owned resources. Cleanup failures remain failures with a retained receipt.
- The two-minute packaged restart test has a 150-second outer Docker allowance.
  Proxy verification uses the same read-only, no-copy socket mount as Compose;
  a proxy-first volume exercise verified UID 10001 and mode 0700.
- The frozen uncommitted snapshot includes the new collector and detector tests;
  documentation builds the complete desired image set in one receipt.

All six coverage reports and the final Sonar analysis were regenerated after the
schema-17 and final verification fixes. Task `9d1eda1e-d58f-4bf4-8617-d77902a69b64` completed as analysis
`7d99d035-17b9-4389-ab24-cd0df0fd664b`; exact counts and source identity are retained
in `.build/coverage/overall.json`. The preceding formal security audit remains a
separate historical source review. Native matrices and standalone recovery also
passed the subsequent 2026-11-18 final-source rerun recorded below; ordinary/
integration vet and the backend's actual dump/restore exercise passed in the
preceding schema-17 aggregate run.

### Subsequent focused authentication fix

A disposable local fixture with exactly 10,000 recent synthetic throttle rows
confirmed the conditional defect: an untracked enabled account received six 401
responses with no accounting row, while the tracked control blocked after five
failures. This was seeded-state validation, not an attacker-driven capacity fill
or a deployed exposure test.

Forward migration 017 reserves a bounded zero-failure subject row atomically at
admission and denies new subjects at capacity. Reservation runs inside the existing
bounded verification slots, so busy requests and requests canceled before admission do not consume rows.
The first failed verification starts its failure window; normal success releases
the reservation. The handler honors failure-accounting waits as 429/Retry-After.
Migrations 001–016 are byte-for-byte unchanged. The new binary requires schema 17
and an explicit operator migration; this change performs no deployment or automatic
migration. Packaging verification now derives its expected inventory/schema from
the source version instead of hard-coding 16, and the uncommitted source snapshot
explicitly includes the new migration and regression files.

The final fixed fixture returned six 429 responses for the untracked account at
capacity, preserved the tracked five-failure policy and admitted a correct login
after space was freed. Eight concurrent contenders admitted exactly one last-slot
reservation; the version-16 upgrade repaired the same cached runtime connection.
Five focused tests passed on Linux amd64. The affected identity, HTTP, datastore
and operator-command race/integration suites passed 112 top-level tests on Linux
arm64; the separate Docker-controlled dump/restore exercise was explicitly excluded
because no Docker socket or shared service is exposed to the security fixture.
Go static checks and formatting passed for the affected code.

The fixture used cached tools only, an empty allowlisted environment, read-only
sanitized source/tooling/module caches, private bounded scratch storage, no external
network or published ports, and explicit CPU/memory/PID/file/disk/time limits. Its
synthetic PostgreSQL and HTTP traffic stayed in an isolated loopback namespace.
No live authentication traffic, saturation run, actual accounts or external service
was used. Practical deployment attainment remains unmeasured; the conditional code
defect itself is corrected. The subsequent broad schema-17 package/browser,
aggregate coverage and Sonar rerun passed as recorded above. Both independent
native-engine matrices and standalone recovery subsequently passed against the
same final source on 2026-11-18.

### Final requirements review

The final comparison covered the original build, configuration, proxy/CSP,
lifecycle and verification requirements. It corrected these concrete gaps:

- Packaged HTTPS requests previously bypassed certificate validation. They now
  trust only the synthetic fixture certificate, and actual Nginx checks reject
  untrusted, wrong-hostname and expired certificates. An expired certificate is
  rejected even when explicitly trusted. No host clock or shared proxy is changed.
- Both the normal Go browser fixture and packaged supervisor now create credential
  output exclusively. The unit regression preserves existing content and mode;
  actual entrypoint collision probes rejected startup without replacing/deleting
  existing credentials. The relevant writer files remain unchanged after those
  probes, and both complete browser entrypoints passed again in the final run.
- A failed Go subprocess previously lost the tail of its output when Node
  inspected its error. The collector now prints complete stdout/stderr before
  rethrowing. A long-output regression verifies retention of the final assertion
  and preserves raw-counter cleanup and refusal to publish failed coverage.
- README, contributor and security descriptions now reflect implemented Apply/
  recovery, packaging's required coverage job, schema 17 and the accepted same-UID
  worker credential boundary.

An earlier aggregate run failed in `acceptance-0`. Its failing assertion was
truncated by the old collector, so the original test/root cause could not be
reconstructed. The original log remains in
`.build/m15-final-before-diagnostics/coverage/go.acceptance-0.log`. That group and
the complete final aggregate subsequently passed with full diagnostics and the
same assertions, prerequisites, race instrumentation and time/coverage limits.
The earlier one-off failure remains unexplained; this is not a claim that a
production root cause was identified or repaired. Intermediate runs are retained
separately and do not supply the final source-bound coverage/package evidence.

### Acceptance-0 repeat investigation — 2026-11-18

The follow-up investigation ran the exact original 27-test acceptance-0 selection
five times with complete stdout/stderr written directly to private files: four
independent concurrent copies, followed by the original four-shard workload with
isolated no-cache platform/proxy archive builds running alongside it. All five
acceptance-0 runs passed: **130 passes, five expected packaged-restart opt-in
skips, zero failures**. The full shard replay also passed its complete 106-test
inventory: **101 passes and five expected opt-in skips**. No race report, panic or
timeout appeared; the unchanged 15-minute limit, race/atomic instrumentation,
count=1 and all assertions/prerequisites remained in force.

All 211 Go/SQL files match the original failed round byte-for-byte. The code-source
digest remains `16995227202e20f4b3075e743edf247a16cea871552f55516b0cffcc739ff6a0`;
each repetition checked its source identity before and after execution. No concrete
code defect was confirmed, so no product code or test assertions were changed.
The original cause remains unknown because its assertion was lost; these passing
repeats do not establish that an intermittent failure is impossible.

Exact commands, full logs, inventories, timings and the reproduction driver are
retained under `.build/acceptance0-investigation/` and the two run directories
identified by its report. The report also records a temporary build-helper
quoting error and its successful correction, separately from Go results. Both
owned database clusters were removed; temporary build archives were neither
loaded nor published. Existing aggregate/Sonar/package evidence remains separate
and was not replaced or relabeled by this investigation.

### Final consistency check — 2026-11-18

The current checkout matches all 367 recorded package inputs, all 334 retained
scan-snapshot files, all six imported coverage reports and all five image-archive
checksums. Source/HEAD, image/rebuild identities, schema 17, migration inventory,
test counts, exact Sonar coverage counts and repeat-run results agree across the
records. Migrations 001–016 and dependency/runtime lockfiles remain unchanged.
The original requirements remain covered by the implementation and the explicitly
scoped evidence above. Two stale operator-guide passages that still described
packaging as future work were corrected.

This was a consistency check of existing evidence, not another test execution.
At that checkpoint, native matrices, standalone recovery and the scoped security
audit retained their historical scope. The native/recovery rerun below now
supplements that evidence; the scoped security audit remains historical. The
original acceptance-0 cause remains unknown despite five clean diagnostic
repetitions. The consistency receipt is `.build/m15-final-consistency.json`.

### Native engines and standalone recovery rerun — 2026-11-18

`make test-native` executed both Linux amd64 matrices against the final source:
OpenTofu 1.13.8 and Terraform 1.17.1 each passed all 126 analysis cases against its
actual AWS 6.41.0 provider schema, with null 3.2.4 and the existing backend, state,
workspace, lock and ownership checks. Both runtime matrices had zero failures or
skips. Fixture-supply and GNU Make recipe prerequisites executed and passed for
both engines; ordinary content-matching worker/toolchain layers were cached. Both
matrix runtime processes executed afresh, sequentially with at most two scenarios
each.

Native runtime containers used UID/GID 10001, init, read-only roots, dropped
capabilities, no external network or host mounts, two CPUs, 3 GiB memory, 256 PIDs
and bounded executable temporary storage. Local images and public caches remain;
the runtime containers were removed after execution.

The pinned Node entrypoint `scripts/operator-recovery.mjs` passed all nine
top-level race tests, with zero failures or skips (Go package time 14.439 seconds).
It used its own marked disposable PostgreSQL cluster and exercised actual logical
dump/restore, rotation, backup protection, clock reset, interrupted handoff and
inventory/independent-stop guards. The synthetic restore retained one backup
artifact, three open recovery cases and two unresolved attempts; exact missing
evidence remained `unavailable`. The workflow stopped its owned database and
removed its successful receipt.

Source identity matched before and after both commands and equals the coverage
identity above; all 367 package inputs and six imported coverage reports remain
unchanged. No code fix, assertion change or coverage regeneration was needed.
Full logs, schema/image identities and machine-readable results are retained in
`.build/m15-native-recovery/`. Earlier aggregate, Sonar and packaging results remain
separate records. No cloud service was changed, and nothing was published or
deployed. These synthetic local checks do not establish company compatibility or
physical WAL/PITR restoration.

### Consolidated consistency review — 2026-11-18

The final review matched all 367 package inputs, 334 retained scan files, six
coverage inputs and five image archives to the current checkout and receipts.
Raw Go/native/recovery logs agree with their recorded inventories and results.
Both engines' fixture-supply and Make-recipe prerequisites actually executed and
passed; the earlier description of OpenTofu's prerequisites as cached was
incorrect and has been corrected. The original report is retained beside its
amended version. Runtime/recovery results and source identities are unchanged.

The consolidated receipt, evidence hashes and replayable consistency checker are
in `.build/m15-final-consistency-review/`. This review did not rerun functional
tests or change product code. Image users/identities and disposal of owned local
test resources were checked again. Migrations 001–016, dependency/runtime pins,
coverage inputs and thresholds remained unchanged; the reviewed changes were
uncommitted at that checkpoint.

### Hosted CI packaging compatibility fix — 2026-11-18

The first PR run exposed a build-tooling difference: its classic Docker image
store could not export the normalized Docker archive through the default driver.
The packaging job now creates an exact pinned `docker-container` BuildKit builder
and removes it after use. The helper explicitly calls `docker buildx build` so
builder selection is honored. No application, schema or runtime policy changed.

Six focused local regression checks passed, including the actual package helper
with a fixture that rejects the old default-driver command. Two actual no-cache
exports of a tiny synthetic image through the pinned builder matched archive,
manifest and config identities; its disposable builder was removed. These are
focused portability checks, not a new full package/coverage run. Earlier full
local results retain their recorded source identity; the PR workflow collects
fresh package and coverage evidence for the follow-up commit.

These results do not validate an actual Linux deployment host's ownership/network
mappings, externally issued TLS renewal, a public ingress or live Bitbucket OAuth/
webhook delivery, production AWS/artifact permissions, hostile multi-tenant code,
physical WAL/PITR restoration, capacity or recovery time. Restore/backup/key and
independent stop evidence remain operator responsibilities. Company adoption is
milestone 16 and compatibility remains `not_assessed`.

### Cache/epoch reproducibility failure and correction — 2026-11-18

The historical merge validation at `8df6868` is retained separately under
`.build/m15-merge-validation/`. It passed artifact checksums but failed the full
cached-versus-no-cache comparison for platform and proxy. Reading the platform
archives as nested tar streams (without extracting archive paths) showed cached
`run`, `run/cosmoformer`, `tmp` and license directories at epoch **1794952582**
(2026-11-17T21:56:22Z), while the independent build used the merge epoch
**1794960174** (2026-11-18T00:02:54Z). Ownership and modes matched. These historical
results do not validate the changes on this branch.

BuildKit 0.29.0's [timestamp converter](https://github.com/moby/buildkit/blob/v0.29.0/util/converter/converter.go)
clamps timestamps newer than the supplied epoch; it does not raise older ones.
The [Dockerfile cache rules](https://github.com/moby/buildkit/blob/v0.29.0/frontend/dockerfile/docs/reference.md#impact-on-build-caching)
exempt predefined arguments from RUN cache keys unless explicitly declared.
Previously an unchanged RUN could reuse directories created before the next
commit epoch; exporting with a later epoch left those old mtimes intact.

The correction records the actual HEAD commit epoch in source/image receipts and declares
`SOURCE_DATE_EPOCH` in Go, web, certificate, proxy and worker engine/build stages.
A changed epoch invalidates their RUN caches; repeated builds at the same epoch
retain caching. The platform's initial source-receipt COPY changes its scratch
COPY chain with revision/dirty/source/epoch identity before generated directories
are copied. No fixed timestamp replaces source identity, and strict comparisons
remain in place. Worker layers additionally omit apt's wall-clock installation
logs and account backups in the layers that create them, and use the source
epoch for the locked account's last-change day. Package databases, runtime
metadata, licenses, executable modes and runtime UID/GID remain in the images.

Reproduce with a compatible BuildKit 0.29.0 builder selected as documented above:

```sh
BUILDX_BUILDER=your-compatible-builder make test-package
```

This entrypoint runs actual Dockerfiles in a disposable repository: revision A
at T1, revision B at T2 with identical admitted inputs, a warm B build, two
independent no-cache B builds, then two same-size dirty input edits at the same
revision and epoch. Every build verifies the embedded source manifest and exact
Cosmoformer license bytes before admitting an image receipt. A and B must have
different revision/image identities; only builds of B are required to match.
Receipts and comparisons are under `.build/package-cache-<UUID>/`. Tags have an
invocation-specific prefix and cleanup never removes another builder or cache.
The ordinary current-checkout comparison also retains its two complete receipts
and archives under `.build/package/repro-<UUID>/`.

#### Interrupted and intermediate validation (historical)

The first branch attempt exhausted host storage, causing a BuildKit metadata I/O
error and stopping Docker Desktop. Its partial exports, lost-database failures and
50/52 ordinary browser result remain in `.build/m15-fix-validation/`; they supply
no final success evidence. Initial source digests `33e6f14e…` and `d6220c91…` are
intermediate snapshots. After storage recovery, diagnostic runs and deliberately
cancelled obsolete builds are retained separately in `.build/m15-fix-resumed/`.
Their browser successes never converted a failed overall package command into a
pass. No retry loop or assertion relaxation was added.

The resumed worker comparison identified one additional differing entry in layer
2: `/var/cache/ldconfig/aux-cache`. Its tar ownership, mode and normalized mtime
matched, but its contents differed. glibc's [auxiliary-cache implementation](https://github.com/bminor/glibc/blob/glibc-2.41/elf/cache.c#L751)
keys entries by inode, change time, size and device, so that generated build cache
is unsuitable for a reproducible layer. Both worker apt operations now remove it
in their creating layer, alongside build logs. The actual runtime
`/etc/ld.so.cache`, libraries, package database and licenses remain present.
`worker-diff.json` records the comparison; the independent archive was retained.

The first resumed acceptance coverage run passed three shards but failed three
tests in shard 1. Two failures explicitly returned `CLOCK_UNRELIABLE`; normal
execution/planning fixtures used a frozen elapsed clock while PostgreSQL wall
time advanced. A new real HTTP regression establishes an observation, waits
1.2 seconds and requests permission. It reproduced `CLOCK_UNRELIABLE` before the
fixture correction and passed afterward. Normal execution/planning fixtures now
start elapsed progression once; explicitly controlled clocks remain available.
Production clock guards and migrations are unchanged. Both existing backwards-
clock ownership checks, all 28 focused execution/planning regressions and the
complete acceptance inventory passed afterward. Selection/setup mistakes and
pre-fix failures remain in their original diagnostic logs rather than being
counted as successful executions.

#### Historical pre-review validation — 2026-11-18

The pre-review source manifest covered **370 files**, HEAD
`8df68684acdfe7eab3a02332309c72e4cad2977d`, dirty **true**, epoch **1794960174**,
and source digest
`63703d6e7167d0313ed49f9f6deedeaa4feae1c0b2ea017b167350ea7d0c3c6c`.
These results bind to the pre-review source only. The three review corrections
and their fresh validation are recorded in the next section. No historical
receipt substitutes for verification of the corrected source.

The real cache fixture used clean revision A at **1794952974** and clean revision
B at **1794960174** with identical admitted file digests. A and B had distinct
revision/image identities. B matched its warm-cache build, independent no-cache
build and second independent no-cache build across **all five targets**. A dirty
LICENSE change retained B's revision, changed the source digest and changed every
image identity. `cache-proof.json` is retained in `.build/m15-fix-resumed/`; the full fixture
receipts are in `.build/package-cache-1be2f9e5-fa44-4bfc-bddd-61c11d8d725f/`. The ordinary dirty-checkout comparison also passed all five targets;
its original and independent receipts, plus independent-build archives, remain under
`.build/package/repro-9ca102ed-53d0-4d3e-9f81-4ca903cfb670/`.

| Compared target | Independently verified artifact checksums | Warm, cross-epoch, independent and current-checkout identity comparisons |
| --- | ---: | --- |
| platform | 2: executable and contract | All matched |
| proxy | 56: complete SPA inventory | All matched |
| browser-fixture | 2: platform executable and contract; fixture bytes also bound by image/layers | All matched |
| worker-opentofu | 1: worker executable; engine identity additionally checked at runtime | All matched |
| worker-terraform | 1: worker executable; engine identity additionally checked at runtime | All matched |

Independent inspection rehashed the archive, manifest and config blobs, every
uncompressed layer and every declared artifact. Loaded Docker IDs, RootFS layer
IDs, source labels/manifests, executable modes and private socket-directory
ownership/mode also matched. Worker auxiliary caches are absent and the runtime
linker cache is retained. Inspection reads nested tar streams in memory and never
extracts archive paths onto the host. Exact IDs/digests and counts are in
`.build/m15-fix-resumed/archive-verification.json`.

The tested builder was an owned `docker-container` builder using the exact
BuildKit 0.29.0 pin documented above, with Linux amd64 outputs on Docker Desktop.
The classic/config-ID and containerd/manifest-ID receipt forms both have regression
coverage; the real local store exposed manifest IDs. Builds ran after their
selected commit timestamps, with valid host/builder clocks. Epoch changes
invalidate mutable RUN caches; unchanged epochs retain cache reuse, and pinned
base layers remain reusable. The builder was selected explicitly without changing
the global selected builder or Docker settings.

| Exact command / entrypoint | Final result and retained evidence |
| --- | --- |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node --test scripts/package-build.test.mjs scripts/package-reproducibility.test.mjs scripts/package-fixture.test.mjs` | **19 passed**, zero failed/skipped. `helpers-complete.log`. Covers source/dirty receipts, missing/malformed inputs, comparison/build/cleanup failures, complete-inventory preservation and SIGINT/SIGTERM child cleanup. |
| `make test-scripts test-contract-coverage` | **151 script/workflow tests** and **1 standalone contract test passed**, zero failed/skipped. `scripts-complete.log`. |
| `GOCACHE="$PWD/.build/go-cache" GOPATH="$PWD/.build/gopath" .build/tools/go/bin/go test -race -count=1 -json ./internal/... ./cmd/...` | **194 top-level tests in 20 tested packages passed**; no test skips. The separate testsupport package has no test files. `unit-complete.jsonl`. |
| `.build/tools/go/bin/go vet ./...` and `.build/tools/go/bin/go vet -tags=integration ./...` | Both passed after the fixture changes. `vet-complete.log`. The repository Go cache/module paths were supplied as in Make. |
| `make test-go-coverage` | **290 backend top-level tests in 21 packages passed**; acceptance **102 passed, 5 expected opt-in skips**. Actual executed shard selections **27/27/27/26** form a complete, disjoint **107-test** inventory. Linux coverage suites passed. Go gate: **84.57% (10541/12464 statements)**, required >80%. `go-coverage-final.log`, `acceptance-complete.json`, current `.build/coverage/go.*.log`. |
| `make test-integration` with the focused selection below | **28 focused top-level regressions passed** with the owned DSN, marker and container selected by Make; no assertion or production clock-policy changes. `fixture-regressions.log`. The complete integration/acceptance inventory was exercised through the documented backend/four-shard coverage entrypoint above; the pre-fix unsharded attempt was retired after reproducing the clock-fixture defect and is not claimed as a pass. |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node scripts/operator-recovery.mjs` | **9 top-level tests passed**, actual PostgreSQL dump/restore exercised, owned exercise database stopped. `operator-complete.log`. |
| `make test-source test-native`; final `make test-native` after the fixture edits | Linux source/supervision: **57 top-level passes**. OpenTofu **177** and Terraform **169** nested subtest pass records; both matrix roots passed, zero skips/failures. Fixture-supply and Make-recipe prerequisites executed successfully. `source-native-final.log`, `native-complete.log`, `native-counts.json`. |
| `make test-web` | Type checking, production build, **250 frontend tests in 20 files**, and **52 ordinary Chromium/Firefox journeys passed**, zero skips. Frontend coverage: **99.18% lines, 85.91% branches**. `web-complete.log`. |
| `make test-runtime-coverage` | Both actual storage and seed checks passed, with real Node-child coverage; no skips. `runtime-complete.log`. |
| `BUILDX_BUILDER=cosmoformer-m15-repro-resumed make test-package` | **2/2 top-level package tests passed**, zero skips: actual cache/epoch/dirty-input regressions, all-five-target independent reproduction, **62 packaged runtime checks** and **52 packaged Chromium/Firefox journeys**. `package-complete.log`. TLS, CSP, read-only/non-root boundaries, worker identity, migration inventory and uncertain-ownership restart assertions remain intact. |
| `BUILDX_BUILDER=cosmoformer-m15-repro-resumed make package` and `BUILDX_BUILDER=cosmoformer-m15-repro-resumed make test-package-browser` | Both supported entrypoints passed; standalone packaged browsers **52/52**. Each newly exported inventory matched the full-command identities for its targets. A subsequent actual five-target build produced a fresh complete receipt, matched all full-command identities, and passed all **62 runtime checks again**. No earlier receipt was copied back. `entrypoints.log`, `entrypoints-result.json` and the three entrypoint comparison JSON files. |
| `COSMOFORMER_SONAR_CONTAINER=<owned-container-id> COSMOFORMER_SONAR_ORIGIN=http://127.0.0.1:50846 make test-overall-coverage` | All six fresh reports imported successfully. **81.4716% combined (17184/21092)** and **80.7771% lines (14989/18556)** passed unchanged strict >80% gates. Exact local analysis identity is in `.build/coverage/overall.json`; `overall-coverage.log`. The owned ID was supplied from `sonar-container.txt`. |

The focused entrypoint command used the following literal selection (the full
inventory was checked separately through the four coverage shards):

```sh
make test-integration TEST_ARGS="-count=1 -run='TestExecution|TestNativePlanningJourneys|TestHistoricalChangesIgnoreNewerUnacceptedAnalysis|TestWorkspaceUnavailableExactEvidenceAndIORevocation' -v"
```

The five acceptance opt-ins are `TestNativeMakeRecipe`, `TestNativeFixtureSupply`,
`TestNativeEngineCompatibility`, `TestBrowserServer` and
`TestPackagedRestartPreservesUncertainOwnership`. They were exercised by the
native prerequisites/matrices, both browser entrypoints and packaged restart
checks respectively. No opt-in skip substitutes for its required real suite.

The pre-review full-command receipts were captured separately before standalone
entrypoint checks in `.build/m15-fix-resumed/`. The active `.build/package/`
receipt, source snapshot, archives and runtime verification are replaced by the
fresh validation below; they must not be cited as the pre-review source. The local Sonar
scanner credential file was removed by the workflow. Owned PostgreSQL, coverage
container/network, temporary comparison tags and the dedicated builder were
removed after validation; final cleanup evidence is retained in
`.build/m15-fix-resumed/`. Shared builders/caches and historical merge evidence
were preserved. No image/package was published and no deployment was performed.

**The pre-review run completed, but its sign-off is superseded by the review corrections below.**
The changes remain uncommitted for review. Company adoption remains milestone 16;
production host routing/ownership mappings, real TLS issuance/renewal, live
Bitbucket/AWS/artifact permissions, physical WAL/PITR restoration and capacity/RPO/
RTO remain outside this local synthetic validation.


#### Review corrections and diagnostic runs — 2026-11-18

Three P2 review findings affected verification reliability:

- A Docker removal error could bypass lock release and signal-listener removal.
  Finalization now uses independent `finally` blocks. A failed checksum copy and
  failed cleanup retain both errors. Each stopped checksum container is recorded
  in a private, uniquely named `build-resources-<UUID>.json` in its output directory
  before copying. A failed removal keeps that receipt even across later builds;
  successful removal clears only the current invocation's receipt. Docker removal
  has a ten-second timeout. After Docker recovery, inspect the recorded container
  ID and remove that owned container explicitly; do not prune other resources.
- Cancellation sent TERM once and could wait forever. Package builds now reuse
  the existing process-group supervisor: TERM, a 1.5-second grace period, then
  KILL for remaining members of that invocation's group. The supervisor checks
  descendants even when the original group leader has exited. SIGINT and SIGTERM
  regressions exercise both an ignoring child and a surviving ignoring descendant,
  verify bounded completion and lock release, and keep an unrelated sentinel alive.
- Synthetic commits could invoke inherited Git signing tools, hooks or templates.
  Disposable fixture Git commands now use an isolated environment, remove inherited
  Git repository/config selectors, ignore global/system configuration, and explicitly
  disable signing, hooks and templates. Synthetic author identity and commit dates
  remain explicit. The package-state test helper uses the same isolation. A hostile
  synthetic signing/hook configuration verifies clean A/B commits at distinct epochs,
  identical trees and no external signer/hook invocation. No user Git settings change.

The focused helper suite passes **25 tests**, zero failures or skips, including
six new regression cases for these findings. Evidence is retained under
`.build/m15-p2-validation/`. The initial restricted-process test attempt could not
inspect process groups; it is preserved as `helpers-restricted-attempt.log` and
is not counted as a successful run. `helpers.log` records the complete successful
run with process inspection available. Earlier review reproductions remain under
`.build/m15-precommit-review/`.

The failed overlapping web/script validation attempt is retained as
`scripts-overlap-failed.log`: both entrypoints regenerate the same frontend
validators directory. The complete web run finished successfully, then the script
and standalone contract suites ran sequentially and passed **157 + 1 tests**,
zero failures/skips. This rerun changed no source, assertion, exclusion or threshold.

The first Go coverage attempt overlapped emulated package/native builds and a
second race-enabled acceptance run. Its four acceptance groups reached the
existing 15-minute command limit; one test also reached its deliberately short
three-second plan lifetime before its initial approval. These are retained in
`go-overlap-failed/` and `go-coverage-overlap-failed.log`, not counted as passes.
PostgreSQL activity inspection found no lock backlog. A process sample and
resolved symbols (`acceptance-sample.txt`, `sample-symbols.txt`) showed Argon2
hashing with race instrumentation. The final Go coverage run is scheduled after
the other heavy suites, using the unchanged command, limits and assertions; no
failed or partial profile is merged into the final report. The isolated diagnostic
`make test-integration TEST_ARGS='-count=1 -run=TestSavedPlanApplyExpiryWhileWaitingForGate -v'`
passed the same expiry test (17.17 seconds) with its three-second lifetime and
all assertions intact; see `expiry-isolated.log`. This is a diagnostic result,
not a substitute for the complete coverage run.

The initial P2 source had digest
`e4f5a8f1a80228908250231e8daa87ef102e9e2d087dc3095461b5298e4ef814`.
Its real cache regression and archive inspection passed. Its complete package
command was deliberately interrupted before sign-off to refine the new process
test: a killed Linux orphan may remain a zombie briefly, so the assertion now
rejects running processes while accepting a reaped process or zombie, matching
the existing supervisor contract. Test teardown also kills its exact owned
transport group on assertion failure. The cancelled command remains a failure;
its later browser result does not replace the missing complete-command pass.
Those diagnostic records remain in `.build/m15-p2-validation/`.

#### Retired snapshot: same-size transfer failure — 2026-11-18

The rejected snapshot covered **371 files**, HEAD
`8df68684acdfe7eab3a02332309c72e4cad2977d`, dirty **true**, epoch **1794960174**,
and source digest
`2b4767368963db6bd1dd97d4d110d14fb8c56b1b567722a985ff7addc6c531ea`.
The helper suite passed, but archive inspection exposed stale embedded source
metadata: the platform image label and receipt named `2b476736…c531ea`, while the
first layer contained source digest `e4f5a8f…ef814`. The host snapshot contained
the newer manifest. `.build/m15-same-size-manifest-failure/` preserves the complete
platform archive, expected source/receipt, builder metadata and `proof.json`;
`.build/m15-p2-final/` retains both failed archive checks and this retired run.
The package command was cancelled and is not counted as a pass.

BuildKit 0.29.0's [default local differ](https://github.com/moby/buildkit/blob/v0.29.0/client/llb/source.go)
uses file metadata to decide whether to retransmit a file. Rewriting snapshot
mtimes to the same commit epoch hid same-size dirty edits from that transfer
step. Labels could still change because they are separate build arguments.
The final correction keeps fresh snapshot mtimes, while retaining epoch-bound
mutable-stage caches and export-time timestamp rewriting. The admitted source
bytes remain frozen; no fixed source timestamp hides their identity. The builder
now copies and verifies each image's embedded source manifest and license before
writing its complete receipt. Stale manifests/licenses fail closed and retain
diagnostics while releasing owned containers, locks and signal listeners.

The real cache regression now makes two different same-size LICENSE edits,
keeping revision and epoch fixed and keeping dirty state true for the second
edit. Each must change the source digest and every image identity; embedded
manifest and license verification must pass on every actual build. Two new
transport regressions reject stale embedded source or license data despite
otherwise matching image labels. The focused helper suite passes **27 tests**,
zero failures/skips; evidence is in `.build/m15-p2-complete/helpers.log`.

The completed manifest-transfer validation below superseded both retired
snapshots. A subsequent cancellation/Git-isolation review required the additional
fixes and fresh validation recorded at the end of this guide.


#### Historical manifest-transfer correction validation — 2026-11-18

The final admitted snapshot covers **371 files**, HEAD
`8df68684acdfe7eab3a02332309c72e4cad2977d`, dirty **true**, epoch **1794960174**,
and source digest
`df976882bafed83987a15f3c8425054823ac08435e93e1d4c377522021b341a5`.
Fresh evidence is retained in `.build/m15-p2-complete/`.

The targeted real-Docker probe passed a clean build and two same-size dirty
LICENSE edits at the same revision/epoch. Each source digest and image ID changed,
and each actual embedded source manifest and license matched its admitted bytes.
All three archives, embedded manifests, licenses and receipts are retained in
`same-size-probe/`; `same-size-probe/result.json` records the checks. The ordinary
five-target build and independent archive inspection also passed the new manifest
and license checks, including exact source identity, config/manifest/archive and
layer hashes, artifact checksums, executable modes and private-directory ownership.
The source-transfer correction used the existing populated builder cache.

All final commands below passed against this correction. Logs and JSON evidence
are in `.build/m15-p2-complete/`; `summary.json` and `evidence-hashes.json` record
source identity, exact counts, comparisons and retained report hashes. Direct Go
commands used `GOCACHE=$PWD/.build/go-cache` and `GOPATH=$PWD/.build/gopath`; Make
supplied the pinned tools and the marked database's owned DSN/marker/container.

| Exact command / entrypoint | Final result |
| --- | --- |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node --test scripts/package-build.test.mjs scripts/package-reproducibility.test.mjs scripts/package-fixture.test.mjs` | **27 passed**, zero failures/skips. `helpers.log`. Includes bounded SIGINT/SIGTERM cancellation of ignoring children/descendants, unrelated sentinel preservation, failed Docker removal releasing lock/listeners without losing either error or resource ID, isolated Git configuration, and stale embedded manifest/license rejection. |
| `make test-scripts test-contract-coverage` | **159 + 1 passed**, zero failures/skips. `scripts.log`. Frontend and script generators ran sequentially. |
| `.build/tools/go/bin/go test -race -count=1 -json ./internal/... ./cmd/...` | **194 top-level tests in 20 packages with tests passed**, no test skips. The testsupport package has no tests. `unit.jsonl`. |
| `.build/tools/go/bin/go vet ./...` and `.build/tools/go/bin/go vet -tags=integration ./...` | Both passed. `vet.log`. |
| `make test-go-coverage` | **290 backend top-level tests passed**; acceptance **102 passed, 5 expected opt-in skips**, zero failures. Fresh inventory **107**, executed in complete/disjoint shards **27/27/27/26**. Go statement coverage **84.54% (10538/12464)**, unchanged gate >80%. `go-coverage.log`, `go.*.log`, `acceptance-complete.json`. |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node scripts/operator-recovery.mjs` | **9 passed**, actual PostgreSQL dump/restore; owned recovery database removed. `operator.log`. |
| `make test-source test-native` | Linux source/supervision **57 passed**. OpenTofu **177** and Terraform **169** nested pass records; both matrix roots passed, zero failures/skips. `source-native.log`, `native-counts.json`. |
| `make test-web` | Type check, production build, **250 tests in 20 files**, and **52 Chromium/Firefox journeys passed**. Frontend **99.18% lines, 85.91% branches**. `web.log`. |
| `make test-runtime-coverage` | Both real storage/seed checks passed with child-process coverage, zero skips. `runtime.log`. |
| `BUILDX_BUILDER=cosmoformer-m15-p2-validation make test-package` | **2/2 passed**, zero failures/skips: all-five-target warm/cross-epoch/two-cold/repeated same-size dirty regressions; actual dirty-checkout independent reproduction; **62 runtime checks** and **52 packaged Chromium/Firefox journeys**. `package.log`, `cache-proof.json`, full fixture receipts under `.build/package-cache-ffd2ff89-5efa-43d4-852c-3b891407e53c/`. |
| `BUILDX_BUILDER=cosmoformer-m15-p2-validation make package` and `BUILDX_BUILDER=cosmoformer-m15-p2-validation make test-package-browser` | Both supported entrypoints passed, standalone browsers **52/52**. Newly exported images matched full-command identities. A subsequent actual five-target build produced a fresh complete inventory and passed all **62 runtime checks again**. `entrypoints.log`, `entrypoints-result.json`, comparison JSONs and saved `full-command-*.json`; no receipt was copied back. |
| `COSMOFORMER_SONAR_CONTAINER=<owned-id> COSMOFORMER_SONAR_ORIGIN=http://127.0.0.1:59463 make test-overall-coverage` | All six fresh reports imported. **81.4711% combined (17223/21140)** and **80.771% lines (15021/18597)** passed unchanged strict >80% gates. `overall-coverage.log`, `.build/coverage/overall.json`. The owned ID came from `.build/m15-p2-validation/sonar-container.txt`. |

The final coverage run used a freshly recreated marked database and no overlapping
heavy suite. All four acceptance groups passed the original 15-minute command
limit, including the unchanged three-second expiry test. The five opt-ins listed
above were exercised by the actual native, ordinary-browser and packaged-restart
suites; none substitutes for an omitted required check. No assertion, exclusion,
coverage threshold or retry policy was weakened.

Every required identity matched for **platform, proxy, browser-fixture,
worker-opentofu and worker-terraform**: source/revision/dirty/epoch, artifact
checksums, loaded image ID, manifest/config digest and archive checksum. Actual
Created values and RootFS layer lists also matched. Complete original and
independent receipts plus independent archives remain in
`.build/package/repro-cfce06b9-6d73-4318-b513-db9b633475f6/`. The active complete
inventory, source snapshot, five archives and runtime receipt remain in
`.build/package/`. Archive inspection independently verified **62 declared artifact
checksums**, all layer hashes and all **five Cosmoformer license files**; it never
extracted archive paths onto the host. `archive-verification.json` retains exact
IDs/digests and target counts.

The task-owned pinned builder, marked PostgreSQL database and coverage server/
network were removed. Shared builders/caches and historical archives were
preserved. Successful comparison references were cleaned; references and resource
receipts from the deliberately interrupted historical comparison remain available
for diagnosis. The scanner credential file was removed by the workflow. Exact
cleanup status is recorded in `cleanup.json`. Repository changes remain
uncommitted, with no publication or deployment.

**This historical source passed milestone 15's local packaging verification.**
The subsequent review found the two reliability gaps described below; its passing
results do not validate the newer source. Hosted CI has not run for
these uncommitted changes. Company adoption remains milestone 16; real production
routing/ownership, TLS issuance/renewal, Bitbucket/AWS/artifact permissions,
physical WAL/PITR and capacity/RPO/RTO remain outside this local synthetic proof.


#### Historical cancellation and fixture Git isolation validation — 2026-11-18

The follow-up review found two P2 reliability defects. Docker loading, inspection,
container creation, copying and removal still used synchronous calls. While one
blocked, Node could not process the build's SIGINT/SIGTERM handlers. Also, the
cache fixture isolated Git while creating its commits, but its imported snapshot
builder inherited the developer's Git configuration for source queries. A global
`core.fsmonitor` could execute during those queries, and inherited repository or
index selectors could redirect them.

Every Docker operation in `buildPackages` now uses the existing process-group
supervisor. Interruption requests termination, waits 1.5 seconds, and escalates to
KILL for ignoring children or descendants. Captured stdout is drained separately
from diagnostics, preserves split UTF-8 sequences and has a 16 MiB bound. Cleanup
runs with an independent ten-second deadline and the same termination escalation,
so an already-aborted build can still remove its owned container. Failed cleanup
retains both the primary error and the uniquely named recovery receipt while
releasing the build lock and signal listeners. A container name is recorded
before creation starts, allowing recovery when interruption loses the ID response.
Only that recorded container is removed; no shared cache or builder is pruned.

The fixture passes its same isolated Git executor into every source query in
`buildPackages`: epoch, tracked/untracked inputs, revision and dirty state. The
executor strips inherited `GIT_*` settings and disables global/system configuration,
hooks, templates and signing for the disposable history. This stays local to the
fixture; it does not change the process environment or the developer's settings.
Normal repository builds retain their existing Git configuration behavior.

End-to-end regressions run the actual builder with only Docker's transport
replaced. Both signals are delivered during ignoring load, copy, create and remove
operations. Tests observe bounded exit, lock/listener release, owned-resource
cleanup or recovery receipts, a live unrelated sentinel and a successful subsequent
build. Two more regressions run the actual seven-build cross-epoch fixture with
hostile global configuration and repository/index selectors. They require clean
A/B histories, the exact two-hour epoch difference, unchanged admitted bytes,
strict warm/cold comparisons and changed identities after both same-size edits.
The injected filesystem-monitor/signing hook must never execute.

The admitted source is **371 files**, HEAD
`8df68684acdfe7eab3a02332309c72e4cad2977d`, dirty **true**, epoch **1794960174**,
source digest `2008b532f285aa7c5f0967160b40852bce280e5128e0bd63d8dd500cb6a8bd4e`.
Fresh validation passed in `.build/m15-final-p2-validation/` using the owned
pinned builder `cosmoformer-m15-final-p2`. Historical successful reports above
remain separate and are not substituted for this run.


All commands below passed on this final source. Make supplied the pinned tools and
owned database DSN/marker/container; direct Go commands used the project GOCACHE
and GOPATH. Frontend generation and script generation ran sequentially. The full
Go coverage run followed image validation, with no overlapping heavy job and the
original four-shard, 15-minute-per-group limits intact.

| Exact command / entrypoint | Final result and evidence |
| --- | --- |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node --test scripts/package-build.test.mjs scripts/package-reproducibility.test.mjs scripts/package-fixture.test.mjs scripts/workflow-lib.test.mjs` | **46 passed**: 37 package helpers and nine supervision helpers, zero failures/skips. `helpers.log`. Includes both signals during load/copy/create/remove, ignoring children/descendants, failed cleanup diagnostics, actual seven-build fixtures under both hostile-Git configurations, and split UTF-8/output capture limits. |
| `make test-scripts test-contract-coverage` | **170 + 1 passed**, zero failures/skips. `scripts.log`. |
| `.build/tools/go/bin/go test -race -count=1 -json ./internal/... ./cmd/...` | **194 top-level tests in 20 packages with tests passed**, no skips. `unit.jsonl`. |
| `.build/tools/go/bin/go vet ./...` and `.build/tools/go/bin/go vet -tags=integration ./...` | Both passed. `vet.log`. |
| `make test-source test-native` | **57 Linux source/supervision tests**; **177 OpenTofu** and **169 Terraform** nested passes, both matrix roots passed, no failures/skips. `source-native.log`, `native-counts.json`. |
| `make test-runtime-coverage` | **2 passed**, no skips. `runtime.log`. |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node scripts/operator-recovery.mjs` | **9 passed**, including actual PostgreSQL dump/restore. `operator.log`. |
| `make test-web` | Type checking, production build, **250 tests in 20 files** and **52 Chromium/Firefox journeys** passed. **99.18% lines, 85.91% branches**. `web.log`. |
| `BUILDX_BUILDER=cosmoformer-m15-final-p2 make test-package` | **2/2 passed**, no failures/skips: real seven-build cache fixture, actual-checkout independent rebuild, **62 runtime checks**, **52 packaged browser journeys**. `package.log`, `cache-proof.json`. |
| `BUILDX_BUILDER=cosmoformer-m15-final-p2 make package` and `BUILDX_BUILDER=cosmoformer-m15-final-p2 make test-package-browser` | Both passed; **52 more packaged browser journeys**. A subsequent actual five-target build produced a new complete inventory, matched all full-command identities and passed **62 runtime checks again**. `entrypoints.mjs`, `entrypoints.log`, `entrypoints-result.json`, comparison JSONs and `full-command-*.json`. |
| `make test-go-coverage` | **290 backend tests**; acceptance **102 passed, 5 expected opt-in skips**, zero failures. Fresh inventory **107**, complete/disjoint shards **27/27/27/26**. **84.56% (10540/12464)** Go statement coverage. `go-coverage.log`, `go.*.log`, `acceptance-complete.json`. |
| `COSMOFORMER_SONAR_CONTAINER=<owned-id> COSMOFORMER_SONAR_ORIGIN=http://127.0.0.1:51221 make test-overall-coverage` | All six fresh reports imported; **81.4965% combined (17252/21169)** and **80.8058% lines (15042/18615)** passed unchanged strict >80% gates. `overall-coverage.log`, `overall.json`, `aggregate-task.txt`. Owned ID retained in `sonar-container.txt`. |

The five acceptance opt-ins remain `TestNativeMakeRecipe`,
`TestNativeFixtureSupply`, `TestNativeEngineCompatibility`, `TestBrowserServer`
and `TestPackagedRestartPreservesUncertainOwnership`. Actual native, ordinary
browser and packaged-restart suites exercised these paths separately. No test
selection, assertion, coverage exclusion, threshold or retry policy was weakened.

Every strict comparison passed for **platform, proxy, browser-fixture,
worker-opentofu and worker-terraform**: source/revision/dirty/epoch, artifact bytes,
loaded image ID, manifest/config digest and archive checksum. Actual Created and
RootFS lists matched too. The cache fixture evidence is
`.build/package-cache-c8471028-9922-477f-b967-263c676673a0/`; independent actual
checkout receipts and archives are under
`.build/package/repro-02376fcd-06df-4e87-b0eb-897af75ba231/`. The fresh active complete
inventory and archives remain in `.build/package/`. Independent in-memory archive
inspection verified all layer hashes, source identities, all five Cosmoformer
license files, ownership/modes and **62 declared artifact checksums** without
extracting archive paths onto the host. `archive-verification.json` records exact
IDs/digests and per-target counts.

The requested Sonar review used the installed `sonar-review` workflow and this
exact scan selection, after all six coverage producers completed:

```sh
python3 /Users/ikyrannas/.codex/skills/sonar-review/scripts/sonar_review.py scan \
  --repo /Users/ikyrannas/personal/src/cosmoformer \
  --sources cmd,internal,scripts,web/app,web/scripts \
  --tests cmd,internal,scripts,web,tests \
  --context web/app/.generated/api.ts \
  --context web/app/.generated/validators.d.ts \
  --context web/.react-router/types \
  --go-coverage .build/coverage/go.out \
  --js-coverage .build/coverage/web/lcov.info \
  --js-coverage .build/coverage/contract/lcov.info \
  --js-coverage .build/coverage/workflows/lcov.info \
  --js-coverage .build/coverage/runtime/lcov.info \
  --js-coverage .build/coverage/packaging/lcov.info \
  --property sonar.typescript.tsconfigPaths=web/tsconfig.json \
  --expect-language go --expect-language js --expect-language ts
```

The gate is **OK**, with three evaluated conditions: **95.7% new-code coverage**,
**0.0% new duplication**, **zero new violations**. Unlike the previous review,
this analysis has a `PREVIOUS_VERSION` baseline: the earlier local review snapshot
`64147d917307`, analyzed at `2026-11-18T15:00:08Z`. This is a local branch project,
not native PR analysis or hosted CI. The quality gate/profiles were unchanged. The helper's generic first-analysis
notice does not apply to this returned previous-version period.
The exact task is `b0a605bc-594a-4884-adf4-cff42e7f8771`, analysis
`876dd62e-2442-4950-81a0-c791f05b7bf5`, sanitized snapshot
`a2a7dd97d419a37ed67d1fd10e8a00ebe1224a3a7e275b416437e9d9f1d19aea`.
The separate aggregate task/analysis are
`de077673-fe24-42a1-a1a3-99dc6f25557c` /
`05a2021e-e08c-49c2-8a91-5e02b8d5fd81`.

Sonar analyzed **148 source files**: Go 96, JavaScript 25, TypeScript 26 and CSS 1.
Its snapshot included 387 Git-listed files plus 12 explicit generated type-context
files; the two `.env.example` files were excluded. It imported one fresh Go and
five fresh JavaScript profiles, and reported **81.5% overall coverage**, **80.8%
lines**, **0.3% duplication**, and **zero hotspots**. There were no parsing,
coverage-import or expected-language failures. One warning concerns language-file
selection in the sanitized snapshot; unsupported SQL/Dockerfile analysis is not
claimed. Changed Dockerfiles were reviewed manually. Final documentation adds
these results after the scan; analyzed source bytes remain unchanged.

The **same 11 open Sonar flags** remain: five bug labels, four vulnerability labels
and two smells, with unchanged overall reliability/security ratings of D. Source
triage found no new actionable correctness or confirmed security defect:

- `S5850` / `S2871`: existing independent regex alternatives have the intended
  anchors; filename strings deliberately use lexical sorting for canonical hashes.
- `S1143`: failed cleanup must fail the command. Build cleanup preserves primary
  and cleanup errors in `AggregateError`; cache cleanup throws only without an
  earlier failure. End-to-end tests observe errors, receipts and released locks.
- `S4036`: PATH selects operator-owned local tools; no cross-principal input reaches
  this command selection and no shell interpolation was introduced.
- `S5443`: the existing absent-cluster negative check uses an isolated read-only
  UID-70 container with a private mode-0700 tmpfs, rather than shared host `/tmp`.
- `S6845`: unchanged focusable scroll containers in the frontend are outside this
  packaging patch. Ordinary and packaged browser checks passed.

The requested `security-audit` workflow was applied as a **focused, read-only
source review** of changed packaging, supervision, Git, inventory and deployment
boundaries, with Sonar findings triaged against those paths. It found **zero new
confirmed security vulnerabilities**. This does not constitute a whole-codebase
penetration test or verification of company infrastructure. Functional regressions
and the full local validation are reported separately above. Full scan reports,
issues, hotspots, snapshot metadata and scanner logs are retained in
`sonar-review/`; `sonar-review-result.json` records the triage and limitations.

Initial regression development exposed synthetic-transport bookkeeping inside the
fixture's Git tree; the bookkeeping was separated before the final source was
frozen. Those development logs remain separate from final results. The first
temporary aggregate Sonar service used an internal network without a usable host
port; only that owned service/network was recreated before the successful scan.
Its startup error is retained. Neither event was hidden or promoted as a pass.

`summary.json`, `state.json` and `evidence-hashes.json` identify the final source,
counts, report hashes and retained comparisons. The owned builder, marked database,
and aggregate Sonar container/network were removed; the personal Sonar service was
stopped while preserving history. Shared builders/caches and historical diagnostic
images/archives were preserved. Fresh successful comparison references and the
build lock were cleared, and the scanner credential file was removed.
`cleanup.json` records the checks. All changes remain uncommitted; nothing was
published or deployed.

**This historical source passed the listed validation.** The subsequent review
found an untested process-inspection failure path; the correction and fresh
validation below supersede its local sign-off. Hosted CI has not run on
these uncommitted changes. Company adoption remains milestone 16, including real
production routing/TLS/service permissions, physical WAL/PITR and capacity/RPO/RTO.


#### Process-inspection failure correction — 2026-11-18

The final follow-up review found one remaining P2 cancellation gap. If `/bin/ps`
was missing or denied, process-group inspection rejected before KILL escalation.
The package runner suppressed that rejection and continued awaiting child exit.
An ignoring Docker child could therefore keep the parent and build lock alive
indefinitely despite the intended cancellation and cleanup deadlines.

The supervisor now attempts forced termination of its known owned process group
without depending on successful inspection. Inspection calls have a bounded
one-second timeout with KILL escalation, and near-zero remaining time slices do
not launch an inspector that would immediately time out. Both the grace phase
and the subsequent two-second exit/pipe-drain phase use absolute deadlines. A
failed or unverifiable shutdown reports an aggregate of the inspection/signaling
errors and the owned process-group ID. On bounded failure it releases its own
child handles and pipes, allowing the caller to report failure even if the OS
refuses signaling. It never selects processes by name or signals unrelated groups.

The package runner races child completion against cancellation completion, rather
than waiting for child exit before observing a shutdown error. The original
SIGINT/SIGTERM reason and supervision errors remain visible together. Container
cleanup still has its separate ten-second deadline after interruption; failure
retains its owned-container receipt, while locks and signal listeners are released.
An inspection error remains a reported failure even when group KILL succeeds,
because inspection could not confirm the complete group state.

Twelve new end-to-end cases run the real snapshot builder and supervisor, replacing
only Docker transport and routing the inspector system call to a real nonexistent
or non-executable fixture file. The resulting OS errors are `ENOENT` and `EACCES`.
They cover both signals, ignoring children, surviving pipe-holding descendants,
load/copy interruption, removal under its independent deadline, and denied group
signaling. The outside controller uses real inspection to check every owned PID,
container receipts, error preservation, released locks/listeners, a live unrelated
sentinel and a successful subsequent build. If the OS denies signaling too, the
caller must return a bounded failure with its recovery identity; only the outside
fixture controller then kills that known group. Existing assertions and grace
periods remain unchanged.

The independent offline before/after check uses a locally available container
where `/bin/ps` is genuinely absent. Before: parent, ignoring child and lock still
present after five seconds. After: nonzero parent exit in about 1.6 seconds, child
no longer executing, lock released, original interruption and inspection errors
visible, unrelated sentinel alive. It runs without network, with an empty
allowlisted environment, read-only public source/toolchain, 16 MiB scratch tmpfs,
one CPU, 256 MiB RAM, 32 PIDs, 128 open files, 1 MiB file limit and a 20-second
wall limit. Only trusted-controller observations are retained in
`.build/m15-inspection-final/inspection-proof.json`; target scratch is
destroyed. The available container Node verifies the OS failure independently;
the project-pinned Node runs all host end-to-end regressions.

The first correction snapshot was
`b1af42c3e50380aa79eb9f03cf97184e99890960912900fba83b8cff5f3065a8`.
Its 58 helpers passed, but fresh coverage inspection showed that disposable
fixture copies did not measure the repository module's error paths. Three direct
repository-module supervision regressions were therefore added. These cover
missing/denied inspection and denied group signaling through real child processes
and assert bounded errors, recovery identity and unrelated-process preservation.
Their isolated coverage run measured 45/46 changed supervisor executable lines
and 25/27 changed branches. No production code or coverage gate was changed to
raise coverage. The package run was deliberately interrupted before completion;
its partial results and development logs remain historical under
`.build/m15-inspection-fix-validation/` and are not promoted as final validation.

The expanded-test snapshot
`ba0bf4f5f84c896fd8362d91e69f872b01a893edb7237b2c5c72d6376834adc0`
passed full functional validation and both aggregate coverage gates under
`.build/m15-inspection-complete/`. Sonar measured 90.6% new-code coverage, but its
gate was red on one new `S9383` promise-intent warning. The completion observer
watches `done`, which only resolves; awaiting it would recreate the cancellation
hang. It is now explicitly marked `void`, with its fulfillment-only intent
explained. This changes no shutdown behavior and disables no rule. Static
preflight cleared the warning; it imported no coverage and is not the final
validation. All validation was subsequently regenerated for the annotated source
below. Those preceding reports remain historical.


#### Final annotated-source validation — 2026-11-18

The final admitted source covers **371 files**, HEAD
`8df68684acdfe7eab3a02332309c72e4cad2977d`, dirty **true**, epoch **1794960174**,
source digest
`65491d813902fa2142685da42eb600cebca4c186bbd2ee48a8b8a97c50070728`.
All final evidence is under `.build/m15-inspection-final/`; older successful or
interrupted candidates are not substituted for this source. The owned pinned
builder was `cosmoformer-m15-inspection-fix`. Its image, driver and cache assumptions
remain the same as above; shared builders/settings were unchanged.

| Exact command / entrypoint | Final result and evidence |
| --- | --- |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node --test scripts/package-build.test.mjs scripts/package-reproducibility.test.mjs scripts/package-fixture.test.mjs scripts/workflow-lib.test.mjs` | **61 passed**: 49 package helpers and 12 supervision helpers, zero failures/skips. `helpers.log`. Includes 12 missing/denied-inspection end-to-end cases and three direct repository-module fault-path tests. |
| `make test-scripts test-contract-coverage` | **185 + 1 passed**, zero failures/skips. `scripts.log`. |
| `.build/tools/go/bin/go test -race -count=1 -json ./internal/... ./cmd/...` | **194 top-level tests in 20 packages with tests passed**, no skips. `unit.jsonl`. |
| `.build/tools/go/bin/go vet ./...` and `.build/tools/go/bin/go vet -tags=integration ./...` | Both passed. `vet.log`. |
| `make test-source test-native` | **57 source/supervision tests**; **177 OpenTofu** and **169 Terraform** nested passes, both matrix roots passed, no failures/skips. `source-native.log`, `native-counts.json`. |
| `make test-runtime-coverage` | **2 passed**, no skips. `runtime.log`. |
| `.build/tools/node-v26.13.0-darwin-arm64/bin/node scripts/operator-recovery.mjs` | **9 passed**, including actual PostgreSQL dump/restore. `operator.log`. |
| `make test-web` | Type checking, production build, **250 tests in 20 files** and **52 Chromium/Firefox journeys** passed. **99.18% lines, 85.91% branches**. `web.log`. |
| `BUILDX_BUILDER=cosmoformer-m15-inspection-fix make test-package` | **2/2 passed**, zero failures/skips: all-five-target cache/epoch/warm/two-cold/repeated same-size edits, actual-checkout independent reproduction, **62 runtime checks** and **52 packaged browser journeys**. `package.log`, `cache-proof.json`. |
| `BUILDX_BUILDER=cosmoformer-m15-inspection-fix make package` and `BUILDX_BUILDER=cosmoformer-m15-inspection-fix make test-package-browser` | Both passed, **52 additional packaged browser journeys**. A subsequent actual five-target build produced a new complete inventory, matched full-command identities and passed **62 runtime checks again**. `entrypoints.mjs`, `entrypoints.log`, `entrypoints-result.json`, comparison JSONs and `full-command-*.json`. |
| `make test-go-coverage` | **290 backend tests**; acceptance **102 passed, 5 expected opt-in skips**, zero failures. Fresh **107-test** inventory is complete/disjoint across **27/27/27/26** shards. Go coverage **84.54% (10538/12464)**. `go-coverage.log`, `go.*.log`, `acceptance-complete.json`. |
| `COSMOFORMER_SONAR_CONTAINER=<owned-id> COSMOFORMER_SONAR_ORIGIN=http://127.0.0.1:53022 make test-overall-coverage` | All six fresh reports imported; **81.5411% combined (17312/21231)** and **80.8372% lines (15081/18656)** passed unchanged strict >80% gates. `overall-coverage.log`, `overall.json`, `aggregate-task.txt`; owned ID/origin retained in `sonar-container.txt` / `sonar-origin.txt`. |
| `python3 .build/m15-inspection-final/inspection-probe.py` | Bounded offline before/after check passed with genuinely missing `/bin/ps`; errors retained, owned child stopped, lock released and unrelated sentinel preserved. `inspection-proof.json`, `inspection-probe.log`. |

Make supplied the pinned tools and owned DSN/marker/container context. Direct Go
commands used the project GOCACHE and GOPATH. Frontend and script generation ran
sequentially; final Go coverage followed removal of the owned builder, with no
heavy image job overlapping its original 15-minute-per-group limits. The five
expected acceptance opt-ins remain the native Make/supply/compatibility,
ordinary-browser and packaged-restart tests named above; actual native, browser
and packaged runtime suites exercised their paths. No assertion, selection,
coverage exclusion, threshold, timeout or retry policy was weakened.

The first web attempt started before the owned database was ready and failed at
browser-server startup. Its log remains `web-before-database-ready.log`. After
correcting the invocation order and using a fresh Make process, the exact full
web command passed. This was an environment-context correction, not a code change
or a hidden retry. All final coverage reports came from successful final-source
producers after the coverage-start marker.

All strict identities matched for **platform, proxy, browser-fixture,
worker-opentofu and worker-terraform**: source/revision/dirty/epoch, artifact bytes,
loaded IDs, manifest/config digests and archive hashes. Created timestamps and
RootFS lists matched too. Cache receipts/comparisons remain in
`.build/package-cache-cecb0087-21b8-43e9-8ad9-7ef93352ab1d/`; actual-checkout
independent receipts and archives remain in
`.build/package/repro-a9bd1fd0-089a-420e-9a50-5cd50314d2e3/`. The active complete
inventory, snapshot, archives and verification remain in `.build/package/`.
Independent in-memory archive inspection verified every layer hash, source
identity, all five Cosmoformer license files, ownership/modes and **62 artifact
checksums**, without extracting member paths. `archive-verification.json` records
exact IDs/digests and counts.

The final Sonar scan used the exact source/test/context/language selection and
six-profile arguments shown above, through the installed `sonar-review` workflow.
The final **gate is OK**: **100.0% new-code coverage**, **0.0% new duplication** and
**zero new violations**, with all three conditions evaluated. Its
`PREVIOUS_VERSION` baseline is the preceding ba0bf4 candidate analysis at
`2026-11-18T17:49:43Z`, version `a8fa0ad51fbb`; therefore the final new-code scope
concerns the explicit observer annotation. The substantive shutdown correction
had 90.6% new-code coverage in that preceding scan, whose sole new warning was
`S9383`. The later static preflight imported no reports; this final scan imported
all six newly generated reports. The annotation resolved the warning through
explicit code intent. No gate, profile, exclusion or issue status was changed.

The final task is `3c4f85a7-dbee-47b7-b959-821e7f99c799`, analysis
`36e74dda-fa0e-4a8b-8c53-19b835f76ade`, sanitized snapshot
`450e83c9f27dab0dff47277ff0fa5dd3c2137241c1de4f3cbde2b2b0930c98e2`.
Aggregate task/analysis are `1056ce27-40d4-4bf8-b415-2653511f20e3` /
`e45c4f0e-af22-4671-a110-672cd4da541c`.

Sonar analyzed **148 source files**: Go 96, JavaScript 25, TypeScript 26 and CSS 1,
from 387 Git-listed files plus 12 approved generated type-context files. The two
`.env.example` files were excluded. Rounded overall coverage is **81.5% combined,
80.8% lines**, duplication **0.3%**, with **zero hotspots**. One warning concerns
language-file selection in the sanitized snapshot; there were no parser,
coverage-import or required-language failures. SQL/Dockerfile analysis is not
claimed; changed Dockerfiles were manually reviewed. Final docs record these
results after scanning; analyzed source bytes remain unchanged.

The same **11 existing open flags** remain: five bug labels, four vulnerability
labels and two smells, with overall reliability/security ratings of D. Their
source triage remains as documented above: intended lexical sorting/regex
alternatives, deliberate cleanup-failure propagation, operator-owned tool PATH,
the private-container absent-cluster negative test, and unchanged focusable
frontend scroll regions. No new actionable correctness or confirmed security
finding remains in the reviewed changes. A green local new-code gate does not
clear all-code flags and is not hosted CI or native PR analysis.

The requested `security-audit` workflow was used as a focused source review of
process ownership, bounded termination/drain, error preservation, cleanup receipts,
Git isolation and unchanged deployment boundaries. The offline probe used the
strict controls above, retained only trusted-controller observations and destroyed
target scratch. It found **zero new confirmed security vulnerabilities**. This is
not a whole-codebase penetration test or company-infrastructure validation. The
raw Sonar reports, issues, hotspots, snapshot and scanner log are retained in
`sonar-review/`; `sonar-review-result.json` records exact triage and limitations.

`summary.json`, `state.json`, `inspection-proof.json` and `evidence-hashes.json`
record source identity, counts, diagnostics, comparisons and report hashes. The
owned builder, marked database and aggregate service/network were removed. The
personal Sonar service was stopped with history/volumes retained. Shared builders,
caches and historical diagnostic images/archives were preserved; successful new
comparison references, build lock and scanner credential file were cleared.
`cleanup.json` records the checks. All changes remain uncommitted; nothing was
published or deployed.

**Milestone 15 is ready for local sign-off with the remaining P2 fixed.** There
are no remaining failures in final validation. Hosted CI has not run for these
uncommitted changes; company adoption remains milestone 16.
