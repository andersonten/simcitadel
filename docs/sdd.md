# Citadel — Software Design Document

| | |
|--|--|
| **Scope** | Local docker-compose and shared-cluster `default` namespace |
| **Audience** | Implementers and contributors |

---

## 0. How to read this document

This document specifies *what to build*, to the level of detail where two
independent implementers would produce compatible systems. The code in this
repository refers to it by section number, so a section number is a stable
reference.

Every section that defines an interface is normative. Where a value is a
placeholder to be measured or confirmed, it is marked **TBD** with the method for
resolving it. Where a design choice needed an argument rather than a fact, it is
marked **[SDD decision]** with its reasoning.

Two constraints are easy to get wrong and each has its own section: §7.4
(alternate exchanges cannot be declared on app-owned exchanges) and §6.6
(WireMock webhook templating cannot be assumed to see the response body).

---

## 1. Constraint traceability

Four constraints bind this design. Each is discharged by a specific mechanism,
so a reviewer can check the claim rather than take it.

| Constraint | Discharged by |
|--|--|
| **C1** Simplicity — one command, one binary, no bespoke setup | Single static `citadel` binary (§3.2); `docker compose up` with no init scripts run by hand (§11.1); every service self-configures from environment variables |
| **C2** No assumption of a terminal or a human | Every command is non-interactive by default (§4.5); `--output json` and `--junit` (§4.6); exit codes are an interface (§4.4); all input from files, flags, environment |
| **C3** Environment identity is a parameter | Addresses exist only in `citadel.yaml` (§5); no component may read a hostname it did not receive as configuration (§3.4); service names and queue names derive from `environment.name` (§7.2) |
| **C4** Scenarios are data | The step schema is closed (§6.2); there is no expression evaluation, only value substitution (§6.4); `citadel validate` rejects unknown keys |

**C3 is the one to enforce mechanically**, because it is invisible when violated
and expensive when discovered. §14.3 specifies a build-time test that greps the
compiled configuration surface for literal `localhost`, `127.0.0.1` and
`:5672`-style defaults and fails the build.

---

## 2. What Milestone 1 delivers

Three Go binaries, one Helm chart, one compose stack, one reference scenario
pack.

| Artifact | Form | Consumed by |
|--|--|--|
| `citadel` | Static binary, 4 platforms | Developers, later CI |
| `event-trigger` | Container image | The stack |
| `event-capture` | Container image | The stack |
| `citadel-stack` | Helm chart | Shared cluster |
| `deploy/compose` | Compose files + profiles | Developers |
| `examples/reference` | Scenario pack | The demo, and the template for adopters |

Nothing else is in scope. Notably absent and deliberately so: a web UI, a
scenario recorder, a Kubernetes operator, and any pipeline definition.

---

## 3. System design

### 3.1 Process model

Seven long-lived processes in the default profile, plus one init container that
runs and exits. Each is independently restartable and holds no state another
process depends on, except where noted.

| Process | Image | State it holds | Restart cost |
|--|--|--|--|
| `wiremock` | `wiremock/wiremock` (pinned) | Scenario states, request journal | Scenario must re-set state |
| `rabbitmq` | `rabbitmq:*-management` | Topology, queue contents | Full environment reset |
| `app-db` | `postgres` | App state, outbox, inbox | Full environment reset |
| `event-trigger` | Built | Publish budget counters, keyed by run ID | Budget resets; harmless |
| `event-capture` | Built | Ring buffer of captured messages | Buffer lost; a run in flight fails |
| `app` | `${APP_IMAGE}`, default built | App state in `app-db`; none of its own | Re-creates its schema; idempotent |
| `debezium` | `debezium/server` (see the §10.1 pin) | Replication slot in `app-db`, in-memory offsets | Re-snapshots; see the §4.9 caveat |
| `rabbitmq-init` | `rabbitmq:*-management` | None; exits after it loads the §7.4 policy | Re-runs; idempotent |

`app` is the application under test, and a consuming repository replaces it by
setting `APP_IMAGE`. The default builds the example application of this
repository, which exists so the whole pipeline runs here: it consumes the inbound
event, reads its upstream through WireMock, and writes an inbox row and an outbox
row in one transaction. It is not a production application and it does not stand in
for one. Its purpose is that §14.4 can run, and the first run of it found four
defects that failed every run and that no unit test had reached.

`debezium` starts after `app` reports healthy, and that ordering is a requirement
rather than a preference. The connector resolves `table.include.list` against the
live catalog when it starts, and with `publication.autocreate.mode=filtered` it
finds no match, refuses to create the publication and exits 1 rather than wait.
`app` creates `public.outbox` before it reports healthy, so the health condition
is what lets `debezium` run in the default profile at all.

`citadel` itself is not a process in the stack. It is a client, run from the
developer's machine or from a pod, and it holds all run state in memory for the
duration of one run.

### 3.2 Why the CLI is a client and not a server

**[SDD decision]** The alternative — a long-lived runner service in the stack
that the developer POSTs scenarios to — was considered and rejected. It would
make Kubernetes execution marginally simpler (no port-forwards) but it breaks
C1: the developer's scenario files, payloads and SQL live on their disk, and a
server would need them uploaded, mounted or baked into an image on every edit.
The edit-run loop is the product. A client that reads local files and talks to
remote services keeps that loop at one command in both environments, and
port-forwarding is a solved problem.

Consequence: `citadel` needs network reachability to every target. In Kubernetes
that means `kubectl port-forward` for each. §4.7 specifies a `citadel forward`
helper so this is one command rather than six terminals.

### 3.3 Trust and network boundaries

The event trigger is an unauthenticated "publish anything to the broker"
endpoint. This is acceptable only because it is never routable from outside the
environment:

- Compose: published on `127.0.0.1` only, never `0.0.0.0`.
- Kubernetes: `ClusterIP`, no Ingress, reached by port-forward.
- The Helm chart has no value that can expose it. There is no `ingress.enabled`
  key to set by accident.

### 3.4 Configuration discipline

Every process takes its entire configuration from environment variables, with no
defaults that encode an address. A missing required variable is a startup
failure with the variable's name in the message, never a fallback to
`localhost`.

```go
// internal/config/env.go — the only way a service reads its environment.
func MustURL(key string) *url.URL   // absent or unparseable → fatal, names the key
func MustString(key string) string
func OptDuration(key string, def time.Duration) time.Duration  // defaults allowed
                                                               // for tuning, never
                                                               // for identity
```

This is C3 in code. The type signature is the enforcement: there is no
`GetURLOrDefault`.

---

## 4. The `citadel` CLI

### 4.1 Command surface

```
citadel run <path>...            Execute one scenario file, or every *.yaml under a directory
citadel validate <path>...       Static checks. No environment contact. Exit 2 on any failure
citadel doctor                   Check every configured target is reachable and correctly set up
citadel reset                    Perform the reset sequence (§4.9) without running a scenario
citadel logs --last-failure      Print the most recent diagnostic bundle
citadel forward                  Establish port-forwards for a Kubernetes config and hold them
citadel version                  Version, commit, build date; --output json supported
```

Global flags, valid on every command:

```
--config <path>        Default: ./citadel.yaml, then $CITADEL_CONFIG
--output human|json    Default: human when stdout is a TTY, json otherwise   [SDD decision]
--verbose              Step-level detail to stderr
--no-color             Also implied by NO_COLOR or a non-TTY stdout
```

The `--output` default is the one place a TTY is consulted, and it selects
presentation only. Behaviour, exit codes and side effects are identical either
way, which is what C2 actually requires. A pipeline that wants human output can
ask for it.

`run`-specific flags:

```
--set k=v              Repeatable. Overrides a scenario variable
--junit <path>         Write JUnit XML. One <testcase> per step and assertion
--bundle-dir <path>    Diagnostic bundle root. Default: ./.citadel/runs
--step-through         Pause before each step. Requires a TTY; errors without one
--keep-going           Continue after a failed assertion. Steps still stop on failure
--timeout <dur>        Whole-run ceiling. Default 10m. Distinct from per-step timeouts
```

