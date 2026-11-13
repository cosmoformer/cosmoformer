# Cosmoformer

Cosmoformer is an early-stage, open-source infrastructure automation project. The local reference implementation provides accounts, access control, verified PR planning and source checks with a Go backend, PostgreSQL, and a React Router SPA using Astryx.

You can create local accounts, sign in, manage organizations and teams, and assign project/environment access. New accounts have no access. Administration, deployment, approval and raw-evidence permissions are separate. Check source performs Git/HCL preflight and permanent state-destination checks without native execution or an AWS session.

Explicit Plan requests now progress through separate preflight and execution assignments. Assessed compatible stacks can use a fresh Linux worker checkout, pinned module closure, guarded managed inputs and one fixed AWS session to run init → manifest seal → provider schema → saved plan → show JSON. Current source, authority, grants, runtime, inputs and exact worker/backend reservations gate execution. Existing source checks retain their immutable source-only intent. Verified PR and tracked-branch reconciliation now creates distinct native planning requests for enabled, assessed compatible stacks under current repository/resource/runtime gates. Automation never approves or queues Apply. Actual company compatibility remains `not_assessed`.

Readiness requires verified exact-version evidence, a sealed reviewed-input manifest, supported native analysis and an independent current managed-secret check. Scoped API reads expose sanitized Changes; raw evidence requires the separate raw-evidence grant. No changes is distinct from source-check success and includes output-only change detection. Logs retain redacted segments or explicit gaps. Missing responses, elapsed deadlines and signal delivery never prove stop or release uncertain ownership.

The run workspace supports exact-plan approval and own-vote withdrawal. An approval binds the plan ID/revision, retained manifest digest and Git commit after a fresh bounded Bitbucket read and current input/evidence checks. Counts use distinct enabled members with current scoped approval authority; eligible authors may count. Grant restoration can restore a current vote only while its plan remains valid. Withdrawal needs current scoped visibility and works during a provider outage. Review polling does not call Bitbucket or renew browser idle expiry. Saved-plan Apply requires a separate explicit confirmation of the exact stack, environment, source commit, plan revision and retained manifest. Insufficient current approvals reject the request without queuing work.

Schema version **13** adds saved-plan Apply retirement, exact operation identity and exactly-once apply-start epoch events while preserving migrations 001–012. The reference engines are OpenTofu **1.13.8** and Terraform **1.17.1**, Linux amd64. Exact AWS provider **6.41.0** schema profiles provide the selected warnings; other validated allowed-registry providers have generic coverage. Unsupported KMS execution remains blocked.

The run workspace supports scoped Cancel, separately confirmed Force Stop and formal recovery. Requests, delivery, observed controls and independently proven stop remain separate. Recovery evidence is versioned; clearance requires verified stop, reconciliation, distinct current approvers and current Apply permission. Diagnostic plans retain recovery-only provenance. Clearance requires a new normal plan and new normal approvals afterward.

