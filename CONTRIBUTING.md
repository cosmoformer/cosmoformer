# Contributing to Cosmoformer

Cosmoformer is an early-stage open-source infrastructure automation project. Local accounts, access control, source-only preflight and gated explicit native planning are implemented for reference development; no production release is available. The current implementation uses Go, PostgreSQL and React Router. See README.md for local setup and checks.

## Bugs and Feature Proposals

Use the [issue tracker](https://github.com/cosmoformer/cosmoformer/issues) to report reproducible problems in existing functionality or propose improvements.

Use the relevant issue template and provide enough context to understand the problem and desired outcome. A feature request is a proposal, not an approved implementation commitment.

Discuss substantial changes before starting implementation, especially changes that introduce a technology stack, dependencies, integrations, or architectural decisions.

## Contributing Changes

Keep changes focused on the agreed scope. Explain their purpose, reference related issues where applicable, and update relevant documentation.

Use the pull request template to describe the changes and report the validation performed and its results. If validation was not possible, explain why.

Do not include secrets, credentials, private keys, or other sensitive information. Remove sensitive details from logs, examples, and screenshots.

## Security Reports

Report suspected security vulnerabilities privately as described in [SECURITY.md](SECURITY.md). Do not report vulnerabilities in public issues or pull requests.

## License

See [LICENSE](LICENSE) for the project's MIT License.

## Development checks

`make test-local` exercises real OpenTofu against disposable SeaweedFS S3 storage and is included in `make check`. It uses separate containers, volumes, networks and credentials from `make dev-storage`. See the README for local fixture commands and the boundary between standalone engine tests and application execution.

Run `make test-db-start`, then `make check`; stop the throwaway database with `make test-db-stop`. Install Playwright Chromium/Firefox as described in the README first. Database tests require a marked test control database and refuse application-database fallback. Keep generated API specifications, frontend validators, credentials and design documents out of commits.

GitHub runs backend tests, four acceptance shards, Linux coverage, each native engine, runtime checks, frontend/browser checks and script checks on independent runners. Each SQL consumer has its own marked PostgreSQL service. `local-reference` remains the final required check: all jobs must succeed, every discovered acceptance test must appear in exactly one completed shard, and coverage bundles must match the same commit, source digest and workflow run, with valid attempt provenance. Successful bundles from an earlier attempt of the same run remain usable when only failed jobs are retried. Coverage is unioned before enforcing the existing Go and overall thresholds strictly above 80%; missing or changed bundles fail the gate. Only public Go/npm dependencies and Go build outputs are cached, never test results or coverage. New pushes cancel superseded runs.

Local `make check` retains the complete sequential workflow. `make test-native NATIVE_ENGINE=opentofu` or `NATIVE_ENGINE=terraform` selects one engine for targeted verification; omitting the selector still runs both. All runtime pins are validated before either selection runs. CI discovers acceptance tests from the compiled Go package, so adding a test does not require editing a shard list. Shards are deterministic and balanced by top-level test count; use per-job timings to rebalance if fixture costs diverge.

Phase-2 checks include the isolated Linux `test-source` image, with external networking disabled during test execution. Source/API fixtures use invented repositories, accounts, state destinations and credentials. Do not replace them with company data or enable test endpoint overrides in production configuration. Migrations are forward-only; do not rewrite migrations 001–010. Explicit planning intent and ready-plan immutability use migration 010; exact-plan approval integrity and input invalidation use forward migration 011. New source routes must retain current authorization, generation/ticket fencing, exact replay identities and explicit proof before releasing an assignment. No test/check publishes an image, deploys a service or accesses AWS.

The initial phase-3 `test-native` harness is also required by `make check`. It runs both pinned Linux engines, offline providers and loopback Git/S3/DynamoDB fixtures inside read-only containers with external networking disabled. Build-time downloads are checksum-verified public packages. Native tests never consume PostgreSQL DSNs or host credentials. See the README for the backend-owner prerequisites; synthetic success must not be described as production or company compatibility.

Plan analysis checks use `go test -race ./internal/plan ./internal/evidence ./internal/worker` and `make test-native`. Keep the data-driven synthetic plans and compatibility provenance in `internal/testdata`; specifications remain outside the repository. Schema profiles must be derived from actual pinned, offline native output, never from invented hashes or synthetic-only fixtures. Rerun both engine profiles when changing schema interpretation, sensitivity, normalization or rules. Keep direct artifact downloads under raw permissions. Only independently validated ready-plan Changes may use ordinary scoped reads. Regenerate the Go-owned contract and run affected frontend checks when wire DTOs change. Platform/worker PostgreSQL journeys inject native/AWS I/O; actual pinned engines/providers stay in the separate network-isolated harness without database access or an alternate executor. Company assessment remains `not_assessed`; never add a compatibility bypass.
