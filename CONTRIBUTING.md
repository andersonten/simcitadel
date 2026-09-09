# Contributing

Thank you for your interest in Citadel. This file holds the conventions the
codebase follows. `docs/sdd.md` is the design source of truth, and the code
refers to it by section number.

## Before you start

Open an issue for a behaviour change, a new step type, or a new dependency.
A typo fix, a documentation fix, or a test that holds existing behaviour needs
no issue.

## Build and verify

```bash
make verify              # the full gate: fmt, tidy, vet, build, unit tests
go test -race ./...      # run for any change to concurrency or AMQP code
make test-integration    # container-backed tests, needs a Docker daemon
make stack-verify        # every compose service reaches healthy, then stops
```

A change is ready when `make verify` exits 0 and the race detector is clean.
Run `go build ./...` after every file change rather than at the end.

CI runs the same commands, plus `make stack-verify` and a GoReleaser config
check.

## Go conventions

- Target the current stable Go release. Keep the `go` directive in `go.mod`
  accurate.
- Depend on the standard library first. Justify a new dependency in the pull
  request body.
- Build a static binary and ship it `FROM scratch`. Do not add a runtime layer.
- Return an error. Do not call `panic` outside `main` and outside package
  initialization.
- Wrap an error with `fmt.Errorf("...: %w", err)` and add the failed operation
  as context.
- Accept a `context.Context` as the first parameter of every function that does
  input or output. Respect cancellation.
- Log to standard output with `log/slog` in JSON format. These processes run in
  Kubernetes.
- Read configuration through `internal/config`. Do not read an environment
  variable directly from a package under `cmd/`. A test in each `cmd` package
  walks the syntax tree and fails on a direct read.
- No component may hold an address it did not receive as configuration. This is
  constraint C3 of `docs/sdd.md` section 1, and a test enforces it against every
  string literal in the tree.
- Define an interface at the consumer, and only when a second implementation
  exists.
- Document an exported identifier with a comment that starts with the identifier
  name.

## Testing

- Write the failing test before the implementation, for a new behaviour and for
  a bug fix.
- Name a test `Test{Unit}_{Scenario}_{Expectation}`. Use a table test for a set
  of similar cases.
- Test a pure function directly. Test config parsing and template resolution as
  unit tests.
- Test the AMQP publish path and the WireMock admin path against a container.
  Do not add an interface to make a package testable.
- A test must not depend on a shared environment, a real upstream service, or a
  fixed sleep. Poll for a condition with a timeout instead.
- Never skip a test with `t.Skip` to make a pipeline pass.

## Simulation content conventions

- A scenario references an external JSON payload file and an external SQL file.
  It does not inline the content. Payloads stay reusable, and SQL stays
  reviewable on its own.
- Pin an event payload fixture to the upstream version, for example
  `article-submitted.v1.0.0.json`. A new upstream version needs a new file.
- Keep every connection target in `citadel.yaml`. Do not add an environment
  variable for a target that the config file already holds.
- Keep a WireMock mapping in standard WireMock format. Use `scenarioName` and
  `requiredScenarioState` for state, and keep the state names equal to the event
  names.
- Never commit a real credential, a real token, or production data. Use a
  placeholder value and an environment variable reference.

## Writing style

Prose follows ASD-STE100 Simplified Technical English. This covers commit
messages, pull request bodies, documentation, and doc comments. It does not
cover identifiers, code, or string literals in code.

- Use the active voice and the simple present tense.
- Give one instruction in one sentence.
- Keep a descriptive sentence to 25 words, and a procedural sentence to 20.
- Write `must` for an obligation and `can` for a possibility. Do not write
  `should`, `may`, `might`, or `could`.
- Write `for example` or `that is` in place of `e.g.` or `i.e.` Do not write
  `etc.`
- Do not add information in parentheses or between dashes. Write a new sentence.

Keep quoted output, log lines, error text, YAML keys, and third-party
documentation as they are.

## Git conventions

- Branch prefix: `feature/`, `fix/`, `refactor/`, `docs/`, or `chore/`.
- Commit message: conventional commits, for example `feat:`, `fix:`,
  `refactor:`, `docs:`, `test:`, and `chore:`.
- Keep a pull request to one concern.
- Update `docs/sdd.md` when behaviour diverges from it, or record the difference
  in an issue.

## Releases

A maintainer pushes a `v*` tag. GoReleaser then builds the CLI for macOS,
Linux, and Windows, publishes the archives to the GitHub release, and pushes
multi-architecture sidecar images to GHCR.
