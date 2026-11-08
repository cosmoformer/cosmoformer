# Contributing to Cosmoformer

Cosmoformer is an early-stage open-source infrastructure automation project. Local accounts, access control and source-only preflight are implemented for reference development; no production release is available. The current implementation uses Go, PostgreSQL and React Router. See README.md for local setup and checks.

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

Phase-2 checks include the isolated Linux `test-source` image, with external networking disabled during test execution. Source/API fixtures use invented repositories, accounts, state destinations and credentials. Do not replace them with company data or enable test endpoint overrides in production configuration. The version-5 migration is forward-only; do not rewrite the four already delivered migrations. New source routes must retain current authorization, generation/ticket fencing, exact replay identities and explicit proof before releasing an assignment. No test/check publishes an image, deploys a service or accesses AWS.

The initial phase-3 `test-native` harness is also required by `make check`. It runs both pinned Linux engines, offline providers and loopback Git/S3/DynamoDB fixtures inside read-only containers with external networking disabled. Build-time downloads are checksum-verified public packages. Native tests never consume PostgreSQL DSNs or host credentials. See the README for the known backend-owner contract mismatch; synthetic success must not be described as production or company compatibility.
