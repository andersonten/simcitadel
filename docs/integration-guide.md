# Integration guide

This guide shows how to add Citadel simulation content to an application
repository so that the team can run repeatable scenario workflows against their
own service.

Citadel is a platform. This repository ships the CLI, the sidecars, and the
container images. Each application team owns its own simulation content: the
scenarios, the event payloads, the WireMock stubs, the SQL files, and the
configuration that ties them together.

## Prerequisites

| Tool    | Version | Purpose                                     |
|---------|---------|---------------------------------------------|
| Go      | 1.26.2+ | Build the CLI (or use a published binary)   |
| Docker  | 24+     | Run the compose stack                       |
| Make    | any     | Optional, for convenience targets           |
| citadel | latest  | The CLI binary, from the release page       |

The team does not need to clone or build the SimCitadel platform repository
unless it wants to modify the tooling itself. The published container images and
the released CLI binary are enough. Install the CLI from the
[latest release](https://github.com/andersonten/simcitadel/releases/latest), or
with `go install github.com/andersonten/simcitadel/cmd/citadel@latest`.

## Directory structure

Add a `simulation/` directory at the root of the application repository. The
internal layout mirrors the reference pack in `examples/reference/`:

```text
my-service/
├── src/                        ← existing application source
├── simulation/
│   ├── citadel.yaml            configuration file (one per environment)
│   ├── citadel.local.yaml      optional override for local-only values
│   ├── scenarios/
│   │   ├── happy-path.yaml
│   │   └── overdue-reminder.yaml
│   ├── payloads/
│   │   ├── article-submitted.v1.0.0.json
│   │   └── editor-invitation-accepted.v1.0.0.json
│   ├── sql/
│   │   ├── seed-article.sql
│   │   └── outbox-pending-count.sql
│   ├── queries/
│   │   └── article-stage.graphql
│   └── wiremock/
│       ├── mappings/
│       │   ├── article-review-article.json
│       │   └── taxonomy-journal.json
│       └── __files/
│           └── (response body files, if WireMock bodyFileName is used)
├── docker-compose.citadel.yml  compose file for the simulation stack
└── .env.example                environment variable defaults
```

### What each directory holds

| Directory              | Content                                             |
|------------------------|-----------------------------------------------------|
| `simulation/scenarios` | YAML scenario files that define steps and assertions |
| `simulation/payloads`  | CloudEvents JSON templates, one per event version   |
| `simulation/sql`       | SQL files for `sql/exec` and `sql/query` steps      |
| `simulation/queries`   | GraphQL query files for `app/graphql` steps         |
| `simulation/wiremock`  | WireMock mappings and response body files           |

Pin each payload file to the upstream event version it represents. Use the
naming pattern `{event-name}.{version}.json`, for example
`article-submitted.v1.0.0.json`. A new upstream version gets a new file. Do not
overwrite the old one.

## Configuration file

The `citadel.yaml` file holds every connection target, capture binding, and
default value. It lives inside `simulation/` so that the `paths` block can use
relative references. Environment variable references use `${VAR}` syntax and
resolve at load time.

```yaml
version: 1
environment:
  name: local

targets:
  wiremock:
    url: "${WIREMOCK_URL}"
    adminUrl: "${WIREMOCK_ADMIN_URL}"
  event-trigger:
    url: "${EVENT_TRIGGER_URL}"
  event-capture:
    url: "${EVENT_CAPTURE_URL}"
  rabbitmq:
    amqpUrl: "${RABBITMQ_AMQP_URL}"
    managementUrl: "${RABBITMQ_MANAGEMENT_URL}"
  app:
    url: "${APP_URL}"
    graphqlPath: /graphql
    readinessPath: /healthz
  app-db:
    driver: postgres
    dsn: "${APP_DB_DSN}"
    inbox:
      table: inbox
      messageIdColumn: message_id

# Capture bindings declare which exchanges the capture service monitors.
# Use "declare: own" when Citadel must declare the exchange itself.
capture:
  exchanges:
    - name: public.my-service.DomainEvent.v1
      declare: own

defaults:
  awaitTimeout: 30s
  awaitInterval: 500ms
  quietPeriod: 2s
  publishBudget: 100
  appReadyTimeout: 30s
  resetSettle: 2s

# Relative paths resolve against the directory that holds citadel.yaml.
paths:
  scenarios: scenarios
  payloads: payloads
  sql: sql
  queries: queries
  wiremock: wiremock
```

Never commit real credentials. Use placeholder values and `${VAR}` references
that resolve from the environment.

## Compose stack

The application repository needs a compose file that starts the simulation
infrastructure beside the application. Use the images published by the
SimCitadel platform:

```yaml
# docker-compose.citadel.yml
services:
  rabbitmq:
    image: rabbitmq:3.13-management-alpine
    ports:
      - "5672:5672"
      - "15672:15672"
    environment:
      RABBITMQ_DEFAULT_USER: citadel
      RABBITMQ_DEFAULT_PASS: citadel
    healthcheck:
      test: rabbitmq-diagnostics -q check_running
      interval: 5s
      timeout: 5s
      retries: 10

  # The server starts empty. citadel run imports the pack over the admin API at
  # the start of every run, so the stubs need no volume mount here.
  wiremock:
    image: wiremock/wiremock:3.12.1
    ports:
      - "9090:8080"
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/__admin/health"]
      interval: 5s
      timeout: 5s
      retries: 5

  event-trigger:
    image: ghcr.io/andersonten/event-trigger:0.1.0  # pin to a release tag
    ports:
      - "8081:8081"
    environment:
      AMQP_URL: amqp://citadel:citadel@rabbitmq:5672/
    depends_on:
      rabbitmq:
        condition: service_healthy

  event-capture:
    image: ghcr.io/andersonten/event-capture:0.1.0  # pin to a release tag
    ports:
      - "8082:8082"
    environment:
      AMQP_URL: amqp://citadel:citadel@rabbitmq:5672/
    depends_on:
      rabbitmq:
        condition: service_healthy

  app-db:
    image: postgres:16-alpine
    ports:
      - "5432:5432"
    environment:
      POSTGRES_USER: citadel
      POSTGRES_PASSWORD: citadel
      POSTGRES_DB: citadel
    healthcheck:
      test: pg_isready -U citadel
      interval: 5s
      timeout: 5s
      retries: 5

  app:
    build: .
    ports:
      - "5000:5000"
    environment:
      DATABASE_URL: postgres://citadel:citadel@app-db:5432/citadel?sslmode=disable
      RABBITMQ_URL: amqp://citadel:citadel@rabbitmq:5672/
      WIREMOCK_URL: http://wiremock:8080
    depends_on:
      app-db:
        condition: service_healthy
      rabbitmq:
        condition: service_healthy
      wiremock:
        condition: service_healthy
```

Adjust the `app` service to match the application's own Dockerfile, port, and
environment variables. Pin every SimCitadel image to a release tag instead of
`latest`.

## Environment variables

Create a `.env.example` in the application repository with the host-side
addresses that `citadel.yaml` references. These point at the compose stack from
the developer's machine:

```bash
RABBITMQ_AMQP_URL=amqp://citadel:citadel@127.0.0.1:5672/
RABBITMQ_MANAGEMENT_URL=http://citadel:citadel@127.0.0.1:15672
WIREMOCK_URL=http://127.0.0.1:9090
WIREMOCK_ADMIN_URL=http://127.0.0.1:9090/__admin
EVENT_TRIGGER_URL=http://127.0.0.1:8081
EVENT_CAPTURE_URL=http://127.0.0.1:8082
APP_URL=http://127.0.0.1:5000
APP_DB_DSN=postgres://citadel:citadel@127.0.0.1:5432/citadel?sslmode=disable
```

Copy the file to `.env` and source it before a run.

## Writing a scenario

A scenario file has four sections: metadata, variables, steps, and assertions.
Steps drive the environment. Assertions verify the outcome after all steps pass.

```yaml
name: order-created-happy-path
description: >
  Publish an Order.Created event and assert the application processes it,
  stores the order, and emits a downstream notification.

variables:
  orderId: "12345"
  customerId: "cust-001"

steps:
  - name: set-wiremock-state
    type: wiremock/set-state
    scenarioName: OrderLifecycle
    state: OrderCreated

  - name: publish-order-created
    type: event/publish
    exchange: public.orders.Order.v1
    routingKey: order-created
    dataFile: order-created.v1.0.0.json
    as: created

  - name: await-order-stored
    type: await
    target: app
    graphql:
      query: |
        query Order($id: String!) {
          order(id: $id) { id status }
        }
      variables:
        id: "{{ .orderId }}"
    expect:
      jsonPath: "$.data.order.status"
      equals: "Received"
    retry:
      until: pass
      timeout: 10s
      interval: 500ms

assertions:
  - name: downstream-notification-emitted
    type: event/captured
    captured:
      exchange: public.my-service.Notification.v1
      eventType: OrderReceived
    expect:
      countAtLeast: 1

  - name: inbox-row-recorded
    type: inbox/assert
    target: app-db
    where:
      messageId: "{{ .created.id }}"
    expect:
      rows: 1
      notNull: [processed_at]
```

### Key conventions

- **Reference external files.** Use `dataFile` for payloads, `file` for SQL, and
  `queryFile` for GraphQL. Do not inline large content.
- **Pin payload versions.** Name each payload file after its upstream event
  version. A new version gets a new file.
- **Use Go templates.** Variables declared in the `variables` block and step
  outputs (through `as`) are available in `{{ .varName }}` syntax.
- **Poll, do not sleep.** Use `retry` with `until: pass` instead of a `wait`
  step. The scenario proceeds the moment the condition holds.
- **Assert on application state.** Read the application's own data, not the
  mock. This catches bugs that a mock assertion would mask.

## Running a scenario

```bash
# 1. Start the stack
docker compose -f docker-compose.citadel.yml up -d --wait

# 2. Export the environment
cp .env.example .env   # first time only
set -a && source .env && set +a

# 3. Run the doctor check
citadel doctor -config simulation/citadel.yaml

# 4. Run a scenario
citadel run -config simulation/citadel.yaml \
  simulation/scenarios/order-created-happy-path.yaml

# 5. Stop the stack
docker compose -f docker-compose.citadel.yml down -v
```

`citadel doctor` checks that every target in the configuration is reachable and
configured. Run it once after the stack starts to catch connection problems
before a scenario run.

Exit code 0 from `citadel run` means every step passed and every assertion held.

## WireMock stubs

WireMock mappings use standard WireMock JSON format. Use `scenarioName` and
`requiredScenarioState` to make stubs stateful. Keep the state names equal to
the event names for readability.

```json
{
  "scenarioName": "OrderLifecycle",
  "requiredScenarioState": "OrderCreated",
  "request": {
    "method": "POST",
    "urlPath": "/graphql",
    "bodyPatterns": [
      { "contains": "order" }
    ]
  },
  "response": {
    "status": 200,
    "headers": { "Content-Type": "application/json" },
    "jsonBody": {
      "data": {
        "order": {
          "id": 12345,
          "status": "Created"
        }
      }
    }
  }
}
```

`citadel run` imports the whole pack over the WireMock admin API once per run,
before the first scenario. It reads the directory that `paths.wiremock` names,
uploads every mapping and every `__files` body, and removes a mapping that no
file holds. A stub therefore exists only because a file in the repository
declares it, and the same mechanism works against a local container and a
cluster. The `wiremock/set-state` step advances the scenario state during a run,
and the reset between scenarios returns every scenario to its start state
without touching the mappings.

## Kubernetes deployment

For in-cluster development, the SimCitadel Helm chart deploys the sidecars
beside the application in a namespace. The configuration file gains a
`kubernetes` block:

```yaml
kubernetes:
  context: my-cluster
  namespace: citadel-dev
  portForwards:
    - service: wiremock
      localPort: 9090
      remotePort: 8080
    - service: event-trigger
      localPort: 8081
      remotePort: 8081
```

Run `citadel forward` to open the tunnels before a scenario run. The same
scenarios, payloads, and stubs work in both local and cluster environments.

## Checklist for a new team

1. Create `simulation/` with the subdirectories listed above.
2. Write `citadel.yaml` with the targets for the application.
3. Add a compose file that starts the stack beside the application.
4. Create `.env.example` with the host-side connection values.
5. Write one WireMock stub for the most common upstream query.
6. Write one payload file for the most common inbound event.
7. Write a single scenario that publishes the event, waits for the application
   to process it, and asserts the outcome.
8. Run `citadel doctor` and fix every reported problem.
9. Run the scenario. Exit code 0 means it works.
10. Add `simulation/` content to version control. Add `.env` to `.gitignore`.

Start with one workflow. Expand to more scenarios after the first one passes.

## Reference

- `docs/sdd.md` in the SimCitadel repository is the design source of truth. It
  specifies the architecture, every component, the run lifecycle, the scenario
  format, and the messaging topology.
- `examples/reference/` in the SimCitadel repository is a complete scenario
  pack that demonstrates every content type.
- Run `citadel run -help`, `citadel doctor -help`, and `citadel validate -help`
  for the full flag surface of each command.
