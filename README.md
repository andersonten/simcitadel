# Citadel

[![ci](https://github.com/andersonten/simcitadel/actions/workflows/ci.yml/badge.svg)](https://github.com/andersonten/simcitadel/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/andersonten/simcitadel)](https://github.com/andersonten/simcitadel/releases/latest)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![go reference](https://pkg.go.dev/badge/github.com/andersonten/simcitadel.svg)](https://pkg.go.dev/github.com/andersonten/simcitadel)

A service simulation platform for isolated microservice development. Citadel
replaces upstream dependencies with controllable simulations, so a team deploys
one service and tests it against deterministic events, GraphQL responses, REST
responses, and change data capture.

## What it does

Citadel gives each developer a private, repeatable environment that simulates
every dependency of the system under test:

- **WireMock** serves GraphQL and REST responses with stateful scenarios.
- **Event trigger** publishes AMQP messages on demand through an HTTP call.
- **Event capture** records outbound messages and exposes them for assertion.
- **Debezium** provides change data capture from the application outbox table.
- **`citadel` CLI** runs YAML scenario files that sequence these into a workflow
  and assert the outcome.

A multi-step workflow that takes 1 to 3 hours of manual work runs in under two
minutes and produces the same result every time.

Exit codes are an interface, `--output json` and `--junit` are first-class, and
no command needs a terminal, so the same scenarios run on a laptop and in a
pipeline.

## Architecture

```text
┌─────────────────────────────────────────────────────────────┐
│                     Developer Machine                        │
│                                                              │
│  citadel run ─────┬── event-trigger ──► RabbitMQ ──► app     │
│                   │                                   │      │
│                   ├── WireMock (stubs)  ◄─────────────┘      │
│                   │                                   │      │
│                   ├── event-capture  ◄─── Debezium ◄─ DB     │
│                   │                                          │
│                   └── assertions on state, events, inbox     │
└─────────────────────────────────────────────────────────────┘
```

## Install

### Released binary

Download the archive for your platform from the
[latest release](https://github.com/andersonten/simcitadel/releases/latest),
then put `citadel` on your `PATH`.

macOS and Linux:

```bash
VERSION=0.1.0
OS=$(uname -s | tr '[:upper:]' '[:lower:]')     # darwin or linux
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -sSfL "https://github.com/andersonten/simcitadel/releases/download/v${VERSION}/citadel_${VERSION}_${OS}_${ARCH}.tar.gz" \
  | tar -xz citadel
sudo mv citadel /usr/local/bin/
citadel version
```

macOS marks a downloaded binary as quarantined. Clear it with
`xattr -d com.apple.quarantine /usr/local/bin/citadel` if Gatekeeper blocks the
first run.

Windows, in PowerShell:

```powershell
$Version = "0.1.0"
Invoke-WebRequest "https://github.com/andersonten/simcitadel/releases/download/v$Version/citadel_${Version}_windows_amd64.zip" -OutFile citadel.zip
Expand-Archive citadel.zip -DestinationPath $env:LOCALAPPDATA\citadel
# Add %LOCALAPPDATA%\citadel to your PATH.
```

Every archive carries a checksum in `checksums.txt` on the release page.

### With Go

```bash
go install github.com/andersonten/simcitadel/cmd/citadel@latest
```

A binary built this way reports `dev` for its version, because the build
identity comes from the release process.

### Container images

The sidecars publish to GHCR for `linux/amd64` and `linux/arm64`:

```bash
docker pull ghcr.io/andersonten/event-trigger:latest
docker pull ghcr.io/andersonten/event-capture:latest
```

Pin a release tag rather than `latest` in a compose file that a team shares.

## Adding Citadel to an application repository

Each team owns its own simulation content. Read
[docs/integration-guide.md](docs/integration-guide.md) for the directory
structure, configuration, compose setup, and a step-by-step checklist for a new
team.

## Quickstart with the reference stack

This runs the example application of this repository, which exercises the whole
pipeline. It needs Docker 24 or later, Go 1.26.2 or later, and Make.

### 1. Clone and build

```bash
git clone https://github.com/andersonten/simcitadel.git
cd simcitadel
make verify
```

`make verify` runs `gofmt`, `go mod tidy -diff`, `go vet`, the build, and all
unit tests.

### 2. Set environment variables

```bash
cp .env.example .env
set -a
source .env
set +a
```

`set -a` tells the shell to export every variable it assigns. Without it, the
variables stay local to the shell and the `citadel` binary cannot read them.
The defaults work for the local compose stack without changes.

### 3. Start the compose stack

```bash
make stack-up
```

This starts RabbitMQ, WireMock, PostgreSQL, the event trigger, the event
capture service, the example application, and Debezium. It waits for every
service to report healthy.

### 4. Run a scenario

```bash
make citadel
./citadel run -config examples/reference/citadel.yaml \
  examples/reference/scenarios/editorial-assignment-happy-path.yaml
```

The CLI loads the WireMock stubs, acquires the environment lock, resets state,
runs each step, and evaluates the assertions. Exit code 0 means every assertion
passed.

### 5. Stop the stack

```bash
make stack-down
```

## CLI commands

| Command    | Purpose                                                        |
|------------|----------------------------------------------------------------|
| `run`      | Execute one or more scenario files and report the result       |
| `validate` | Parse and validate scenarios without running them              |
| `doctor`   | Check that every dependency is reachable and configured        |
| `forward`  | Open `kubectl port-forward` tunnels from the Kubernetes config |
| `version`  | Print the build identity                                       |

Run `citadel <command> -help` for the full flag surface of each command.

### Exit codes

| Code | Meaning                                                     |
|------|-------------------------------------------------------------|
| 0    | Every step and every assertion passed                       |
| 1    | An assertion failed, which is a finding about the application |
| 2    | A scenario or a configuration file is invalid               |
| 3    | The environment failed, so the result says nothing about the app |
| 4    | Another run holds the environment lock                      |
| 5    | The run was cancelled                                       |
| 6    | An internal error in Citadel                                |

## Project layout

```text
cmd/citadel/          CLI binary
cmd/event-trigger/    Event trigger sidecar (HTTP to AMQP)
cmd/event-capture/    Event capture sidecar (AMQP to HTTP query API)
cmd/example-app/      Minimal application that exercises the full pipeline
internal/amqp/        AMQP connection and publish logic
internal/capture/     Capture buffer, filtering, and binding
internal/config/      Configuration parsing and path resolution
internal/db/          Database driver registration and helpers
internal/diagnostics/ Diagnostic bundle and doctor checks
internal/expect/      Expectation evaluation (JSONPath, status, headers)
internal/health/      Self-probe for FROM-scratch health checks
internal/runner/      Run lifecycle, reset, lock, output, JUnit
internal/scenario/    Scenario model, parse, validate, template
internal/steps/       Step executors for every step type
internal/trigger/     Trigger HTTP server, budget, CloudEvents validation
deploy/compose/       Docker Compose stack with pinned images
deploy/docker/        Dockerfiles for the released images
deploy/helm/          Helm chart for Kubernetes deployment
examples/reference/   Reference scenario pack
docs/                 Design documentation
```

## Configuration

Citadel reads a `citadel.yaml` file. The file holds every connection target and
every default value. Environment variable references use `${VAR}` syntax and
resolve at load time.

See `examples/reference/citadel.yaml` for a complete example.

### Content directories

The `paths` block names where Citadel finds each kind of content. The defaults
are:

| Key          | Default        | Content                        |
|--------------|----------------|--------------------------------|
| `scenarios`  | `./scenarios`  | Scenario YAML files            |
| `payloads`   | `./payloads`   | Event payload JSON templates   |
| `sql`        | `./sql`        | SQL files for exec and query   |
| `queries`    | `./queries`    | GraphQL query files            |
| `wiremock`   | `./wiremock`   | WireMock mappings and files    |

Each relative path resolves against the directory that holds `citadel.yaml`.
The CLI imports the WireMock pack over the admin API once per run, so a stub
needs no volume mount and works the same in a cluster.

## Scenario file format

A scenario is a YAML file with these sections:

- **`name`** — unique identifier within the directory.
- **`variables`** — key-value pairs available to Go templates in every field.
- **`steps`** — ordered list of actions that drive the environment.
- **`assertions`** — checks that run after all steps pass.

### Step types

| Type                  | Purpose                                           |
|-----------------------|---------------------------------------------------|
| `event/publish`       | Publish an AMQP message through the trigger       |
| `wiremock/set-state`  | Advance a WireMock scenario to a named state      |
| `await`               | Poll a probe source until an expectation passes   |
| `app/graphql`         | Query the application GraphQL endpoint            |
| `app/http`            | Call the application over HTTP                    |
| `event/captured`      | Assert on captured outbound events                |
| `event/not-captured`  | Assert that no matching event exists              |
| `inbox/assert`        | Assert on the inbox table of the application      |
| `sql/exec`            | Execute a SQL statement                           |
| `sql/query`           | Query a database and assert on the result         |
| `wait`                | Pause for a fixed duration                        |

The set is closed, and `citadel validate` rejects an unknown step type or an
unknown key.

## Testing

```bash
make test               # Unit tests
make test-integration   # Container-backed integration tests (needs Docker)
go test -race ./...     # Race detector
```

The integration suite starts its own RabbitMQ and PostgreSQL containers. It runs
behind the `integration` build tag, so `go test ./...` does not trigger it.

## Makefile targets

| Target             | What it does                                                    |
|--------------------|-----------------------------------------------------------------|
| `verify`           | Full gate: fmt, tidy, vet, build, unit tests                    |
| `citadel`          | Build the CLI binary with the build identity                    |
| `test-integration` | Run the container-backed tests                                  |
| `stack-up`         | Start the compose stack and wait for healthy                    |
| `stack-down`       | Stop the stack and remove volumes                               |
| `stack-verify`     | Start, assert healthy, then stop                                |
| `pin-check`        | Assert every compose image digest matches the registry          |

## Kubernetes deployment

The Helm chart in `deploy/helm/citadel-stack/` deploys the sidecars beside the
application in a namespace. The `kubernetes` block in `citadel.yaml` holds the
context, namespace, and port forwards. Run `citadel forward` to open the tunnels
before a scenario run.

## Design documentation

Read [docs/sdd.md](docs/sdd.md) for the full design. It specifies the
architecture, every component, the run lifecycle, the configuration schema, the
scenario format, and the messaging topology. The code refers to it by section
number.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) for the conventions, the verification
gate, and the release process.

## License

MIT. See [LICENSE](LICENSE).
