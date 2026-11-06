# Cosmoformer

Cosmoformer is an early-stage, open-source infrastructure automation project. The first local development slice implements accounts and access control with a Go backend, PostgreSQL, and a React Router SPA using Astryx.

You can create local accounts, issue one-use setup/reset codes, sign in, manage organizations and teams, and assign project/environment access. New accounts have no access. Administration, deployment, approval and raw-evidence permissions are separate. Terraform/OpenTofu execution, Git integrations and infrastructure changes are not implemented yet. There is no production release.

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

Individual targets: `test-unit`, `test-integration`, `test-web`. `test-web` runs TypeScript checks, frontend unit/component tests with coverage, the production build, and Playwright journeys in Chromium and Firefox. The browser suite serves the compiled SPA over local HTTPS, starts its own account server, and exercises sign-in, setup, invitations, grant/revocation and empty-access flows. No cloud credentials or infrastructure are required. Safari is not covered by this automated suite.

Frontend tests use Vitest, React Testing Library and jsdom. They exercise the actual API client and Go-generated response validators with controlled HTTP responses, real router/theme contexts, and session timers. They cover error handling, CSRF headers, expiry and stale responses, setup/reset/sign-in forms, scope changes, account administration and team/grant controls. These tests do not replace browser checks or the server's permission tests.

```sh
make test-web-unit       # no database or HTTPS certificate required
make test-web-coverage   # frontend coverage only
cd web && npm run test:unit:watch
```

With the isolated test database running, `make test-coverage` generates both Go and frontend coverage. `make test-go-coverage` measures Go unit and acceptance tests together, including coverage from the real command processes. Each run uses a fresh temporary coverage directory and Go’s coverage tools to combine the results. Command tests cover argument/exit behavior, protected proof files, migration and role checks, bootstrap/recovery, API startup and graceful shutdown. Unit command checks need no database; command integration checks use only the marked throwaway database. Reports are ignored local outputs:

- `.build/coverage/go.out`: Go coverage for SonarQube.
- `.build/coverage/go.html`: browsable Go report.
- `.build/coverage/web/lcov.info`: frontend coverage with repository-relative source paths for SonarQube.
- `.build/coverage/web/index.html`: browsable frontend report.

Coverage includes all application TypeScript/React files, including unvisited files; generated API code is excluded. Frontend coverage checks require 90% statements, lines and functions, and 85% branches. Playwright remains a separate functional browser check; its coverage is not merged into the unit report.

`make contract` exports the Go-owned API and generates browser types/response validators. `.build/openapi.json`, `web/app/.generated`, tool caches and build outputs are ignored. Design documents stay outside this repository.

## Operator commands for local development

Serving never migrates implicitly. The runtime uses `COSMOFORMER_DATABASE_DSN_FILE`; migration/bootstrap/recovery commands use the separate `COSMOFORMER_MIGRATION_DSN_FILE`. Input files must be regular, owner-readable only. The development setup provisions distinct owner and restricted runtime database roles.

```sh
# List the four embedded forward-only migrations without a database connection:
.build/tools/go/bin/go run ./cmd/cosmoformer migrate --list

# Recover an existing enabled installation administrator on this local DB:
COSMOFORMER_MIGRATION_DSN_FILE="$PWD/.build/dev/owner.dsn" \
  .build/tools/go/bin/go run ./cmd/cosmoformer recover-admin \
  --username admin --setup-code-file "$PWD/.build/dev/recovery-code"
```

Recovery writes a new one-use reset code to a new protected file, revokes sessions and does not change permissions. Use `/account/reset`, then sign in. There is no backward migration command; lower schema versions require a separately planned restore procedure.

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md). Licensed under [MIT](LICENSE).

## Brand assets

The approved Spiral Ribbon logo is available as vector artwork, transparent PNGs, and browser/app icons. See [the brand bundle](assets/brand/README.md) for formats, colors, and regeneration with `make brand`.
