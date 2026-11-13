# Opticore

A minimal CWMP (TR-069) ACS daemon for FTTH fleets, written in Go. One binary,
one PostgreSQL database, an HTTP API. It speaks the protocol, keeps the device
state and runs the RPCs you ask for; it holds no provisioning logic of its own.

- Wire behaviour, faults and session handling match GenieACS, so it can replace
  it without changing the ONT side.
- No embedded library, no plugin system: clients are HTTP/JSON.
- Failures are visible: every task keeps its attempts, fault code, previous
  values and result, and is streamed over SSE as it changes.

## Why it exists

The job of an ACS is narrow: accept an Inform, keep a session alive, exchange
RPCs, wake devices, and remember what happened. GenieACS does that, and also
does provisioning — presets that match devices and run JavaScript fragments, a
MongoDB data store, its own config language. For a fleet whose subscriber
configuration already lives in a subscriber system, that puts the same
decisions in two places: the ACS grows rules nobody can test in isolation, and
reading or changing a device means going through the ACS's data model instead
of yours.

Opticore splits it: the ACS is the transport and the ledger, your application
is the brain. The ACS keeps devices, sessions, the task queue with every
attempt, fault and previous value, wake-up and events. Your application decides
what to configure, and says so as tasks — over HTTP, or through the inform hook
when a device comes online.

What that buys:

- **No hidden writes.** Opticore writes exactly one thing on its own: its
  connection-request credentials. Everything else is a task someone asked for.
- **Your data, your queries.** State is plain PostgreSQL, so subscriber joins,
  audits and reports run where the rest of your data already is.
- **Every change is a row.** A task carries its args, its previous values, its
  result and its fault, and can be inspected or cancelled like any other record.
- **Boring operations.** One static binary, migrations on start, JSON logs, no
  message broker, no external config service.
- **Provisioning stays testable.** The hook is HTTP, so the rules can be unit
  tested, replayed and versioned outside the ACS, in whatever language you
  already use.

It is deliberately not a device manager UI, not a template engine, not an
inventory, and not a ZTP server. Those belong on top.

## Requirements