`--step-through` is the single interactive affordance and it fails loudly
without a TTY rather than hanging, which is the failure mode C2 is guarding
against.

### 4.2 Run lifecycle

Fixed order. Each phase either completes or ends the run with the exit code
shown.

| # | Phase | On failure |
|--|--|--|
| 1 | Load and interpolate config (§5.2) | 2 |
| 2 | Static validation of every scenario to be run (§13) | 2 |
| 3 | Acquire the environment lock (§4.8) | 5 |
| 4 | Preflight: health of every target the scenario references (§4.10) | 3 |
| 5 | Load the WireMock pack under `paths.wiremock` (§5.1) | 3 |
| 6 | Acquire Hydra token if any target needs auth (§9) | 3 |
| 7 | Reset (§4.9) | 3 |
| 8 | Record `runStart`; open the capture window | — |
| 9 | Execute `steps` in order | 1 |
| 10 | Execute `assertions` in order | 1 |
| 11 | End-of-run environment checks (§4.11) | 1 |
| 12 | Always: `after:` block, bundle on failure, release lock | — |

Phase 5 runs once per run and not once per scenario. The reset of §4.9 returns
every scenario to its start state and leaves the mappings in place, which an
integration test proves against the pinned image.

The pack loads over the admin API rather than through a mount. A mount cannot
reach a cluster without a ConfigMap, a ConfigMap caps the whole pack at 1 MiB,
and a stub change behind either one needs a pod restart. The CLI already holds
`adminUrl` and already forwards the port per §4.7, so it needs nothing new. This
is also what makes the CLI the sole authority over mock state, per decision D1.

The import is authoritative: it sets `deleteAllNotInImport`, so a stub that no
file under `paths.wiremock` holds is removed from the server. A mapping that
outlives the file that declared it can answer a request and make a scenario pass
against a stub nobody can find in the repository.

Two cases load nothing and are not failures. An absent `paths.wiremock` and a
pack with no mapping both leave the server as it is, because a scenario that
touches no upstream needs no pack, and an empty authoritative import would delete
whatever the server already holds. Both log the path they looked in, because an
empty mock answers every upstream call with a 404 and the path is the first thing
worth knowing.

A `__files` subdirectory uploads with the pack, so a `bodyFileName` resolves. The
relative path is preserved, and `PUT /__admin/files/{name}` carries it.

Phases 9 and 10 differ in one respect only: a failed step aborts the remaining
steps, whereas assertions all run and report together unless `--keep-going` is
absent — in which case the first failed assertion still lets the rest run, and
only the exit code differs. **[SDD decision]** Assertions always all run. A
scenario that fails three assertions should tell you all three; that is the
whole reason for separating them from steps.

Phase 12 runs even on panic, via `defer`. The lock release and the bundle write
are the two things that must never be skipped.

### 4.3 Run identity

Every run generates a `runId` (UUIDv7, so it sorts by time). It is:

- sent on every event trigger request as `X-Citadel-Run`, which is how the
  trigger scopes its publish budget;
- stamped into every published message as a CloudEvents extension attribute
  `citadelrun`;
- the diagnostic bundle directory name;
- included in every JSON output line.

**[SDD decision]** Stamping the run ID into the envelope costs one attribute and
buys the ability to tell "this message is from my run" from "this message was
left behind", which is otherwise guesswork when someone is looking at a shared
cluster. CloudEvents permits extension attributes, and the event standard does not forbid
them. The event capture service filters on it by default; `--any-run` disables
that for debugging.

**A message that carries no run ID matches a run-scoped query.** The stamp is
applied by the event trigger, so it is present on an inbound event and absent from
an outbound one: the application under test produces that through its outbox and
Debezium, and it knows nothing about Citadel. Rejecting an unstamped message
therefore hid every event the application emits, which is most of what a scenario
asserts on. This costs nothing, because §4.8 holds an exclusive lock so one run
uses an environment at a time and §4.9 clears the capture buffer before every
scenario, so an unstamped message in the buffer arrived during this run. A message
stamped with a *different* run is still rejected, which is the case §4.3 is for.

### 4.4 Exit codes

The exit code is a stable interface. Changing a meaning is a breaking change.

| Code | Meaning | Typical cause |
|--|--|--|
| 0 | Everything passed | |
| 1 | A step or assertion failed | The system under test behaved unexpectedly. **This is the only code that indicates a finding about the app** |
| 2 | Invalid input | Bad config, bad scenario, missing referenced file |
| 3 | Environment problem | Target unreachable, unhealthy, misconfigured, auth failed |
| 4 | Run timeout | `--timeout` exceeded |
| 5 | Lock held | Another run is in progress |
| 6 | Internal error | A bug in Citadel |

Separating 1 from 3 is the point of the table. A pipeline, or a developer,
needs to distinguish "the app is wrong" from "the environment is broken",
because those go to different people. Every failure path must classify itself
into one of these; a step that cannot reach its target exits 3, not 1, even
though it failed during phase 9.

### 4.5 Non-interactivity

No command prompts. No command reads stdin unless the path argument is `-`.
No command requires a terminal except `--step-through`. Colour is suppressed
without a TTY. Progress output is line-oriented, never a redrawn spinner, so a
captured log is readable.

### 4.6 Machine-readable output

`--output json` emits newline-delimited JSON to stdout, one object per event.
Human-facing narration goes to stderr, so `citadel run --output json > run.jsonl`
is clean.

```json
{"ts":"2026-08-19T09:00:00.000Z","runId":"...","type":"run.start","scenario":"editorial-assignment-happy-path","environment":"local"}
{"ts":"2026-08-19T09:00:00.412Z","runId":"...","type":"step.start","index":1,"name":"Put the article stubs in their pre-submission state","stepType":"wiremock/set-state"}
{"ts":"2026-08-19T09:00:00.480Z","runId":"...","type":"step.pass","index":1,"durationMs":68}
{"ts":"2026-08-19T09:00:12.900Z","runId":"...","type":"step.fail","index":4,"durationMs":20001,"failure":{"class":"assertion","expected":"OPEN","actual":"null","jsonPath":"$.data.pe_tasks...","attempts":80}}
{"ts":"2026-08-19T09:00:13.050Z","runId":"...","type":"check.fail","check":"unroutable-depth","failure":{"class":"environment","message":"queue citadel.unroutable holds 1 message(s): exchange public.orchestration-engine.Article.v1, routing key article.submitted"}}
{"ts":"2026-08-19T09:00:13.100Z","runId":"...","type":"run.end","outcome":"fail","exitCode":1,"bundle":".citadel/runs/0191.../"}
```

`check.fail` carries one end-of-run check of §4.11. `check` is one of
`unroutable-depth`, `unconsumed-depth`, `envelope-conformance`,
`skipped-outbox`, and `unmatched-requests`. A check failure is the reason a run
ends non-zero without a failed step, so it reaches the same stream as the step
events. These five and the five above are every event type the stream carries.

`failure.class` is one of `assertion`, `environment`, `timeout`, `input`,
`internal`, and maps to the exit code. The schema is versioned by a
`schemaVersion` field on `run.start`.

`--junit` is written at run end. One `<testcase>` per step and assertion, with
the step name as the test name and the scenario name as the classname, so a
pipeline that understands JUnit renders a scenario as a test suite with no
Citadel-specific integration. **[SDD decision]** This is the cheapest possible
concession to the CI constraint: no pipeline code is written now, but when
someone wants it, the artifact already exists in the format every CI system
already parses.

### 4.7 `citadel forward`

Reads the config's `kubernetes` block, starts a `kubectl port-forward` per
target, waits for each local port to accept a connection, prints the mapping,
and blocks until interrupted. On exit it tears every forward down. It exists
because six terminals is a bad first impression and because a half-established
set of forwards produces exactly the confusing partial failures §12 is trying to
eliminate.