Schema version **15** adds host-only maintenance, atomic offline key rotation, exact backup holds, clock recovery and restore reconciliation. Restore revokes replay authority, requires a genuinely new write key and routes adopted backends through existing recovery. See the [operator maintenance and restoration guide](docs/operator-guide.md#operator-maintenance-and-restoration-milestone-13) and run `node scripts/operator-recovery.mjs` with the pinned Node toolchain for a synthetic, isolated PostgreSQL restore exercise. Migrations 001–014 remain unchanged.

Schema version **16** adds safe artifact retention and cleanup while preserving migrations 001–015. Cleanup defaults to disabled; enabled daily scheduling and explicit administrator actions select at most 100 exact-version claims per bounded batch. Usable plans, live or uncertain attempts, recovery and declared backups remain protected. Failed claims retry their original version; completed deletion keeps a tombstone and historical evidence stays truthfully unavailable. Test backup protection and restoration before enabling cleanup. See [retention operations](docs/operator-guide.md#artifact-retention-and-cleanup-milestone-14).

There is no production release. Deployment packaging and company adoption remain milestones 15 and 16. Synthetic PostgreSQL/worker tests and the separate network-isolated native matrices do not establish live AWS/IAM or company compatibility. See the [planning behavior and boundaries](docs/operator-guide.md#native-planning).

## Local development

Requires Docker Desktop (running), Make, OpenSSL and Node to bootstrap tools. Use Go **1.27.3**, Node **26.13.0**, and the checksum-pinned PostgreSQL **18.6** image. Nothing installs into global tool directories.

```sh
make tools        # optional: checksum-verified Go/Node downloads into .build/tools
make deps
make dev-setup    # isolated local DB, explicit migrations, HTTPS certificate
```

Then run these in separate terminals:

```sh
make dev-api
make dev-web
```

Open [the local workspace](https://localhost:5173/account/setup). The development certificate is self-signed; accept the local certificate exception in your browser. Username is `admin`; choose your own password using the one-use code saved in `.build/dev/admin-setup-code`. Read that file locally and enter its contents in the setup form. No default password is created. After setup, sign in explicitly.

The database listens only on `127.0.0.1:54329`; the HTTPS frontend on `127.0.0.1:5173`. The frontend and seed client reach the API through the owner-only `.build/dev/api.sock` Unix socket, inside a private directory. Another local user cannot impersonate the API by occupying a TCP port. The frontend overwrites client-address metadata with the actual socket peer; the API trusts it only on its owner-protected Unix transport. Public authentication is limited per canonical client IP, so clients sharing an IP share a budget. Unsupported methods do not spend that budget. Direct owner socket requests use a separate operator budget, and TCP API requests ignore forwarding headers. Generated credentials and certificates stay in owner-readable files inside ignored `.build/dev/`. Never commit or share that directory. A crashed API may leave a stale socket; verify that its server has stopped before removing it.

`make dev-stop` stops the recorded development database and retains its data. Repeat `make dev-setup` to restart it; bootstrap never reopens after completion. Stop the two foreground servers with Ctrl-C.

### Development commands

Run `make dev` for one foreground session after installing tools/dependencies. It starts the local dependencies, rebuilds the API and browser contract when Go sources or embedded SQL contents change, restarts the API, and starts the frontend with its existing reload support. Logs share the terminal. Ctrl-C stops the API/frontend and any database or S3 service started by that session; services already running are retained. A failed initial build or occupied API/frontend port exits with an error. Schema changes still need explicit `make dev-setup`; serving never migrates implicitly. The separate `dev-api` and `dev-web` commands remain available.

`make doctor` checks the pinned tools, Docker, frontend dependencies, certificate/key, database schema, S3 buckets/versioning and local ports, and prints a repair command for each problem. It performs no setup or migrations. If a development process was forcibly killed, check the PID in `.build/dev/session.lock/pid` before removing that stale lock. Normal shutdown removes it automatically.

`make dev-seed` adds **Local demo → Sandbox → Development**, a **Local developers** team, and an environment Planner grant for the administrator running the command. Start the API and complete administrator password setup first. Save that chosen password in an owner-only file using a private editor, then run:

```sh
chmod 600 /absolute/path/to/local-password-file
SEED_PASSWORD_FILE=/absolute/path/to/local-password-file make dev-seed
```

The username defaults to `admin`; override it with `SEED_USERNAME`. Passwords are read from the protected file, never command arguments or logs. Seeding uses the authenticated local API, preserves existing records, rejects ambiguous sample names, and can be repeated without duplicates. It creates no default password, account, source connection or cloud resources. Its API session is logged out afterward.

### Local S3 and OpenTofu

`make dev-setup` also starts digest-pinned SeaweedFS **4.47** in Docker and builds a Linux amd64 runner using the existing checksum-pinned OpenTofu **1.13.8** and AWS provider **6.41.0**. Run `make dev-storage` to start only storage and the fixture. The first build downloads public dependencies; runs install providers only from the bundled filesystem mirror.

Authenticated S3 listens at `http://127.0.0.1:18333`; other SeaweedFS ports are not published. Generated local credentials and configuration stay in owner-readable `.build/local/dev/`. Configuration is copied into a separate owned Docker volume with private permissions for the image's SeaweedFS user, including on Linux hosts whose UID differs. Its `runner.env` contains credentials for other local S3 clients; do not source it into a shell used for real AWS work. Containers use `http://seaweed:8333`.

Harness-owned buckets separate versioned state (`cosmoformer-state`), development artifacts (`cosmoformer-artifacts`) and fixture resources (`cosmoformer-resources`). The [OpenTofu fixture](tests/local/main.tf) creates `example.txt` through the AWS provider pointed at SeaweedFS. No AWS account or Docker provider is needed.

```sh
make dev-tofu                              # plan
make dev-tofu TOFU_ARGS='plan -out=local.plan'
make dev-tofu TOFU_ARGS='apply local.plan'
make dev-tofu TOFU_ARGS='apply -var=content=changed'
make dev-tofu TOFU_ARGS='destroy'
```

Commands run in `.build/local/dev/work/`. Starting storage refreshes the generated `main.tf` from the source fixture. State, objects and filer metadata persist in a recorded Docker volume across `make dev-stop` and `make dev-storage`. Stop retains database and S3 data; `destroy` removes only fixture-managed resources. The API/frontend still use `make dev-api` and `make dev-web`. Each OpenTofu command has its own runner identity; a failed command can clean up only its own runner. Avoid concurrent development commands against the same work directory, since they still share configuration and plan files.

Storage setup resumes incomplete provisioning with the recorded identity and credentials and verifies resource ownership before reuse. It records start intent before SeaweedFS can write data. Once storage may have been used, a missing data volume requires explicit recovery; setup never replaces it with an empty volume. Older receipts without an initialization phase are treated conservatively: their recorded volume must still exist. Verify whether a legacy failed setup ever held data before manually removing its receipt or resources.

`make test-local` builds the same runner and creates a separate SeaweedFS container, internal network, volume, random loopback port and credentials per invocation. It checks interrupted setup recovery, missing initialized-volume protection, authentication, saved-plan apply, no-op planning, updates, state lineage/serial, restart persistence, external drift and repair, concurrent conditional lock writes, native lock contention, artifact round trips and destruction. It removes its recorded resources on success or failure, without development-storage fallback. After a hard process interruption, `.build/local/test-*/environment.json` identifies any resources needing manual cleanup. This suite is included in `make check` alongside the existing synthetic native tests.

The runner has a read-only root filesystem, only generated local credentials and fixture mounts, and no Docker socket or host credential mounts. Its internal Docker network has no external routing. These standalone engine/storage exercises do not enable native plans or artifact writes through the application. Custom endpoints stay in the local harness; production validation and AWS owner/IAM requirements are unchanged. Passing fixtures establish pinned local compatibility, not general AWS service parity.

The local runner image builds independently of application code and acceptance-test compilation. Its AWS binary, license and original mirror metadata are retained byte-for-byte so existing provider locks remain valid.

## Accounts and permissions

1. In **Local accounts**, create an account and issue its setup code. Share the code privately; it is shown once. Setup codes expire in 24 hours; reset codes in 30 minutes.
2. Create an organization and project, then add environments.
3. Invite the account to the organization. The owner must accept while signed in.
4. Add the accepted member to a team. Grant that team a role for a project or environment.

Project grants inherit to all environments; environment grants add access. Viewer reads ordinary infrastructure information, Planner adds plan requests, and Deployer adds apply/stop/recovery actions. Administrator manages configuration and access; it does not imply those execution permissions. Approval and raw evidence are separate grants. These permissions gate planning, saved-plan Apply and execution controls.

Disabling an account revokes its sessions immediately. Password setup/reset revokes existing sessions and proofs without changing grants. Sessions expire after one hour idle or twelve hours from sign-in. Only intentional activity renews idle time; background reads do not. Password reset is administrator-assisted; there is no email delivery or public signup.

## Plan analysis library

Milestone 5 adds `internal/plan` for strict native evidence validation, sanitized Changes and deterministic `warnings-v1` findings. It preserves known, unknown, hidden, null and absent values, renders decimal numbers without precision loss, protects native sensitivity and supplied known secrets, and keeps drift separate from planned actions. Raw plan JSON remains separate protected evidence; analysis errors return no raw fallback or partial all-clear result.

Selected AWS rules cover destructive actions, RDS protection/backup/public settings, security-group ingress, VPC routes and network ACL rules. Network findings describe potential settings changes, not proven reachability or business outages. Exact provider/schema profiles are checked offline with both pinned engines and AWS **6.41.0**. Other allowed external providers receive validated generic action/value rendering with explicit absent semantic coverage. Default route-table/default ACL variants receive no related-type semantic fallback.

`internal/testdata/plans.json` contains 126 synthetic cases. `internal/testdata/compatibility.json` records native schema fingerprints and extracted resource shapes; synthetic plans alone do not establish compatibility. See the [analysis limits and integration boundary](docs/operator-guide.md#plan-analysis-library-and-coverage) before using the library. Native planning and readiness are connected through explicit requests; exact-plan approvals and formal recovery are available, and saved-plan Apply runs the exact retained binary after fresh preparation and current eligibility checks. Company compatibility remains `not_assessed`.

## Plan-review workspace

Open a project from Organizations, select an environment, and open a stack or an exact run. URLs retain organization/project/environment/stack context across refresh and deep links. Stack pages show current source; run pages preserve their own accepted source and commit/PR, purpose, plan/revision/manifest identity, profile and timestamps.

Check source and Request plan are separate explicit actions. Accepted requests open their exact returned run. Ready plans are labelled **Ready for review**. Changes shows consequential findings, action counts, risk-ordered searchable resources, replacement order, lossless values, outputs and separate drift. Hidden/unknown values and partial/generic/unsupported coverage stay visible as limitations; absent warnings never prove safety. Retained finalized Changes can be reviewed after expiry/invalidation without restoring execution eligibility. Missing/corrupt exact evidence stays unavailable.

Logs support follow/pause, bounded archive cursor continuation, preview resets and known/unknown gaps. The browser retains at most 512 Ki characters and 500 displayed segments; that display limit is separate from durable evidence loss. Polling does not renew idle sessions. Scope changes, logout/expiry and denied reads clear protected data, and sign-in never replays a planning mutation. Raw downloads remain explicit and separately authorized through the existing API. The workspace supports keyboard navigation, responsive layouts and light/dark themes.

## Checks

`make test-integration-local` starts a fresh PostgreSQL container, runs the host integration suite, then removes the container and its protected control files. Each invocation uses its own `.build/test-runs/run-*` directory and random port, independently of the development DB and manually managed test DB. `TEST_ARGS` can select tests, for example `make test-integration-local TEST_ARGS="-run 'TestLocalDevelopmentSeed|TestBootstrap' -count=1"`. Cleanup also runs after test failures or Ctrl-C. Creation identity is saved before Docker starts, so cleanup can find an owned container even if its creation reply is lost. If Docker cannot confirm the outcome, the receipt is retained for recovery; run `COSMOFORMER_TEST_DIR=/absolute/path/to/run-directory node scripts/dev.mjs test-stop` after Docker settles. A forced kill may also require this cleanup; do not remove other runs' resources. The command overrides inherited test DSNs with its own marker-checked database. The seed acceptance test exercises the actual Node client against the Go API and PostgreSQL twice and verifies the resulting grants and logout.

Tests use a separate throwaway PostgreSQL container, marker-checked control database, generated per-test databases, and a non-owner runtime role. They never fall back to the development database.

```sh
make test-db-start
cd web && npx --no-install playwright install chromium firefox && cd ..
make check
make test-db-stop
```

The full check now also builds a checksum-pinned Linux source-worker test image and runs it with external networking disabled. These tests exercise actual Git over a loopback HTTPS fixture and verify the baked OpenTofu/Git identities. PostgreSQL/API tests use synthetic Bitbucket responses, including OAuth, signed webhook receipts, source reconciliation, worker claims and response-loss replay. No real Bitbucket credentials, company repositories or AWS services are used. The separate native harness exercises pinned engines and providers against local synthetic backend fixtures; platform infrastructure execution remains future work.

`make test-native` builds and runs both OpenTofu **1.13.8** and Terraform **1.17.1** on Linux amd64. The Linux builder also checks the Make recipe under GNU Make 4.x before runtime testing. Build-time fixture supply verifies the pinned null **3.2.4** and AWS **6.41.0** archives and preserves their licenses. Runtime containers use a read-only filesystem, executable temporary storage, no external network and no host mounts or inherited credentials. Git HTTPS, S3 and DynamoDB fixtures run on loopback inside each container. Provider installation uses only the bundled filesystem mirror. No PostgreSQL or platform server is involved.

Native checks cover exact Git commits, exact AWS analysis profiles and all 126 warning/normalization fixtures, actual native JSON analysis, AWS/null schemas, output-only roots without provider locks, retained null-provider lock bytes, fresh-checkout saved-plan replay, state lineage/serial rejection, and S3/DynamoDB/dual lock contention. State keys are compared with the production preflight resolver for default workspaces, the native `env:` prefix and an explicit prefix containing path segments. These checks are part of `make check`; ordinary host integration explicitly skips the native scenarios.

**Reconciled native contracts:** declare `backend_owner_account_id: "111122223333"` as required stack metadata in `cosmoformer.yaml`; omit the unsupported `expected_bucket_owner` setting from native backend files. Both the worker and coordinator derive expected-owner intent from the assignment, and source digests retain it. The fixed owner session-policy generator and loopback owner-change tests preserve the access constraint; the production AWS adapter must attach that policy and perform SDK owner checks before enabling native execution. Source preflight does not verify live AWS ownership.

Key validation uses the selected pinned runtime: both reject leading/trailing slashes in raw keys and prefixes; Terraform also rejects repeated slashes in raw keys. OpenTofu accepts those keys. Both accept repeated internal prefix slashes and use native joining for non-default workspaces. Invalid keys are rejected before joining, never cleaned into another destination. Update existing fixture declarations explicitly and run a fresh source check; no state, permanent claim or queued assignment is automatically rewritten. Company compatibility remains unassessed.

Individual targets: `test-unit`, `test-integration`, `test-source`, `test-scripts`, `test-native`, `test-local`, `test-web`. `test-source` includes worker-command startup and clock tests inside the baked Linux image; CI runs these same suites through `test-go-coverage`. `test-scripts` verifies coverage merging and thresholds, actual contract generation and the real Make native-test recipe with stubbed Docker commands; it is included in `make check` and emits helper-script LCOV. `test-web` runs TypeScript checks, frontend unit/component tests with coverage, the production build, and Playwright journeys in Chromium and Firefox. The browser suite serves the compiled SPA over local HTTPS, starts its own account server, and exercises sign-in, setup, invitations, grant/revocation and empty-access flows. Source settings, secret-write errors, source-check controls and worker credential visibility also have component tests against the actual generated API validators. Safari is not covered by this automated suite.

Frontend tests use Vitest, React Testing Library and jsdom. They exercise the actual API client and Go-generated response validators with controlled HTTP responses, real router/theme contexts, and session timers. They cover error handling, CSRF headers, expiry and stale responses, setup/reset/sign-in forms, scope changes, account administration and team/grant controls. These tests do not replace browser checks or the server's permission tests.

```sh
make test-web-unit       # no database or HTTPS certificate required
make test-web-coverage   # frontend coverage only
cd web && npm run test:unit:watch
```

With the isolated test database running, `make test-coverage` generates Go, frontend, contract and helper-script coverage. `make test-go-coverage` measures Go unit and acceptance tests together, including coverage from the real command processes. It also runs the pinned Linux worker image with networking disabled, collecting source, worker, clock and worker-command coverage. Each run uses fresh temporary directories. Host and Linux profiles are merged by source block, retaining a block as covered if either run exercised it; shared code is counted once. Docker is required for this target. Command tests cover argument/exit behavior, protected proof files, migration and role checks, bootstrap/recovery, API startup and graceful shutdown. Unit command checks need no database; command integration checks use only the marked throwaway database. Reports are ignored local outputs:

- `.build/coverage/go.out`: Go coverage for SonarQube.
- `.build/coverage/go.html`: browsable Go report.
- `.build/coverage/scripts/lcov.info`: coverage of the profile merger and Go coverage check, collected by Node's built-in test runner.
- `.build/coverage/contract/lcov.info`: coverage of actual Go contract generation and browser-validator emission, including strict response and safe integer checks.
- `.build/coverage/workflows/lcov.info`: helper-script unit and process-supervision coverage.
- `.build/coverage/runtime/lcov.info`: coverage from the real local-storage CLI and seed-client acceptance suites; collected by `test-runtime-coverage`, also included in `make check`.
- `.build/coverage/web/lcov.info`: frontend coverage with repository-relative source paths for SonarQube.
- `.build/coverage/web/index.html`: browsable frontend report.

`make test-go-coverage` fails unless total Go statement coverage is strictly greater than 80%; exactly 80% fails. This uses the merged host/Linux counts rather than averaging package percentages. `make check`, which runs in CI, includes this coverage check and Go vet in place of repeating the same Go suites separately.

CI also runs `make test-overall-coverage` against a fresh, pinned SonarQube service after all coverage suites. Both **overall combined coverage and overall line coverage must be strictly greater than 80%**; exactly 80% fails. The gate uses integer covered/total counts, not rounded percentages or package averages, and does not depend on a new-code baseline or the default Sonar quality gate. Missing, empty or stale reports, changed source/HEAD, incomplete analysis, missing Go/JavaScript/TypeScript analysis, and mismatched analysis IDs fail the job. No application-source coverage exclusions were added.

`make check` and `make test-coverage` begin a collection receipt and generate the five required reports. `make check` also retains the native, browser and other functional checks. The CI service is disposable, loopback-published and uses an ephemeral evaluation database; it is not part of the product deployment. The scanner shares only that service's network namespace and reads a prepared source snapshot. Its project token is held in an owner-only temporary file and removed after the scan. For local verification, use a fresh container of the pinned Sonar image on a dedicated Docker network, publish its port 9000 only on loopback, then run:

```sh
COSMOFORMER_SONAR_CONTAINER=<fresh-container-id> \
COSMOFORMER_SONAR_ORIGIN=http://127.0.0.1:<published-port> \
make test-overall-coverage
```

A previously initialized Sonar service is not reusable for this target: the gate requires factory bootstrap and creation of a new project. Stop and remove only the disposable container/network you created afterward. Source files and HEAD must remain unchanged from collection through analysis. The exact task/analysis IDs, source digest, integer counts and pass/fail result are retained in `.build/coverage/overall.json`; scanner diagnostics are in `.build/coverage/sonar/scanner.log`.

Coverage includes all application TypeScript/React files, including unvisited files; generated API code is excluded. The local setup, tool installer, readiness checks, development-session supervisor and brand exporter are also tested with controlled files, downloads and child commands. Their coverage is included in the same LCOV. Frontend coverage checks still require 90% statements, lines and functions, and 85% branches independently of those helpers. Playwright remains a separate functional browser check; its coverage is not merged into the unit report.

`make contract` exports the Go-owned API and generates browser types/response validators. `.build/openapi.json`, `web/app/.generated`, tool caches and build outputs are ignored. Design documents stay outside this repository.

## Operator commands for local development

Serving never migrates implicitly. The runtime uses `COSMOFORMER_DATABASE_DSN_FILE`; migration/bootstrap/recovery commands use the separate `COSMOFORMER_MIGRATION_DSN_FILE`. Input files must be regular, owner-readable only. The development setup provisions distinct owner and restricted runtime database roles.

```sh
# List the ten embedded forward-only migrations without a database connection:
.build/tools/go/bin/go run ./cmd/cosmoformer migrate --list

# Recover an existing enabled installation administrator on this local DB:
COSMOFORMER_MIGRATION_DSN_FILE="$PWD/.build/dev/owner.dsn" \
  .build/tools/go/bin/go run ./cmd/cosmoformer recover-admin \
  --username admin --setup-code-file "$PWD/.build/dev/recovery-code"
```

Recovery writes a new one-use reset code to a new protected file, revokes sessions and does not change permissions. Use `/account/reset`, then sign in. There is no backward migration command; lower schema versions require a separately planned restore procedure.

## Source preflight

In **Sources & workers**, organization administrators register source connections, repositories, resource permissions, execution-role references and paused workers. Installation administrators approve exact worker runtime identities. Team members with Planner permission can queue **Check source** from **Source checks**. Configuration administration does not grant plan authority.

The platform accepts tracked inventory only after authoritative source reads and strict `cosmoformer.yaml` validation. A source-only worker checks the exact commit, contained module closure, literal backend configuration and administrator-granted destinations. Successful preflight permanently binds the stack's state and lock/checksum namespaces. A collision is rejected across the installation without exposing another organization's details. The result is **Source check passed**, not a native infrastructure plan.

Source work uses the production worker admission/claim protocol, generation fencing, bounded elapsed budgets and ordered idempotent messages. Unknown process ownership is retained rather than released on a timeout or missing response. There is no automatic retry of uncertain assignments. Known, positively stopped preparation failures may be explicitly retried. The fixed source job queue handles signed webhook reconciliation, five-minute source sweeps and stable PR feedback with bounded retries; it cannot apply infrastructure.

Workers and production source coordination require Linux `CLOCK_BOOTTIME`; macOS remains supported for account/frontend development and the local injected-clock protocol tests. `make test-source` supplies the isolated Linux verification environment. The reference worker image and runtime download identities are recorded in [runtimes.json](deploy/worker/runtimes.json). A local image can be built with `make worker-image`; nothing is published or deployed.

Managed secrets and source/OAuth credentials require a separately protected 32-byte encryption key. See the [implemented operator procedures](docs/operator-guide.md) for initial key registration, supported source inputs, worker credentials and local verification. The fifth migration adds the helpers omitted from the initial foundation, plus the durable phase-2 protocol records; it upgrades version 4 forward without rewriting the original migrations.

Actual company repository, module, backend, runtime and IAM compatibility remains unassessed. The synthetic reference checks do not enable company infrastructure execution.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md). Licensed under [MIT](LICENSE).

## Brand assets

The approved Spiral Ribbon logo is available as vector artwork, transparent PNGs, and browser/app icons. See [the brand bundle](assets/brand/README.md) for formats, colors, and regeneration with `make brand`.

## Pull request deployment workflow

Project workspaces include **Pull requests**, scoped to the viewer’s current project and environment grants before grouping or pagination. PR heads retain their separate stack runs, planning/deployment outcomes, queue reasons, recovery attention and exact Changes links. Historical source remains explicit; navigation never retargets a run.

Verified same-repository open PRs targeting the tracked branch can automatically plan compatible authorized stacks. Explicit source checks remain source-only. PR Apply still needs current per-stack policy, exact-plan approval and a separate authenticated confirmation. After merge, the actual tracked commit gets its own normal plan: **No changes** finishes truthfully; remaining changes need their own approvals and explicit Apply. PR approvals and binaries never transfer.

One durable, bounded PR comment links to protected project/run/Changes pages and reflects current authoritative outcomes. Feedback delivery has independent retries and visible failures; it cannot execute infrastructure, grant approval or replay Apply after sign-in or reconnect. See [the operator workflow](docs/operator-guide.md#pull-request-planning-and-deployment-milestone-11) for selection rules, migration 014 and local verification boundaries.

The project Modules view provides a scoped, commit-pinned catalog with plain usage instructions and example links. Catalog administration does not grant private-source credentials or execution rights. Git-owned stack dependencies are visible links for manual coordination, with no automatic ordering or execution. See the [module catalog guide](docs/operator-guide.md#module-catalog-and-manual-dependencies).
