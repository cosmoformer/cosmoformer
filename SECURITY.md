# Security Policy

## Project Status

Cosmoformer has a local development implementation of accounts and access control. There is no production release or release support matrix. The development servers, self-signed HTTPS certificate and local database setup are intended for a trusted laptop, not deployment.

## Reporting a Vulnerability

Report suspected security vulnerabilities privately using [GitHub private vulnerability reporting](https://github.com/cosmoformer/cosmoformer/security/advisories/new).

Sign in to GitHub to open a private report. Do not report vulnerabilities through public issues, pull requests, or discussions.

Include, where applicable:

- A description of the vulnerability and affected components.
- Steps to reproduce the problem or a minimal proof of concept.
- The potential security impact.
- Any suggested mitigation.

Share only the information necessary to investigate the report. Do not include real credentials, private keys, or unrelated sensitive information.

Keep vulnerability details private while maintainers investigate and coordinate disclosure.

## Local credential handling

Do not share `.build/dev/`, generated setup/reset codes, browser traces or database files. The local runtime uses a restricted non-owner PostgreSQL role; migration and host recovery use a separate owner credential. Passwords use Argon2id. Sessions use Secure/HttpOnly cookies, bounded expiry, same-origin checks and CSRF verification. Test databases are disposable and contain synthetic data. Production deployment, external authentication, operational recovery and security hardening beyond this account slice remain future work.