It shells out to `kubectl` rather than embedding `client-go`'s port-forwarder.
**[SDD decision]** Embedding pulls in a large dependency tree and a kubeconfig
resolution problem for a convenience command; shelling out inherits the
developer's working kubectl context, which is what they expect anyway. If
`kubectl` is absent the command says so and exits 3.

### 4.8 The environment lock

Decision: one scenario at a time per environment. The lock is an **exclusive,
auto-delete RabbitMQ queue** named `citadel.lock`.

```
queue.declare(name="citadel.lock", exclusive=true, auto_delete=true, durable=false)
```

A second client's declare fails with `RESOURCE_LOCKED` (405), which is reported
as exit 5 with the message "another run holds the lock on environment `<name>`".
The lock releases when the connection closes, including when the process is
killed, which is the property a lockfile does not have.

**[SDD decision]** Chosen over a lockfile (wrong scope — the environment is
shared, the filesystem is not), over a lock table in the app database (writes to
the system under test), and over a lock endpoint on the capture service (needs
a lease and expiry, which is a distributed-systems problem for no gain).
RabbitMQ already provides exactly this primitive with the right lifetime, in
both environments, with no new code.

The trade-off is that the lock is unavailable if the broker is down — but in
that case there is nothing to run.

### 4.9 Reset

Unconditional, always, before every run. Order matters and is fixed.

1. `POST {wiremock}/__admin/scenarios/reset`
2. `DELETE {wiremock}/__admin/requests`
3. Purge every queue in the environment — the app's, the spy queues, the
   unroutable queue — via the RabbitMQ management API. **Not the lock queue**:
   §4.8 declares it exclusive, so the management API answers 400 for a purge of
   it while a run holds it, and it holds no test data
4. `DELETE {event-capture}/captured`
5. `POST {event-trigger}/budget/reset` for this `runId`
6. Truncate the tables named in `reset.truncate`, in the order given
7. Settle: wait `defaults.resetSettle` (default 250 ms), then verify all purged
   queues report depth 0. If any does not, fail with exit 3 naming the queue

Step 2 clears the request journal with `DELETE`. The pinned WireMock 3.12.1 does
not serve `POST /__admin/requests/reset` and answers it with 404, which failed
every reset and therefore every run.

Purging before clearing the capture buffer (3 before 4) is deliberate:
the reverse order lets a message that was in flight during the purge land in a
freshly cleared buffer and pollute the run.

Step 7 exists because "purge returned 200 but a producer is still writing" is a
real state and it produces a scenario that fails on a message it never sent.

**What reset does not do.** It does not reset Debezium, and it cannot. A
Postgres replication slot lives in the database, so a shadow database that
survives between runs resumes from its old LSN and never re-snapshots.
`citadel reset` detects a pre-existing slot and prints the `docker compose down -v`
command needed for a true reset rather than pretending it did one.

### 4.10 Preflight

Only the targets the scenario actually references are checked, so a scenario
that does not use CDC is not blocked by a broken Debezium.

| Target | Check |
|--|--|
| wiremock | `GET /__admin/health` returns 200 |
| event-trigger | `GET /health` reports `broker: connected` |
| event-capture | `GET /health` reports every configured spy queue bound |
| rabbitmq | Management API reachable; the lock queue declared |
| app | The configured readiness endpoint returns 200, retried to `defaults.appReadyTimeout` (default 60 s) |
| app-db, upstream-shadow | `SELECT 1` |
| debezium | `GET /q/health` is `UP` **and** the connector's state is `RUNNING` |

The last row matters: Debezium reports healthy while a connector has failed
(§7), so health alone is not evidence.

### 4.11 End-of-run environment checks

Run after assertions, and they can fail a run that passed every assertion.

- **Unroutable depth.** If `citadel.unroutable` (§7.4) is non-empty, fail and
  list the exchange and routing key of each message. A message nobody could
  route is a broken scenario even if the assertions happened to pass.
- **Unconsumed depth.** Any app-consumed queue with depth > 0 is reported. A
  warning by default, an error if `strictUnconsumed: true`.
- **Envelope conformance.** Every message captured during the run, except on
  exchanges listed in `capture.ignore`, is validated against CloudEvents 1.0 plus
  the event standard's required `version` attribute. A malformed outbound event
  fails the run even if no assertion mentioned it. The exemption list exists
  because infrastructure messages such as the Debezium heartbeat are not business
  events and are not required to conform (§10.1).
- **Skipped outbox operations.** The Debezium connector log is scanned over the
  run window for EventRouter invalid-operation warnings. An `UPDATE` or `DELETE`
  against the outbox is skipped with only a log line under the production
  `table.op.invalid.behavior=warn` setting, which means a fixture that mutates an
  outbox row instead of inserting one produces silence. Any occurrence fails the
  run with the offending row's id.
- **Unmatched requests.** If WireMock recorded unmatched requests during the run
  window, they are reported with near-misses. A warning by default; an error if
  `strictUnmatched: true`.

**[SDD decision]** Conformance is an error and unmatched is a warning, because a
malformed event is unambiguously a defect while an unmatched request is
sometimes just the app polling something irrelevant. Both are configurable; the
defaults are chosen so the first experience is not a wall of failures.

---

## 5. Configuration

### 5.1 File

One file per environment. Scenarios never contain an address.

```yaml
# citadel.yaml
version: 1
environment:
  name: local              # Used in queue names, bundle metadata, and lock messages

targets:
  wiremock:
    url:      http://127.0.0.1:9090
    adminUrl: http://127.0.0.1:9090/__admin
  event-trigger:
    url: http://127.0.0.1:8081
  event-capture:
    url: http://127.0.0.1:8082
  rabbitmq:
    amqpUrl:       "amqp://${RABBITMQ_USER}:${RABBITMQ_PASSWORD}@127.0.0.1:5672/"
    managementUrl: http://127.0.0.1:15672
  app:
    url:             http://127.0.0.1:5000
    graphqlPath:     /graphql
    readinessPath:   /health/ready
    auth:            hydra-client
  app-db:
    driver: postgres
    dsn:    "${APP_DB_DSN}"
    inbox:  { table: inbox, messageIdColumn: message_id }
  upstream-shadow:
    driver: postgres
    dsn:    "${SHADOW_DB_DSN}"
  debezium-outbox:
    url:           http://127.0.0.1:8083
    connectorName: reference-app-outbox

auth:
  hydra-client:
    tokenUrl:     "${HYDRA_TOKEN_URL}"
    clientId:     "${CITADEL_CLIENT_ID}"
    clientSecret: "${CITADEL_CLIENT_SECRET}"
    scopes:       [reference-app.read]

capture:
  exchanges:
    - { name: public.orchestration-engine.Article.v1, declare: own }
    - { name: public.orchestration-engine.Task.v1,    declare: own }
  ignore:                       # Captured and queryable, but exempt from the
    - private.orchestration-engine.Heartbeat.v1   # conformance check (§4.11)

defaults:
  awaitTimeout:     30s
  awaitInterval:    250ms
  quietPeriod:      5s
  publishBudget:    100
  appReadyTimeout:  60s
  resetSettle:      250ms
  strictUnconsumed: false
  strictUnmatched:  false

paths:
  scenarios: ./scenarios
  payloads:  ./payloads
  sql:       ./sql
  queries:   ./queries
  wiremock:  ./wiremock

kubernetes:              # Only present in the cluster config
  context:   my-cluster
  namespace: default
  forwards:
    - { service: citadel-wiremock,       localPort: 9090,  remotePort: 8080 }
    - { service: citadel-event-trigger,  localPort: 8081,  remotePort: 8080 }
    - { service: citadel-event-capture,  localPort: 8082,  remotePort: 8080 }
    - { service: citadel-rabbitmq,       localPort: 15672, remotePort: 15672 }
    - { service: citadel-rabbitmq,       localPort: 5672,  remotePort: 5672 }
    - { service: reference-app,          localPort: 5000,  remotePort: 8080 }
```

A relative `paths` entry resolves against the directory holding the config file
and not against the working directory, so a run gives the same answer from
anywhere.

