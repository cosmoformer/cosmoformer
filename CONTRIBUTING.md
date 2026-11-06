# Contributing to Cosmoformer

Cosmoformer is an early-stage open-source infrastructure automation project. Local accounts and access control are implemented for development; no production release is available. The current implementation uses Go, PostgreSQL and React Router. See README.md for local setup and checks.

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

Run `make test-db-start`, then `make check`; stop the throwaway database with `make test-db-stop`. Install Playwright Chromium/Firefox as described in the README first. Database tests require a marked test control database and refuse application-database fallback. Keep generated API specifications, frontend validators, credentials and design documents out of commits.