- Go 1.26 or newer to build.
- PostgreSQL. Tested against 18.
- The CWMP listener must be reachable by the devices, and it should sit on an
  isolated management VLAN (see [Security](#security)).

## Quick start

```sh
# minimal configuration
cat > .env <<'ENV'
OPTICORE_DB_DSN=postgres://postgres:secret@localhost:5432/opticore?sslmode=disable
OPTICORE_SECRET_KEY=$(openssl rand -base64 32)
CWMP_ADDR=:7547
OPTICORE_API_ADDR=:8081
ENV

chmod 600 .env
set -a; . ./.env; set +a

make build
./bin/opticore migrate        # schema; `serve` also does this on start
./bin/opticore serve
```

Then create an API token and point a device at the ACS:

```sh
./bin/opticore token create ops --scope read,write   # printed once
curl -H 'Authorization: Bearer <token>' localhost:8081/v1/devices
```

Give the ONTs `http://<this host>:7547/` as their ACS URL. The first Inform
creates the device row; Opticore then writes its own connection-request
credentials so it can wake the device later.

## Configuration

Every setting is an environment variable; the daemon reads nothing else.
Values in parentheses are defaults.

### Required

| Variable | Meaning |
|---|---|
| `OPTICORE_DB_DSN` | PostgreSQL DSN, e.g. `postgres://user:pass@host:5432/opticore?sslmode=disable` |
| `OPTICORE_SECRET_KEY` | base64 of 32 bytes, AES-256-GCM key for secrets at rest. Losing it makes stored secrets unrecoverable |

### Device-facing listener

| Variable | Default | Meaning |
|---|---|---|
| `CWMP_ADDR` | `:7547` | listener address; bind the management-VLAN address where possible |
| `CWMP_GPV_BATCH` | `32` | parameter names per `GetParameterValues` |
| `CWMP_SESSION_TIMEOUT` | `30s` | idle limit between requests, also the session-lock TTL |
| `CWMP_CR_TIMEOUT` | `2s` | connection-request timeout |
| `CWMP_REQUEST_TIMEOUT` | `10s` | per-request read/write limit |
| `CWMP_MAX_BODY` | `10MiB` | largest accepted body, after decompression |
| `CWMP_MAX_SESSIONS` | `1000` | concurrent sessions before `503` + `Retry-After` |
| `CWMP_ALLOW_CIDRS` | any | comma list of CIDRs or IPs allowed to reach the listener |
| `CWMP_CR_URL_MATCH_SOURCE` | `true` | drop a `ConnectionRequestURL` whose host is not the Inform's source IP |
| `CWMP_AUTH_USERNAME` | off | device-to-ACS username; set with the password or neither |
| `CWMP_AUTH_PASSWORD` | off | device-to-ACS password |

### HTTP API

| Variable | Default | Meaning |
|---|---|---|
| `OPTICORE_API_ADDR` | `:8081` | API listener |
| `OPTICORE_API_AUTH` | `true` | require a bearer token on `/v1`; `false` disables API auth entirely |
| `OPTICORE_API_INSECURE_LOCAL` | `false` | accept unauthenticated `/v1` from loopback only |
| `OPTICORE_API_REQUIRE_REVEAL` | `true` | `?reveal=1` needs a token with the `reveal` scope |

### Inform hook

| Variable | Default | Meaning |
|---|---|---|
| `OPTICORE_HOOK_URL` | off | `http://`, `https://` or `unix:/path/to.sock` |
| `OPTICORE_HOOK_SECRET` | — | required when the URL is set; sent as a bearer token |
| `OPTICORE_HOOK_EVENTS` | `0 BOOTSTRAP` | event codes that trigger the hook; a prefix matches |
| `OPTICORE_HOOK_PREFETCH` | — | parameter paths read before step 1 |
| `OPTICORE_HOOK_STEP_TIMEOUT` | `2s` | one provisioner call |
| `OPTICORE_HOOK_TIMEOUT` | `10s` | the whole hook, across steps |

### Tasks and retention

| Variable | Default | Meaning |
|---|---|---|
| `OPTICORE_TASK_EXPIRY` | `10m` | minimum lifetime of a task, from `run_at` |
| `OPTICORE_TASK_MAX_ATTEMPTS` | `5` | retries before a task is left failed |
| `OPTICORE_TASK_RETENTION` | `720h` | finished tasks older than this are deleted |
| `OPTICORE_EVENT_RETENTION` | `720h` | events older than this are deleted |
| `OPTICORE_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

`OPTICORE_FILE_ADDR` and `OPTICORE_FIRMWARE_DIR` are read but unused: firmware
transfer is not implemented yet.

## How a session works

1. The device POSTs an `Inform`. Opticore records the device, its parameters and
   the event, and answers.
2. If the device has no connection-request credentials, or sent `0 BOOTSTRAP`,
   Opticore sets fresh ones. Those two leaves are the only parameters it writes
   on its own. A set touching them directly is rejected.
3. If a hook is configured and the event matches, the hook runs (see below).
4. Queued tasks run in order. A batch is a series of tasks with one `batch_id`
   and a sequence number; when one fails, the rest of the batch is cancelled.
5. An empty reply ends the session.

Faults `9002` and `9004` are retried, but only for `get`, `set` and
`delete_object`; a retryable fault ends the session and the retry happens on
the next wake. Other faults fail the task. A non-idempotent task whose session
died mid-RPC is marked `unknown` rather than retried.

One session per device. A new Inform replaces an idle session; a busy one gets
`400`. Bootstrap replaces the parameter cache and clears `pending_reboot`.

Tasks expire at `max(2 × inform interval, OPTICORE_TASK_EXPIRY)` after
`run_at`, so a reboot scheduled for 02:00 stays valid until then, and a device
that stays away does not accumulate a queue forever.

## Tasks

`POST /v1/devices/{id}/tasks` takes one task or an array of them (an ordered
batch). `args` by name:

| Task | Args |
|---|---|
| `get` | `{"paths": ["InternetGatewayDevice.DeviceInfo."]}` — exact names or a subtree ending in `.` |
| `set` | `{"values": [["path", "value", "xsd:string"]]}` — leaves only; the type may be omitted |
| `add_object` | `{"path": "InternetGatewayDevice.X."}` — result carries the new instance number |
| `delete_object` | `{"path": "InternetGatewayDevice.X.3."}` |
| `reboot` | `{}` |
| `reset` | `{}` — factory reset |

A `set` reads the old values with a `GetParameterValues` in the same session
first and stores them in the task's `prev`, which is the raw material for a
rollback.

A leaf whose last segment ends in `password`, `passphrase`, `secret`, `psk` or
`key` (case-insensitive, `ParameterKey` excepted) is a secret: it is sealed
with `OPTICORE_SECRET_KEY` before it reaches the database, and is returned as
`••••` unless the request carries `?reveal=1` from a token with the `reveal`
scope.

Task statuses: `pending`, `running`, `done`, `failed`, `unknown`, `cancelled`,
`expired`.

## Inform hook

The daemon holds no provisioning rules. When an Inform matches
`OPTICORE_HOOK_EVENTS`, Opticore reads `OPTICORE_HOOK_PREFETCH` and then POSTs
to your provisioner, inside the live session:

```json
{
  "step": 1,
  "device": {"id": "00259E-HG8145V5-48575443BD7589AB", "serial": "48575443BD7589AB",
             "oui": "00259E", "product_class": "HG8145V5", "software_version": "V5R020C10S271"},
  "events": ["0 BOOTSTRAP", "1 BOOT"],
  "params": {"InternetGatewayDevice.ManagementServer.PeriodicInformInterval": {"value": "300", "type": "xsd:unsignedInt"}},
  "prefetch": {"InternetGatewayDevice.DeviceInfo.SoftwareVersion": {"value": "V5R020C10S271", "type": "xsd:string"}},
  "state": "opaque string you returned last time",
  "results": [{"name": "set", "status": "done", "result": {}}]
}
```

Reply with the tasks to run and an opaque `state`:

```json
{"tasks": [{"name": "set", "args": {"values": [["InternetGatewayDevice.X.SSID", "Home", "xsd:string"]]}}],
 "state": "any string"}
```

Opticore executes them as ordinary task rows (so SSE, `prev` and the task log
see them), then calls the next step with their results. An empty `tasks` array
ends the hook, and the session continues with anything queued by the API.

The hook stops on a non-2xx answer, a timeout, or 8 steps, and the session
continues without it; the Inform is still published over SSE, so a provisioner
that is down means "no extra work", never a failed session. Not calling the
hook at all is normal: with no `OPTICORE_HOOK_URL` the daemon only runs what
the API queues.

## HTTP API

Bearer tokens, hashed at rest, with scopes `read`, `write` and `reveal`.
`Idempotency-Key` on task creation is kept permanently and unique per device:
a retry returns the original rows instead of enqueueing again.

| Method and path | Purpose |
|---|---|
| `GET /v1/devices` | list; `serial_prefix`, `product_class`, `limit`, `cursor` |
| `GET /v1/devices/{id}` | one device; `?reveal=1` for secrets |
| `GET /v1/devices/{id}/params?prefix=P` | cached parameters under a prefix |
| `GET /v1/devices/{id}/tasks` | task log; `status`, `limit` |
| `GET /v1/devices/{id}/events` | events; `before`, `limit` |
| `GET /v1/tasks/{taskId}` | one task |
| `POST /v1/devices/{id}/tasks` | queue tasks; `{name,args,run_at,expires_at}` or an array |
| `DELETE /v1/devices/{id}/tasks/{taskId}` | cancel a pending task |
| `POST /v1/devices/{id}/wake` | connection request now |
| `DELETE /v1/devices/{id}` | forget a device; it re-registers if it informs again |
| `POST /v1/jobs` | bulk job |
| `GET /v1/jobs` | list jobs |
| `GET /v1/jobs/{id}` | progress and the per-device failure list |
| `DELETE /v1/jobs/{id}` | cancel: pending tasks become `cancelled`, running ones finish |
| `GET /v1/events` | SSE stream |
| `GET /healthz` | database reachability and the age of the newest Inform |

A device id is `OUI-ProductClass-Serial`, e.g.
`00259E-HG8145V5-48575443BD7589AB`; a bare serial is accepted everywhere a
device is addressed.

Lists answer `{"data": [...], "meta": {"next_cursor": "...", "total": n}}`.
Errors answer `{"error": {"code": "DEVICE_NOT_FOUND", "message": "...",
"details": {}}}`; codes include `VALIDATION` (400), `UNAUTHORIZED` (401),
`FORBIDDEN` (403), `DEVICE_NOT_FOUND` / `TASK_NOT_FOUND` / `JOB_NOT_FOUND`
(404), `CONFLICT` (409) and `DEVICE_UNREACHABLE` (502).

### Jobs

```json
POST /v1/jobs
{"name": "reboot-olt3",
 "task": {"name": "reboot", "args": {}},
 "select": {"serials": ["48575443BD7589AB"], "product_class": "HG8145V5"},
 "max_parallel": 10,
 "run_at": "2026-11-14T02:00:00+07:00"}
```

The selector accepts `serials`, `ids`, `product_class` and
`software_version`, combined as filters; `serials` must all exist. A job
expands to one task per device in a single transaction. The daemon wakes at
most `max_parallel` devices at a time and the rest run when they inform on
their own, so a mass reboot does not become an inform storm. A job is `done`
when no task of it is pending or running.

### SSE

`GET /v1/events` streams Inform events and task transitions. Filter with
`type=` and `device_id=`, and resume with `Last-Event-ID` (or
`?last_event_id=`). Ids are `e<events.id>` for events and
`t<task id>:<unix nano>` for task updates; each frame's data is the event or
task as JSON.

## CLI

```
opticore serve                                 run the daemon
opticore migrate                               apply migrations
opticore devices [--serial P] [--class C]      list devices
opticore device <serial|id> [--prefix P]       one device and its parameters
opticore tasks <serial|id>                     task log
opticore task <serial|id> get <path>...
opticore task <serial|id> set <path> <value> [type]
opticore task <serial|id> reboot
opticore jobs | job <id> | job cancel <id>
opticore wake <serial|id>
opticore token create <name> --scope read,write[,reveal]
opticore version
```

Every listing subcommand takes `--json`. The CLI connects to the database
directly and never applies DDL except `migrate`.

## Security

- **Single source of truth for the ACS URL.** Two ACSes pointed at one device
  overwrite each other's connection-request credentials and the wakes start
  failing with `401`. Keep one.
- **`CWMP_ALLOW_CIDRS`** rejects requests from outside the management subnets
  before the body is read.
- **`CWMP_CR_URL_MATCH_SOURCE`** drops a `ConnectionRequestURL` whose host is
  not the Inform's source IP, and gives that session no credentials, so a
  spoofed Inform cannot redirect wakes.
- **`CWMP_AUTH_USERNAME` / `CWMP_AUTH_PASSWORD`** make devices authenticate with
  HTTP Digest (MD5, `qop=auth`, stateless signed nonces) or Basic. Set the same
  pair in the ONTs' `ManagementServer.Username` / `Password`.
- **`OPTICORE_API_AUTH=false`** turns API auth off for every client, not just
  loopback: anyone who can reach the API listener can read devices, queue
  tasks and reveal secrets. The daemon logs a warning at startup. Use it only
  behind a network boundary or on a throwaway instance; prefer
  `OPTICORE_API_INSECURE_LOCAL=true` when only this host needs access.
- **Secrets** are AES-256-GCM encrypted at rest and masked unless revealed with
  the `reveal` scope. `OPTICORE_SECRET_KEY` is the one thing you must back up
  alongside the database.
- A refused wake clears the stored credentials, so the next Inform sets them
  again instead of leaving the device unreachable.

## Operations

- One daemon per database: it takes a PostgreSQL advisory lock at startup and
  refuses to run if another holds it. Keep it that way — sessions and wake
  coalescing live in process memory.
- `serve` applies migrations, then logs the schema version and the active
  device-security settings.
- The janitor runs every minute: it expires overdue tasks, and once an hour it
  deletes finished tasks and events past their retention. Pending and running
  tasks are never deleted.
- `GET /healthz` is served on both listeners.
- Logs are JSON on stderr. `OPTICORE_LOG_LEVEL=debug` adds the auth challenge
  and hook step detail.

## Development

```sh
make dev      # hot reload (air)
make build    # bin/opticore, version from git describe
make test     # unit tests
make itest    # everything, needs PostgreSQL and OPTICORE_TEST_DSN
make vet fmt lint
```

`make itest` needs `OPTICORE_TEST_DSN`; the database tests skip themselves
without it, and they run in a throwaway schema so they can share one database.
Locally, `make db-test` creates `opticore_test` if it is missing.

CI runs gofmt, vet, build, `go test -race` against a PostgreSQL service, and a
container build.

`make release major|minor|patch` tags the next `v*` version from the latest tag
and nothing else. Pushing that tag:

- builds static binaries for `linux`, `darwin` and `windows`, on `amd64` and
  `arm64`, with `-X main.version` set to the tag, plus `checksums.txt`;
- publishes them as a GitHub release;
- builds and pushes the multi-arch image
  `ghcr.io/<owner>/opticore:<tag>` and `:latest`.

## Container image

```sh
docker run --rm -p 7547:7547 -p 8081:8081 \
  -e OPTICORE_DB_DSN='postgres://user:pass@db:5432/opticore?sslmode=disable' \
  -e OPTICORE_SECRET_KEY="$(openssl rand -base64 32)" \
  ghcr.io/<owner>/opticore:latest
```

The image is distroless and runs as non-root, with `serve` as the command, so
migrations are applied on start. It carries CA certificates for an `https` hook
URL. Mount nothing; all configuration is environment variables. Port `8082` in
`EXPOSE` is reserved for the file server and is not served yet.

## Not implemented yet

- Rollback: per-task or per-job revert from `prev`.
- Firmware `download`, upload and the file server.
- `ping` (UDP `EchoRequest`).
- `ScheduleInform` / `ScheduleDownload`.
- SSE events for job transitions, and creating jobs from the CLI.
- A TUI.

## Layout

```
cmd/opticore/     daemon wiring, CLI
internal/cwmp/    HTTP transport, sessions, XML scanner, SOAP decode/write, auth
internal/acs/     session engine, task service, jobs, wake self-heal
internal/hook/    provisioner client, server-side handler, step payloads
internal/api/     /v1 handlers, SSE, tokens
internal/store/   pgx store, secrets, migrations runner
internal/wake/    connection-request client (Digest + Basic)
migrations/       embedded goose migrations
```