`paths.wiremock` is the pack the run loads in phase 5 of §4.2. It holds the
mappings, in either the single-mapping or the `{"mappings": [...]}` form, and an
optional `__files` subdirectory of response bodies. It is the only place stubs
come from: no deployment mounts them, which is what lets one mechanism serve a
local stack and a cluster.

The consequence is deliberate. A freshly deployed environment serves no stub
until a run loads one, so an upstream call before the first run returns 404.

The Kubernetes config is the same file with different values and a `kubernetes`
block. That the two differ only in values is C3 being true rather than claimed,
and it is what a preview environment would later exploit: a generated config per
namespace, nothing else.

**The `driver` field of a database target.** Two dialects are legal.

| Value | Dialect | Parameter marker |
|--|--|--|
| `postgres`, `postgresql`, `pgx` | PostgreSQL | `$1` |
| `sqlserver`, `mssql` | SQL Server | `@p1` |

`postgres` is the expected value, and §10.2 gives SQL Server its own profile for
the team whose upstream is SQL Server. The dialect decides the parameter marker,
so `sql/exec`, `sql/query` and `inbox/assert` each generate the correct one. A
scenario that writes a marker in its own SQL owns that choice, and a scenario
written against Postgres does not run unchanged against SQL Server.

Three preflight and doctor checks read Postgres catalog tables — `wal_level`, the
replication slots and the stale slot — so each one skips a SQL Server target
rather than report a failure about a table that dialect never has.

### 5.2 Resolution rules

1. `${VAR}` is substituted from the process environment. **A referenced variable
   that is unset is a fatal error naming the variable.** There is no empty-string
   fallback; that is how an empty password reaches a broker and produces a
   confusing 403.
2. `${VAR:-default}` supplies a default explicitly, for values that genuinely
   have one. Addresses must not use it.
3. Substitution happens on string values only, after YAML parsing, so a `$` in a
   password does not need escaping unless followed by `{`.
4. Unknown top-level or target keys are an error, not a warning. A typo'd key
   that is silently ignored produces a mysterious runtime failure.
5. `.env` files are **not** read automatically. **[SDD decision]** Implicit
   environment loading makes the same command behave differently in two
   directories, which is the opposite of C1's promise. Compose loads `.env` for
   the stack; the developer's shell provides the CLI's variables, and `doctor`
   tells them exactly which are missing.

### 5.3 Secrets

Secrets appear only as `${VAR}` references. `citadel validate` fails on a
literal-looking secret (a `clientSecret` or `dsn` containing no `${`). Bundle
redaction is specified in §12.2.

---

## 6. Scenario model

### 6.1 Document shape

```yaml
name:        string          # required, unique within a directory
description: string          # optional
variables:   map[string]any  # optional, overridable with --set
reset:       ResetSpec       # optional; defaults applied
steps:       [Step]          # required, non-empty, ordered
assertions:  [Step]          # optional, ordered, all executed
after:       [Step]          # optional, always executed
```

`assertions` and `after` hold the same step objects as `steps`. There is one
step schema, not three. The lists differ only in when they run and how failure
is handled (§4.2).

### 6.2 Step types

The closed set. `citadel validate` rejects anything else. Five are irreducible;
the rest are named forms over them, and the table says which, because a reader
should be able to see that the tool is small.

| `type` | Irreducible? | Purpose |
|--|--|--|
| `event/publish` | ● | Publish a CloudEvents message |
| `event/republish` | over `event/publish` | Re-send a stored message byte-for-byte, same `id` |
| `wiremock/set-state` | ● | Move a WireMock scenario to a named state |
| `sql/exec` | ● | Run a statement against a configured database |
| `sql/query` | over `probe` | Run a query and assert on the result set |
| `probe` | ● | Read something once and compare (§6.5). Rarely written directly |
| `await` | over `probe` | `probe` with retry defaulted on |
| `app/graphql` | over `probe` | Query the app's GraphQL endpoint and assert |
| `app/http` | over `probe` | Call an app REST endpoint and assert |
| `event/captured` | over `probe` | Assert a matching message was captured |
| `inbox/assert` | over `sql/query` | Assert on the consumer's inbox table (§8.4) |
| `event/not-captured` | ● | Assert no matching message over a quiet period (§8.3) |
| `wait` | ● | Sleep. Requires a `reason:` field, enforced by validate |

`event/not-captured` is irreducible because a negative over a time window is not
a repeated positive check; §8.3 specifies why.

`wait` requiring a `reason` is deliberate friction. This design bans sleeps as
synchronisation; this makes the remaining legitimate uses self-documenting and
makes an illegitimate one visible in review.

### 6.3 Common step fields

```yaml
- name:     string            # required. Used in output, JUnit, and diagnostics
  type:     string            # required
  as:       string            # optional. Binds the step's message to a variable
  optional: true              # optional. Failure is reported, run continues
  timeout:  30s               # optional. Per-step ceiling
  retry:                      # optional
    until:    pass            # only value in Milestone 1
    timeout:  30s
    interval: 250ms
```

`retry` is the only repetition mechanism in the system. `await` is exactly a
`probe` whose `retry` defaults to `{until: pass, timeout: defaults.awaitTimeout,
interval: defaults.awaitInterval}`. **[SDD decision]** One mechanism, one set of
bugs, one thing to explain. Treating `await` as a primitive and retry as a
modifier would mean two timeout semantics and two ways to be flaky.

Retry interval is fixed, not exponential. **[SDD decision]** These are
sub-second polls against a local service over a 30-second ceiling; backoff adds
a tuning parameter and latency at the end of the window, in exchange for load
that does not matter at this scale.

### 6.4 Variables and templating

Go `text/template` with a deliberately small function set. There is no
arithmetic, no comparison, no conditional, and no loop — C4.

| Function | Result |
|--|--|
| `uuid` | A fresh UUIDv4 |
| `now` | RFC 3339 UTC timestamp at render time |
| `nowOffset "-72h"` | Timestamp offset from now. For past-dated seeding (§6.4) |
| `default .x 5` | `.x` if set and non-empty, else the literal |

Resolution order for a variable: `--set` flag, then the scenario's `variables`
block, then failure. An unresolved reference is an error at validate time, not a
rendered empty string.

**Message references.** A step with `as: accepted` binds `.accepted`:

```
.accepted.id           CloudEvents id of the message
.accepted.exchange     .accepted.routingKey
.accepted.body         Parsed JSON; .accepted.body.data.articleId is addressable
.accepted.headers      map
.accepted.sentAt       RFC 3339
```

For a receiving step (`event/captured`), the binding is the matched message and
`receivedAt` replaces `sentAt`. If more than one message matched, `as` binds the
first and validate warns that the assertion is ambiguous.

### 6.5 The probe and its expectations

A probe has exactly one source and one `expect`.

```yaml
- name: Wait for the engine to open the editor-assignment task
  type: await
  target: app                    # must resolve to a non-WireMock target (check 6 of §13)
  graphql:
    queryFile: queries/pe-tasks.graphql
    variables: { subjectId: "{{ .articleId }}" }
  expect:
    jsonPath: "$.data.pe_tasks.items[?(@.taskKey=='assign-editor')].status"
    equals:   "OPEN"
  timeout: 20s
```

Sources: `graphql` (`query` or `queryFile`, plus `variables`), `http`
(`method`, `path`, `body`/`bodyFile`, `headers`), `sql` (`query` or `file`, plus
`args`), `captured` (the capture query of §8.2), `journal` (a WireMock request
journal query).

Expectation keys, and the complete list of them:

| Key | Applies to | Meaning |
|--|--|--|
| `status` | http, graphql | HTTP status equals |
| `jsonPath` + `equals` | http, graphql, captured | Single path equals a scalar |
| `jsonPath` (map) | http, graphql, captured | Every path equals its value |
| `jsonPathExists` | same | Path resolves to at least one node |
| `rows` | sql | Row count equals |
| `count` | captured, journal | Match count equals |
| `countAtLeast` | captured, journal | Match count ≥ |
| `columns` | sql | Map of column name to expected value, first row |
| `notNull` | sql | List of columns that must be non-null |
| `cloudEventsValid` | captured | Every match is a valid envelope |
| `graphqlErrors` | graphql | `none` (default) or `any`, since GraphQL errors arrive with HTTP 200 |

