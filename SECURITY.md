# Security policy

## Supported versions

The latest release receives security fixes. Older tags do not.

## Reporting a vulnerability

Report a vulnerability through
[GitHub private vulnerability reporting](https://github.com/andersonten/simcitadel/security/advisories/new).
Do not open a public issue for a security problem.

Include the version, the platform, and the steps that reproduce the problem.
You can expect a first answer within seven days.

## Scope

Citadel runs in a development environment and it holds credentials for that
environment alone. Two areas matter most:

- **Credential handling.** The CLI reads every target from `citadel.yaml` and
  resolves `${VAR}` references at load time. A diagnostic bundle redacts a
  credential before it writes a file. A leak in either path is a vulnerability.
- **The sidecar images.** `event-trigger` and `event-capture` accept HTTP
  requests inside the environment. Neither authenticates a caller, because both
  are private to one developer environment. Do not expose either to a network
  you do not trust. A path that lets a request reach outside the configured
  broker or buffer is a vulnerability.

Never commit a real credential to a scenario, a payload, or a configuration
file. Use a placeholder and an environment variable reference.
