# Cosmoformer

Cosmoformer is an early-stage, open-source infrastructure automation project. The local reference implementation provides accounts, access control and source preflight with a Go backend, PostgreSQL, and a React Router SPA using Astryx.

You can create local accounts, sign in, manage organizations and teams, and assign project/environment access. New accounts have no access. Administration, deployment, approval and raw-evidence permissions are separate. Source preflight adds bounded Bitbucket source reads, strict Git configuration, registered Linux workers and permanent state-destination ownership checks. It does not initialize a backend, read existing state, assume AWS roles, plan or apply infrastructure. There is no production release.

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

The database listens only on `127.0.0.1:54329`; the API on `127.0.0.1:18080`; the HTTPS frontend on `127.0.0.1:5173`. The frontend forwards same-origin API requests. Generated credentials and certificates stay in owner-readable files inside ignored `.build/dev/`. Never commit or share that directory.

`make dev-stop` stops the recorded development database and retains its data. Repeat `make dev-setup` to restart it; bootstrap never reopens after completion. Stop the two foreground servers with Ctrl-C.

## Accounts and permissions

1. In **Local accounts**, create an account and issue its setup code. Share the code privately; it is shown once. Setup codes expire in 24 hours; reset codes in 30 minutes.
2. Create an organization and project, then add environments.
3. Invite the account to the organization. The owner must accept while signed in.
4. Add the accepted member to a team. Grant that team a role for a project or environment.

Project grants inherit to all environments; environment grants add access. Viewer reads ordinary infrastructure information, Planner adds plan requests, and Deployer adds apply/stop/recovery actions. Administrator manages configuration and access; it does not imply those execution permissions. Approval and raw evidence are separate grants. These permissions are enforced now; execution endpoints will be added with their implementation.

Disabling an account revokes its sessions immediately. Password setup/reset revokes existing sessions and proofs without changing grants. Sessions expire after one hour idle or twelve hours from sign-in. Only intentional activity renews idle time; background reads do not. Password reset is administrator-assisted; there is no email delivery or public signup.

## Checks

Tests use a separate throwaway PostgreSQL container, marker-checked control database, generated per-test databases, and a non-owner runtime role. They never fall back to the development database.

```sh
make test-db-start
cd web && npx --no-install playwright install chromium firefox && cd ..
make check
make test-db-stop
```

The full check now also builds a checksum-pinned Linux source-worker test image and runs it with external networking disabled. These tests exercise actual Git over a loopback HTTPS fixture and verify the baked OpenTofu/Git identities. PostgreSQL/API tests use synthetic Bitbucket responses, including OAuth, signed webhook receipts, source reconciliation, worker claims and response-loss replay. No real Bitbucket credentials, company repositories or AWS services are used. The separate native harness exercises pinned engines and providers against local synthetic backend fixtures; platform infrastructure execution remains future work.

`make test-native` builds and runs both OpenTofu **1.13.8** and Terraform **1.17.1** on Linux amd64. The Linux builder also checks the Make recipe under GNU Make 4.x before runtime testing. Build-time fixture supply verifies the pinned null **3.2.4** and AWS **6.41.0** archives and preserves their licenses. Runtime containers use a read-only filesystem, executable temporary storage, no external network and no host mounts or inherited credentials. Git HTTPS, S3 and DynamoDB fixtures run on loopback inside each container. Provider installation uses only the bundled filesystem mirror. No PostgreSQL or platform server is involved.

Native checks cover exact Git commits, AWS/null schemas, output-only roots without provider locks, retained null-provider lock bytes, fresh-checkout saved-plan replay, state lineage/serial rejection, and S3/DynamoDB/dual lock contention. State keys are compared with the production preflight resolver for default workspaces, the native `env:` prefix and an explicit prefix containing path segments. These checks are part of `make check`; ordinary host integration explicitly skips the native scenarios.

**Reconciled native contracts:** declare `backend_owner_account_id: "111122223333"` as required stack metadata in `cosmoformer.yaml`; omit the unsupported `expected_bucket_owner` setting from native backend files. Both the worker and coordinator derive expected-owner intent from the assignment, and source digests retain it. The fixed owner session-policy generator and loopback owner-change tests preserve the access constraint; the production AWS adapter must attach that policy and perform SDK owner checks before enabling native execution. Source preflight does not verify live AWS ownership.