`graphqlErrors: none` being the **default** is load-bearing. A GraphQL error
response is a 200 with an `errors` array and a null data path; without this
default, a scenario asserting on a JSONPath that does not exist because the
query errored produces a confusing "expected OPEN, got null" instead of "the
query failed: field `pe_tasks` not found".

Comparison is by JSON value with numeric normalisation, so `1` matches `1.0` and
a JSON string `"1"` does not match the number `1`. **[SDD decision]** Strict
type matching, because loose matching hides exactly the class of bug — an ID
serialised as a string — that a simulation harness exists to catch.

### 6.6 Closed-loop templating

A WireMock webhook body cannot be templated against the mock's *response*:

```
{{ response.body '$.data.editorialBoard_invitation.id' }}
```

**This cannot be assumed to work.** WireMock's webhook templating model exposes
the triggering request as `originalRequest`; access to the generated response
body from a webhook template is not a documented part of that model. Designing
the closed loop on it risks the demo's most compelling moment failing on a
detail nobody checked.

The design therefore does not depend on it:

- **Primary.** Correlation identifiers come from `originalRequest`, which is
  documented and sufficient in this domain, because the app's query carries the
  `id` and `spaceId` that the resulting event needs:

```json
{
  "name": "publish-invitation-accepted",
  "parameters": {
    "method": "POST",
    "url": "http://event-trigger:8080/publish",
    "headers": { "Content-Type": "application/json", "X-Citadel-Run": "{{originalRequest.headers.X-Citadel-Run}}" },
    "body": "{\"delivery\":\"amqp\",\"exchange\":\"public.srv-review-invitations.editor-invitation.v1\",\"routingKey\":\"editor-invitation-accepted\",\"event\":{\"type\":\"EditorInvitationAccepted\",\"source\":\"SRV-REVIEW-INVITATIONS\",\"version\":\"v1\",\"subject\":\"/articles/{{jsonPath originalRequest.body '$.variables.id'}}\",\"data\":{\"articleId\":{{jsonPath originalRequest.body '$.variables.id'}}}}}"
  }
}
```

- **Fallback if an identifier genuinely originates in the response.** The stub's
  response and the webhook body are written together in the same stub file, so a
  value invented by the mock is a constant the author already knows. It goes in
  both places. This is less elegant and entirely reliable.

**Verification task:** establish whether WireMock exposes the response body to
webhook templates in the pinned version. If it does, the primary form gains an
option; nothing has to change if it does not. Recorded in §16.

---

## 7. Messaging topology

### 7.1 Ownership

| Object | Declared by | Why |
|--|--|--|
| App's consuming queues, exchanges, bindings | The application, at startup, as in production | Duplicating them is the drift |
| **Outbound exchanges the app publishes to** | **Citadel, by the capture service** | The app writes outbox rows; Debezium publishes. Nothing in a Citadel environment would otherwise declare them (§10.1) |
| Spy queues and their bindings | Citadel, at runtime by the capture service | Bindings need the exchange to exist first (§7.3) |
| `citadel.unroutable` and its exchange | Citadel, via `definitions.json` | Ditto |
| `citadel.lock` | Citadel, at runtime, exclusive | Its lifetime is a connection |
| Alternate-exchange association | Citadel, via **policy** (§7.4) | Cannot be a declaration — see below |

### 7.2 Naming

Spy queues are `citadel.spy.<exchange>`, with dots in the exchange preserved:
`citadel.spy.public.orchestration-engine.Article.v1`. Bound with routing key `#`.

**[SDD decision]** Not prefixed with the environment name, because this design gives
each environment its own broker, so there is nothing to collide with. Should
that change, the prefix is a one-line change in one function
(`internal/amqp/names.go`), which is the whole reason the naming lives in a
function rather than in a string literal at each use.

### 7.3 `definitions.json`

Loaded by the RabbitMQ container at startup. Contains only Citadel-owned
objects.

```json
{
  "rabbit_version": "3.13.0",
  "exchanges": [
    { "name": "citadel.unroutable.ae", "vhost": "/", "type": "fanout", "durable": true, "auto_delete": false, "internal": false, "arguments": {} }
  ],
  "queues": [
    { "name": "citadel.unroutable", "vhost": "/", "durable": true, "auto_delete": false, "arguments": {} },
    { "name": "citadel.spy.public.orchestration-engine.Article.v1", "vhost": "/", "durable": true, "auto_delete": false, "arguments": { "x-max-length": 1000, "x-overflow": "drop-head" } }
  ],
  "bindings": [
    { "source": "citadel.unroutable.ae", "vhost": "/", "destination": "citadel.unroutable", "destination_type": "queue", "routing_key": "", "arguments": {} }
  ]
}
```

The spy queue's binding to the app's exchange is **not** here, because the
exchange does not exist until the app declares it and importing a binding to a
missing exchange fails the whole import. The capture service creates its
bindings at runtime, retrying until the exchange appears (§8.1). `x-max-length`
with `drop-head` bounds a spy queue so a runaway loop cannot exhaust the broker.

### 7.4 Alternate exchange

An obvious approach is an alternate exchange to catch unroutable messages.
**An alternate exchange is an argument on the exchange being published to**, and
Citadel does not own those exchanges — the app declares them. Declaring the same
exchange with different arguments fails with `PRECONDITION_FAILED` (406), and
whichever of the app or Citadel declares second dies at startup. Implemented as
written, this mechanism breaks the environment.

It is implemented as a **policy** instead, applied through the management API
after the broker starts:

```
PUT /api/policies/%2F/citadel-alternate-exchange
{
  "pattern": "^(public|private)\\.",
  "apply-to": "exchanges",
  "priority": 0,
  "definition": { "alternate-exchange": "citadel.unroutable.ae" }
}
```

A policy attaches the behaviour to matching exchanges without participating in
their declaration, so there is no argument to mismatch and no ordering
constraint. The pattern deliberately excludes `citadel.*` so the unroutable
exchange cannot feed itself.

This is applied by a small init step in both environments: a `curl` in the
compose stack's rabbitmq init container, and a Helm `post-install` hook Job. It
is idempotent.

### 7.5 Publish semantics

The event trigger publishes with:

- **Publisher confirms** — waits for the broker's ack before returning.
- **`mandatory=true`** — an unroutable message comes back as `basic.return`
  before the confirm, and is surfaced as HTTP 422 with the exchange and routing
  key.
- `deliveryMode=2` (persistent) unless the request overrides it.

The `basic.return` and the confirm race, so the implementation must register the
return handler before publishing and wait for both signals with a bounded
timeout (`publishConfirmTimeout`, default 5 s) rather than assuming order. A
timeout is a 502 with "broker did not confirm within Ns", never a silent success.

Debezium's sink cannot do any of this (§10.1); for Debezium-sourced messages
the only evidence is spy-queue arrival, and the design does not pretend
otherwise.

---

## 8. Event capture service

### 8.1 Binding

At startup, for each exchange in `capture.exchanges`:

1. Declare the exchange `type=topic, durable=true, auto_delete=false`. Citadel
   owns this declaration for outbound exchanges because nothing else in the
   environment does (§10.1). The declare is idempotent when the arguments match
   and fails loudly with `PRECONDITION_FAILED` when they do not.
2. Declare the spy queue.
3. Bind the queue to the exchange with routing key `#`.

For an exchange the *app* declares — an inbound one, where the app is the
consumer — the declare is passive, and if the exchange does not yet exist the
service retries every 2 s for `bindTimeout` (default 120 s), logging once per 10
attempts, because the app is usually still starting. Which mode applies is
config, not inference: `capture.exchanges` entries may be written as
`{ name: ..., declare: own|passive }`, defaulting to `own`.

