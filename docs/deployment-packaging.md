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
`SOURCE_DATE_EPOCH`, then loaded into the local daemon. This avoids the daemon
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
no-cache platform/proxy build with retained first-build identities; differing
source, artifact/archive bytes or loaded image/manifest/config identities fail
verification. Missing identities or empty checksums also fail. Each builder
invocation replaces `images.json`; build the complete desired set in one call
or retain separate verified receipts before another invocation. On failure,
`repro-resources.json` retains the named first images for diagnosis.
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

`make test-package` builds all five targets and runs both container checks and
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

These results do not validate an actual Linux deployment host's ownership/network
mappings, externally issued TLS renewal, a public ingress or live Bitbucket OAuth/
webhook delivery, production AWS/artifact permissions, hostile multi-tenant code,
physical WAL/PITR restoration, capacity or recovery time. Restore/backup/key and
independent stop evidence remain operator responsibilities. Company adoption is
milestone 16 and compatibility remains `not_assessed`.