Key validation uses the selected pinned runtime: both reject leading/trailing slashes in raw keys and prefixes; Terraform also rejects repeated slashes in raw keys. OpenTofu accepts those keys. Both accept repeated internal prefix slashes and use native joining for non-default workspaces. Invalid keys are rejected before joining, never cleaned into another destination. Update existing fixture declarations explicitly and run a fresh source check; no state, permanent claim or queued assignment is automatically rewritten. Company compatibility remains unassessed.

Individual targets: `test-unit`, `test-integration`, `test-source`, `test-scripts`, `test-native`, `test-web`. `test-source` includes worker-command startup and clock tests inside the baked Linux image, so these checks cannot silently skip in normal CI. `test-scripts` verifies profile merging, actual contract generation and the real Make native-test recipe with stubbed Docker commands; both are included in `make check`. `test-web` runs TypeScript checks, frontend unit/component tests with coverage, the production build, and Playwright journeys in Chromium and Firefox. The browser suite serves the compiled SPA over local HTTPS, starts its own account server, and exercises sign-in, setup, invitations, grant/revocation and empty-access flows. Source settings, secret-write errors, source-check controls and worker credential visibility also have component tests against the actual generated API validators. Safari is not covered by this automated suite.

Frontend tests use Vitest, React Testing Library and jsdom. They exercise the actual API client and Go-generated response validators with controlled HTTP responses, real router/theme contexts, and session timers. They cover error handling, CSRF headers, expiry and stale responses, setup/reset/sign-in forms, scope changes, account administration and team/grant controls. These tests do not replace browser checks or the server's permission tests.

```sh
make test-web-unit       # no database or HTTPS certificate required
make test-web-coverage   # frontend coverage only
cd web && npm run test:unit:watch
```

With the isolated test database running, `make test-coverage` generates both Go and frontend coverage. `make test-go-coverage` measures Go unit and acceptance tests together, including coverage from the real command processes. It also runs the pinned Linux worker image with networking disabled, collecting source, worker, clock and worker-command coverage. Each run uses fresh temporary directories. Host and Linux profiles are merged by source block, retaining a block as covered if either run exercised it; shared code is counted once. Docker is required for this target. Command tests cover argument/exit behavior, protected proof files, migration and role checks, bootstrap/recovery, API startup and graceful shutdown. Unit command checks need no database; command integration checks use only the marked throwaway database. Reports are ignored local outputs:

- `.build/coverage/go.out`: Go coverage for SonarQube.
- `.build/coverage/go.html`: browsable Go report.
- `.build/coverage/scripts/lcov.info`: coverage of the profile merger, collected by Node's built-in test runner.
- `.build/coverage/contract/lcov.info`: coverage of actual Go contract generation and browser-validator emission, including strict response and safe integer checks.
- `.build/coverage/web/lcov.info`: frontend coverage with repository-relative source paths for SonarQube.
- `.build/coverage/web/index.html`: browsable frontend report.

Coverage includes all application TypeScript/React files, including unvisited files; generated API code is excluded. The local setup and tool installer are also tested with mocked files, downloads and child commands. Their coverage is included in the same LCOV. Frontend coverage checks still require 90% statements, lines and functions, and 85% branches independently of those helpers. Playwright remains a separate functional browser check; its coverage is not merged into the unit report.

`make contract` exports the Go-owned API and generates browser types/response validators. `.build/openapi.json`, `web/app/.generated`, tool caches and build outputs are ignored. Design documents stay outside this repository.

## Operator commands for local development

Serving never migrates implicitly. The runtime uses `COSMOFORMER_DATABASE_DSN_FILE`; migration/bootstrap/recovery commands use the separate `COSMOFORMER_MIGRATION_DSN_FILE`. Input files must be regular, owner-readable only. The development setup provisions distinct owner and restricted runtime database roles.

```sh
# List the five embedded forward-only migrations without a database connection:
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