`GET /health` reports each exchange as `bound`, `pending` or `failed`. Preflight
(§4.10) treats anything but `bound` as not ready, so a scenario cannot start
while a spy queue is unbound — which would otherwise produce a scenario that
fails an outbound assertion for reasons unrelated to the app.

### 8.2 Storage and query

An in-memory ring buffer per exchange, default 1000 messages, oldest dropped.
Each entry:

```json
{
  "seq": 412,
  "receivedAt": "2026-08-19T09:00:12.500Z",
  "exchange": "public.orchestration-engine.Article.v1",
  "routingKey": "public.orchestration-engine.Article.v1",
  "headers": { "eventType": "EditorAssigned", "aggregateType": "public.orchestration-engine.Article.v1", "aggregateId": "515150" },
  "runId": "0191...",
  "cloudEvents": { "valid": true, "violations": [] },
  "body": { }
}
```

Note that `routingKey` equals `exchange` for everything the app publishes
(§10.1). Filtering outbound events by routing key is therefore useless; filter on
`eventType`, which the outbox router places in a header.

```
GET    /captured?exchange=&routingKey=&routingKeyPattern=&eventType=&since=&runId=&jsonPath=&header.X=
GET    /captured/count?<same filters>
DELETE /captured
GET    /health
```

`eventType` resolves against the `eventType` **header** first and falls back to
the envelope's CloudEvents `type`. The header is populated by the outbox router's
`table.fields.additional.placement` (§10.1) and survives a malformed payload,
which is when it matters most.

`routingKeyPattern` uses AMQP topic semantics (`*` one word, `#` zero or more) —
**[SDD decision]** not glob, because the audience already knows topic patterns
from writing bindings, and having `*` mean two different things in one system is
a trap. An intuitive selector such as `"*Rejected*"` is not a valid topic
pattern, and the reference pack writes `eventTypePattern: "*Rejected*"`, a separate
glob-matched filter on the event type, which is what that example actually meant
— and which is the only form that works, since outbound routing keys carry no
event name.

`runId` filtering is applied by default from the CLI (§4.3).

### 8.3 Negative assertions

`event/not-captured` is irreducible because "no matching message arrived" over a
window is not a poll that can pass early. Semantics:

1. Query for matches **since `runStart`**, not since the assertion began. A
   message that arrived at step 2 must fail an assertion at step 9; anything else
   would let a scenario pass by asserting late.
2. If any match exists, fail immediately, reporting the matched message.
3. Otherwise sleep `quietPeriod` (default from config, overridable per step) and
   repeat the query once.
4. Pass only if both queries found nothing.

**A selector is mandatory.** `citadel validate` rejects an `event/not-captured`
with no `exchange`, `routingKey`, `routingKeyPattern`, `eventType`,
`eventTypePattern`, `jsonPath` or header filter. This is the direct answer to the
most common defect in a negative assertion: a selector that is absent is a
validation error, and
a selector that is present is applied by the capture service's own query engine
where it cannot be silently dropped. The assertion reports which selector it
applied, so the run output shows what was actually checked.

### 8.4 Inbox assertions

The event standard requires consumers to insert every delivered message into an inbox table
before ack. `inbox/assert` is a `sql/query` with a canonical query built from the
target's `inbox` config:

```sql
SELECT message_id, processed_at, retry_count, last_error
FROM   inbox
WHERE  message_id = $1
```

```yaml
- name: The redelivery was deduplicated
  type: inbox/assert
  target: app-db
  where:  { messageId: "{{ .accepted.id }}" }
  expect: { rows: 1, notNull: [processed_at], columns: { retry_count: 0 } }
```

Column names are configurable per target because the standard names the concepts
but each service names its columns. Defaults follow the common Debezium outbox
reference schema. `citadel doctor` verifies the table and columns exist
and reports the exact `ALTER`-free mismatch if not, since a wrong column name
otherwise surfaces as a SQL error mid-scenario.

---

## 9. Identity

Client-credentials against the real shared Hydra.

- The token is acquired once per run, in phase 6, before reset. Failing there
  gives "Hydra client credentials missing or rejected" as exit 3 rather than
  401s mid-scenario.
- Cached in memory for the run, refreshed at 60 s before expiry.
- Applied only to targets whose config names an `auth` block. The trigger and
  capture services are unauthenticated and inside the environment (§3.3).
- `citadel doctor` performs a token acquisition and reports the granted scopes,
  because a token with the wrong scopes fails later and less legibly than one
  that fails to issue.

The offline consequence holds: no Hydra, no Citadel. `doctor` says
so explicitly rather than reporting a generic connection error.

---

## 10. Change data capture

### 10.1 Outbound — the outbox router

One Debezium Server instance, one connector, against the app's Postgres. Real in
every environment; not simulation.

**This configuration is derived from a production deployment, not composed from
the documentation.** An earlier draft of this
section was written from the Debezium reference and got three things wrong; each
is called out below, because they are the kind of error that produces a stack
that starts cleanly and behaves differently from production.

#### The outbox table

Confirmed from the app's EF Core configuration. It matches the EventRouter's
expected column names exactly, which is why the transform needs no field
remapping beyond the timestamp.

| Column | Type | Note |
|--|--|--|
| `id` | uuid, PK | |
| `aggregatetype` | varchar(255) | Holds the **full exchange name**, e.g. `public.orchestration-engine.Article.v1` |
| `aggregateid` | varchar(255) | |
| `type` | varchar(255) | |
| `payload` | **jsonb** | The CloudEvents envelope |
| `created_at` | timestamptz, default `now()` | Mapped via `table.fields.timestamp` |

`payload` must be `jsonb`. The reference outbox schema illustrates the column as
`varchar(255)`, which truncates an envelope, so an application that copies it
carries that bug into every event it publishes.

#### Routing, resolved

The chain from outbox row to exchange, which nothing in the earlier draft got
right:

1. EventRouter routes by `aggregatetype`, whose value is already a full standard
   exchange name such as `public.orchestration-engine.Article.v1`.
2. `route.topic.regex` = `^(public|private)\.orchestration-engine\.([^.]+\.[^.]+)(?:\.(.*))?$`
   and `route.topic.replacement` = `$1.orchestration-engine.$2`. The third group
   is captured and discarded, so the rule's only effect is to **truncate any
   suffix beyond `<Entity>.<version>`**. A two-segment value passes through
   unchanged.
3. `debezium.sink.rabbitmq.exchange` is **not set**, and its documented default
   is the topic name. So the exchange is the routed topic.
4. `routingKey.source=topic` means, per the Debezium documentation, "the routing
   key is the same as the exchange name."

**Therefore exchange and routing key are the same string**, both equal to the
`aggregatetype`: `public.orchestration-engine.Article.v1`.

This is the first correction. The earlier draft set a *static*
`debezium.sink.rabbitmq.exchange`, which would have funnelled every entity's
events into one exchange, and it assumed short routing keys like
`editor-assigned`. Scenario assertions must expect the full dotted name as the
routing key, and the reference pack is written that way.

#### Headers

`table.fields.additional.placement` promotes three columns to headers:

| Header | From |
|--|--|
| `eventType` | `type` |
| `aggregateType` | `aggregatetype` |
| `aggregateId` | `aggregateid` |

The capture service's `eventType` filter therefore reads the **header** first and
falls back to the envelope's CloudEvents `type` (§8.2). Filtering on a header is
cheaper and, more importantly, works even when a payload is malformed — which is
exactly when you most want to find the message.

**These three headers arrive JSON encoded**, because `DEBEZIUM_FORMAT_HEADER=json`
below applies to every header value. The bytes on the wire for `eventType` are a
quote, the name, and a quote. The capture service decodes a header value that is a
JSON string, so a selector compares the value and not its encoding. Without that
decode, every `eventType` selector fails against every event Debezium produces, and
the failure reads as a missing event rather than as a mismatched encoding.

#### Configuration

Supplied as **environment variables**, matching production's mechanism and
naming so the compose file can be diffed against the devops template.

