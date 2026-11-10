# Security Policy

## Project Status

Cosmoformer has a local reference implementation of accounts, access control, source-only preflight and explicitly requested native planning. There is no production release or release support matrix. The development servers, self-signed HTTPS certificate and local database setup are intended for a trusted laptop, not deployment.

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

Do not share `.build/dev/`, generated setup/reset codes, worker credentials, encryption keys, browser traces or database files. The local runtime uses a restricted non-owner PostgreSQL role; migration and host recovery use a separate owner credential. Passwords use Argon2id. Sessions use Secure/HttpOnly cookies, bounded expiry, same-origin checks and CSRF verification. Test databases are disposable and contain synthetic data.

Managed secrets and source/OAuth credentials use authenticated encryption with a separately protected deployment key and durable usage accounting. Browser credential writes have once-visible, metadata-only receipts. Bitbucket OAuth state is local-session-bound; it does not authenticate local accounts. Webhook signatures authenticate a receipt, not execution authority. Workers receive exact repository capabilities rather than broad platform OAuth credentials, database access or the encryption key.

Source workers use fresh private directories, fixed Git arguments, generation-bound assignments and positive process-stop proof. Permanent backend namespace ownership is independent of temporary worker reservations. Expired contact, a pause or a lost acknowledgement cannot release uncertain ownership. The worker image and host are trusted; these controls do not isolate hostile tenants or prove safety against a compromised host, arbitrary executable repository code, detached processes or transformed secret disclosure. Native planning uses fixed commands, one guarded AWS session, a fresh empty-derived child environment and exact worker/backend reservations. Readiness requires retained evidence and independent managed-secret validation. Check source and reconciliation remain source-only; unassessed company stacks cannot execute. Approvals and Apply remain unavailable. Production deployment, local external-identity sign-in and full operational recovery remain future work.