```properties
DEBEZIUM_SOURCE_CONNECTOR_CLASS=io.debezium.connector.postgresql.PostgresConnector
DEBEZIUM_SOURCE_PLUGIN_NAME=pgoutput
DEBEZIUM_SOURCE_SLOT_NAME=pe_outbox_slot
DEBEZIUM_SOURCE_PUBLICATION_AUTOCREATE_MODE=filtered
DEBEZIUM_SOURCE_SCHEMA_INCLUDE_LIST=public
DEBEZIUM_SOURCE_TABLE_INCLUDE_LIST=public.outbox
DEBEZIUM_SOURCE_TOPIC_PREFIX=orchestration-engine
DEBEZIUM_SOURCE_SNAPSHOT_MODE=when_needed

DEBEZIUM_TRANSFORMS=outbox
DEBEZIUM_TRANSFORMS_OUTBOX_TYPE=io.debezium.transforms.outbox.EventRouter
DEBEZIUM_TRANSFORMS_OUTBOX_TABLE_FIELDS_TIMESTAMP=created_at
DEBEZIUM_TRANSFORMS_OUTBOX_TABLE_EXPAND_JSON_PAYLOAD=true
DEBEZIUM_TRANSFORMS_OUTBOX_ROUTE_TOPIC_REGEX=^(public|private)\.orchestration-engine\.([^.]+\.[^.]+)(?:\.(.*))?$
DEBEZIUM_TRANSFORMS_OUTBOX_ROUTE_TOPIC_REPLACEMENT=$1.orchestration-engine.$2
DEBEZIUM_TRANSFORMS_OUTBOX_TABLE_FIELDS_ADDITIONAL_PLACEMENT=type:header:eventType,aggregatetype:header:aggregateType,aggregateid:header:aggregateId

DEBEZIUM_SINK_TYPE=rabbitmq
DEBEZIUM_SINK_RABBITMQ_CONNECTION_HOST=rabbitmq
DEBEZIUM_SINK_RABBITMQ_CONNECTION_PORT=5672
DEBEZIUM_SINK_RABBITMQ_CONNECTION_USERNAME=${RABBITMQ_USER}
DEBEZIUM_SINK_RABBITMQ_CONNECTION_PASSWORD=${RABBITMQ_PASSWORD}
DEBEZIUM_SINK_RABBITMQ_AUTOCREATEROUTINGKEY=true
DEBEZIUM_SINK_RABBITMQ_ROUTINGKEY_SOURCE=topic

DEBEZIUM_FORMAT_KEY=json
DEBEZIUM_FORMAT_KEY_SCHEMAS_ENABLE=false
DEBEZIUM_FORMAT_VALUE=json
DEBEZIUM_FORMAT_VALUE_SCHEMAS_ENABLE=false
DEBEZIUM_FORMAT_HEADER=json
DEBEZIUM_FORMAT_HEADER_SCHEMAS_ENABLE=false
```

`SCHEMAS_ENABLE=false` with `expand.json.payload=true` means the message body is
the CloudEvents envelope the app wrote, with no Connect schema wrapper. That is
what makes the conformance check of §4.11 a direct validation of the app's own
output rather than of Debezium's framing.

#### Deviations from production, and why each is safe

Every difference is listed. A deviation not on this list is a bug.

| Setting | Production | Citadel | Why |
|--|--|--|--|
| Image tag | `3.2.0.Final` | **`3.2.0.Final`** | Match production exactly. See below |
| Offset storage | `JdbcOffsetBackingStore` in the app DB | `MemoryOffsetBackingStore`, flush interval 0 | Clean slate per container start. Offset storage affects restart behaviour only, never message shape or routing |
| `SLOT_DROP_ON_STOP` | `false` | `true` | Ephemeral environments should not leave slots behind. Reduces, but does not remove, the §4.9 caveat |
| Heartbeat | 5000 ms with an action query | **disabled** | See below — three separate problems |
| TLS, port, vhost | 5671, TLS on, named vhost | 5672, no TLS, default vhost | Each environment gets its own broker. Certificate handling is not part of what these scenarios test |
| DB `sslmode` | `require` | `disable` | Local Postgres |

**The image tag is the significant one, and it reverses an earlier instruction.**
The earlier draft pinned ≥ 3.6.1.Final to avoid a bug in which camelCase sink
properties passed as environment variables are silently ignored. Production runs
**3.2.0.Final**, which predates that bug entirely — the production config proves
env-var configuration works there. Pinning Citadel to a *newer* Debezium than
production would mean the local outbound pipeline is not the one being
simulated, which defeats the purpose of running it for real. So: **pin whatever
the app's devops template pins, currently 3.2.0.Final**, and treat the 3.6.0
defect as a hazard for the day the app upgrades, recorded here so the upgrade
skips 3.6.0.

**The heartbeat is disabled, and this is the most consequential correction.**
Production sets a 5-second heartbeat whose action query upserts a fixed-id row
into the outbox with `ON CONFLICT (id) DO UPDATE`. In a Citadel environment that
one setting breaks three things at once:

1. **It would kill the connector.** After the first insert, every subsequent
   heartbeat is an `UPDATE` to an outbox row. The earlier draft set
   `table.op.invalid.behavior=fatal`, under which the connector dies roughly five
   seconds after start, every single time. Production relies on the default
   `warn`, which skips the update and logs.
2. **It would fail every run's conformance check.** The heartbeat's payload is
   `{}`, which is not a valid CloudEvents envelope, so §4.11's rule that a
   malformed captured event fails the run would fail every run for a reason
   unrelated to the app.
3. **It is noise.** Twelve messages a minute into a spy queue makes a captured
   event log much harder to read.

The heartbeat exists to advance a replication slot on a low-traffic production
database. A Citadel environment is short-lived, uses in-memory offsets, and has
a scenario driving traffic through it, so the problem it solves does not exist
here. `DEBEZIUM_SOURCE_HEARTBEAT_INTERVAL_MS=0`.

**`table.op.invalid.behavior` stays at the production default (`warn`).** With
the heartbeat off, `fatal` would be safe today, but any future outbox cleanup job
issuing a `DELETE` would then take the whole environment down mid-demo. The
diagnostic value that `fatal` was there to provide is recovered without the
fragility: the end-of-run checks scan the connector log for skipped-operation
warnings within the run window and report them as a failure (§4.11). Same signal,
no shared failure mode with the system under test.

**`autoCreateRoutingKey=true` is kept for parity, with a note.** It declares a
durable *queue* named after the routing key — and since the routing key equals
the exchange name, the result is a queue with the same name as the exchange,
bound to nothing, permanently empty. It is not a binding and it does not deliver
anything. That is the misunderstanding this setting invites. Citadel
keeps the setting so the topology matches production, and `citadel doctor`
identifies these queues explicitly so nobody spends an afternoon wondering why
the queue that looks right is empty.

#### Who declares the outbound exchange

Nobody, unless Citadel does — and this is the third correction.

The application declares its own topology. That is true of the
queues and exchanges it *consumes* from, but the app does not declare its
outbound exchanges: it writes outbox rows, and Debezium publishes. In production
the exchange exists because consumers and infrastructure declare it. **In a
Citadel environment there are no other consumers**, so if Citadel does not
declare it, Debezium publishes to a non-existent exchange, which raises
`NOT_FOUND` and closes the channel.

The capture service therefore declares each configured outbound exchange as
`type=topic, durable=true, auto_delete=false` before binding its spy queue
(§8.1). If the app or anything else declares the same exchange with different
arguments, the declare fails loudly with `PRECONDITION_FAILED`, which is the
outcome we want — a topology disagreement should be an error at startup, not a
mystery at assertion time.

### 10.2 Inbound — the upstream shadow

Behind the `cdc-postgres` profile, off by default. A Postgres instance holding
only the watched tables, and a second Debezium Server (one connector per
process).

```
POSTGRES_INITDB_ARGS: "-c wal_level=logical"
```

set through a command override rather than a custom image, and
`REPLICA IDENTITY FULL` on any table whose `before` image a scenario asserts on.

Shadows are Postgres regardless of what the real upstream runs. SQL
Server exists behind the `cdc-sqlserver` profile for teams whose *application*
database is SQL Server, pinned to `2025-CU1-ubuntu-24.04`, with the Agent
enabled and the capture-job poll interval reduced — including the stop and start
without which the change is inert.

---

## 11. Deployment

### 11.1 Compose

`deploy/compose/docker-compose.yml` plus profile overlays. `docker compose up`
brings up the default profile and nothing else.

Rules the file must follow, each of which corresponds to a first-run failure we
are choosing not to have:

- Every service has a `healthcheck`, and every dependency uses
  `depends_on: { condition: service_healthy }`. No sleeps, no restart loops as
  a synchronisation mechanism.
- Ports publish to `127.0.0.1:` explicitly (§3.3).
- Images pinned by tag **and** digest.
- No bind-mounted shell scripts. Initialisation that cannot be expressed as an
  image's own entrypoint runs as a short-lived init service, so CRLF line
  endings on Windows cannot break startup. `.gitattributes` forces LF on `*.sql`
  and `*.sh` as a second line of defence.
- Named volumes only for the app database. Everything else is ephemeral, so
  `docker compose down` is a true reset for all but the one case §4.9 warns
  about.

Profiles: default, `outbox`, `cdc-postgres`, `cdc-sqlserver`.

### 11.2 Helm

One chart, `deploy/helm/citadel-stack`, into the shared cluster's `default`
namespace.

- Every object named `{{ .Release.Name }}-<component>` and labelled
  `app.kubernetes.io/part-of: citadel` plus `citadel.io/owner`.
- `emptyDir` only. No PVCs, so no StorageClass dependency.
- Requests and limits on every container.
- No Ingress, and no value that creates one.
- `runAsNonRoot`, `readOnlyRootFilesystem`, dropped capabilities where the image
  permits, so the chart installs under a restricted Pod Security Admission
  profile.
- The RabbitMQ policy of §7.4 applied by a `post-install,post-upgrade` hook Job.
- `imagePullSecret` for a private registry.

The same scenario files run against it with the cluster `citadel.yaml` and
`citadel forward`.

---

## 12. Diagnostics

### 12.1 Bundle

Written to `<bundle-dir>/<runId>/` on any non-zero exit, from the `after` phase
so a crashed run still produces one.

```
0191c3.../
├── run.json               Outcome, timings, resolved config with secrets redacted
├── scenario.yaml          The scenario as loaded, with variables resolved
├── steps.jsonl            One record per step: inputs, outputs, attempts, timings
├── wiremock/
│   ├── unmatched.json     GET /__admin/requests/unmatched
│   ├── near-misses.json   GET /__admin/requests/unmatched/near-misses
│   └── journal.json       Requests within the run window only
├── rabbitmq/
│   ├── queues.json        Depths at run end, including citadel.unroutable
│   └── topology.json      Exchanges and bindings as they actually were
├── captured.jsonl         Every message captured during the run
├── debezium/
│   └── connectors.json    Connector state and last error
└── app/
    └── logs.txt           Last N lines, best effort
```

Near-misses are the highest-value file in the bundle and the reason the bundle
exists: "my stub didn't match and I can't see why" is the most common failure in
a system like this, and near-misses turn it into a diff.

`citadel logs --last-failure` prints a summary and the path. Bundles older than
`bundleRetention` (default 7 days) are pruned on write.

### 12.2 Redaction

Applied to `run.json`, `steps.jsonl` and `journal.json` before writing: any
value whose key matches `(?i)(secret|password|token|authorization|dsn)` is
replaced with `«redacted»`, and any string matching a known bearer-token shape
is replaced wherever it appears. A bundle is something a developer will paste
into a chat window, so this is a correctness requirement, not a nicety.

### 12.3 `citadel doctor`

Every check reports pass, fail, or skipped-because-not-configured, with a
remediation sentence on failure. The value is entirely in the remediation text.

| Check | Failure message says |
|--|--|
| Config parses, all `${VAR}` resolved | Which variable is unset |
| Every target reachable | Which one, and the address tried |
| Hydra token acquires | Whether credentials are missing or rejected, and the granted scopes |
| Spy queues bound | Which exchange is missing, and that the app may still be starting |
| Alternate-exchange policy present | The `PUT` to apply it |
| Debezium connector `RUNNING` | Connector state and last error verbatim |
| Debezium image tag matches the app's devops template | Which versions differ, and that the local pipeline is not the one production runs |
| Heartbeat disabled | That the action query will kill or pollute the run (§10.1) |
| `routingKey.source` set, `routingKeyFromTopicName` absent | That every message will carry an empty routing key |
| Queues whose name equals an exchange name | That these are `autoCreateRoutingKey` artifacts, bound to nothing and permanently empty, and not the queue you are looking for |
| Inbox table and columns exist | The expected names against the actual ones |
| Postgres `wal_level=logical` | The command override to set it |
| No stale replication slot | The `down -v` needed for a true reset |
| SQL Agent running (SQL Server only) | That CDC will never emit without it |
| Capture-job poll interval applied | That the value was recorded and ignored without a job restart |
| App readiness endpoint responds | The endpoint tried |

---

## 13. `citadel validate`

Runs with no environment. Everything here is a failure that would otherwise cost
a developer a confusing run.

1. Scenario parses; no unknown keys at any level.
2. `type` is in the closed set (§6.2); required fields for that type present.
3. Every referenced file exists: `dataFile`, `queryFile`, `file`, `bodyFile`.
4. Every `{{ .var }}` resolves from `variables` or a prior `as:` binding.
5. Every `as:` name is unique; every `ref:` names a prior `as:`.
6. **No assertion or probe targets WireMock** — checked against the
   config's resolved target URLs, not by name, so aliasing cannot defeat it.
7. Every `event/not-captured` carries at least one selector (§8.3).
8. Every WireMock stub file with a webhook `postServeAction` also carries
   `newScenarioState` (the closed-loop guard of §6.6).
9. Every `wiremock/set-state` names a scenario and state that appear in some stub
   file, matched case-sensitively.
10. Every `wait` carries a `reason`.
11. Payload files are valid JSON and validate against their AsyncAPI schema when
    one is configured.
12. No literal secret in the config (§5.3).
13. GraphQL query files parse, and validate against a schema file when one is
    configured.

Checks 6, 7 and 8 are the ones that prevent a *passing* run from being
meaningless, which makes them the most important thing this command does.

---

## 14. Testing Citadel itself

### 14.1 Unit

Template rendering, expectation evaluation, config resolution, topic-pattern
matching, CloudEvents validation, redaction. Table-driven, no I/O.

### 14.2 Integration

Testcontainers for RabbitMQ, Postgres and WireMock. Covers, at minimum, the
failure modes we claim to detect, because a diagnostic that has never fired is a
diagnostic that does not work:

- Publish to an exchange with no binding → 422 with exchange and routing key.
- Publish to a bound exchange → confirmed, captured, run passes.
- `event/not-captured` with a message present before the assertion → fails.
- `event/not-captured` with a matching message arriving during the quiet period
  → fails.
- Lock held by a second process → exit 5.
- Reset leaves every queue at depth 0 and the buffer empty.
- A malformed envelope fails the run even with no assertion on it.
- A step that cannot reach its target exits 3, not 1.

### 14.3 Constraint tests

- **C3.** A test that fails the build if any address-bearing configuration has a
  compiled-in default. Implemented as a check over the `config` package's
  reflected struct tags plus a source scan for `localhost`, `127.0.0.1` and
  literal ports outside test files and example configs.
- **C2.** The full reference scenario runs with stdin closed, no TTY, and
  `TERM=dumb`, and produces valid JSON output, a parseable JUnit file, and the
  documented exit code.
- **C4.** A test asserting the step-type registry matches the documented closed
  set exactly, so adding a type without documenting it fails.

### 14.4 The reference scenario as an acceptance test

`examples/reference` runs ten consecutive times in the integration suite
against the compose stack, and the suite fails on any flake. Repeatability is a
test here rather than a claim.
