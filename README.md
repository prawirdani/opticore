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

## Install

Three ways in; each gives one binary or one image. Commands below use
`opticore` for the binary you ended up with.

### Prebuilt binary

Each release carries static binaries for `linux`, `darwin` and `windows`, on
`amd64` and `arm64`, plus `checksums.txt`:

```sh
ver=v0.1.0   # the release you want
curl -LO "https://github.com/prawirdani/opticore/releases/download/$ver/opticore_${ver}_linux_amd64"
curl -LO "https://github.com/prawirdani/opticore/releases/download/$ver/checksums.txt"
sha256sum -c checksums.txt --ignore-missing
chmod +x "opticore_${ver}_linux_amd64"
sudo mv "opticore_${ver}_linux_amd64" /usr/local/bin/opticore
opticore version
```

Nothing else is needed at runtime: the binary is static, and migrations are
embedded.

### From source

```sh
git clone https://github.com/prawirdani/opticore
cd opticore
make build            # bin/opticore, version from git describe
./bin/opticore version
```

Go 1.26 or newer is the only build dependency.

### Container

```sh
docker run --rm -p 7547:7547 -p 8081:8081 \
  -e OPTICORE_DB_DSN='postgres://user:pass@db:5432/opticore?sslmode=disable' \
  -e OPTICORE_SECRET_KEY="$(openssl rand -base64 32)" \
  ghcr.io/prawirdani/opticore:latest
```

The image is distroless and runs as non-root, with `serve` as the command, so
migrations apply at start. It carries CA certificates for an `https` hook URL.
Mount nothing: all configuration is environment variables.

## First run

```sh
cat > .env <<ENV
OPTICORE_DB_DSN=postgres://postgres:secret@localhost:5432/opticore?sslmode=disable
OPTICORE_SECRET_KEY=$(openssl rand -base64 32)
CWMP_ADDR=:7547
OPTICORE_API_ADDR=:8081
ENV

chmod 600 .env
opticore migrate      # schema; `serve` also does this on start
opticore serve -d     # -d detaches and prints the pid; plain `serve` stays in the foreground
opticore stop         # graceful shutdown of the running daemon
```

`serve` writes a pid file, so `stop` finds the daemon without a pattern match:
`$OPTICORE_PID_FILE`, else `<OPTICORE_LOG_FILE>.pid`, else `opticore.pid` beside
the binary. `stop` also scans the process table, so a daemon started before the
file existed is still found. A second `serve` refuses to start while one is
running, which the database lock would enforce anyway; the pid file turns it
into a message.

`serve -d` hands the terminal back only after the daemon is listening. When
`OPTICORE_LOG_FILE` is unset, `-d` says so and discards the output, so set it
before detaching if you want to keep the logs.

The CLI loads `./.env` itself; no `source` step. Point it elsewhere with
`-env-file /etc/opticore/.env` (or `OPTICORE_ENV_FILE`), which every subcommand
accepts before or after the command name. With the container, pass the same
variables with `-e` instead: real environment variables always win over the
file, and `migrate` is unnecessary because `serve` applies migrations.

Then point a device at the ACS, and check the API answers:

```sh
curl localhost:8081/v1/devices
```

Give the ONTs `http://<this host>:7547/` as their ACS URL. The first Inform
creates the device row; Opticore then writes its own connection-request
credentials so it can wake the device later.

## Configuration

Every setting is an environment variable. Values in parentheses are defaults.

The CLI reads a dotenv file before anything else, in this order:

1. `-env-file path` (must exist),
2. `$OPTICORE_ENV_FILE` (must exist),
3. `./.env` (optional),
4. `.env` in the directory of the `opticore` binary (optional).

Variables already present in the environment are never overridden, so a systemd
unit, a container, or a one-off `FOO=1 opticore serve` behaves as written.

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
| `OPTICORE_API_ADDR` | `:8081` | API listener. There is no authentication: bind it to an address only your provisioning system can reach |

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
| `OPTICORE_LOG_FILE` | empty | append JSON logs to this file instead of stderr, which is what `opticore logs` and the console's logs tab read |
| `OPTICORE_PID_FILE` | derived | where `serve` records its pid; defaults to `<OPTICORE_LOG_FILE>.pid`, else `opticore.pid` beside the binary |

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
| `get` | `{"paths": ["InternetGatewayDevice.DeviceInfo."]}` — exact names or a subtree ending in `.`. Omit `paths` (or send `[]`) to read every parameter under the device's root in one `GetParameterValues` |
| `set` | `{"values": [["path", "value", "xsd:string"]]}` — leaves only; the type may be omitted |
| `add_object` | `{"path": "InternetGatewayDevice.X."}` — result carries the new instance number |
| `delete_object` | `{"path": "InternetGatewayDevice.X.3."}` |
| `reboot` | `{}` |
| `reset` | `{}` — factory reset |
| `ping` | `{"host": "8.8.8.8", "count": 4, "timeout_ms": 1000, "size": 64, "interface": ""}` — only `host` is required |

A `set` reads the old values with a `GetParameterValues` in the same session
first and stores them in the task's `prev`. A revert queues a new `set` built
from that `prev`: `POST /v1/devices/{id}/tasks/{taskId}/revert` for one done
`set`, `POST /v1/jobs/{id}/revert` for every done `set` of a job, as a new job
with the same `max_parallel`. Sealed values are reverted without being shown.

A `ping` spans two sessions. Opticore sets `IPPingDiagnostics.` (TR-098) or
`IP.Diagnostics.IPPing.` (TR-181) to `Requested`, and the task stays `running`.
When the device informs `8 DIAGNOSTICS COMPLETE`, a `GetParameterValues` reads
the result into the task:
`{"state": "Complete", "success": 4, "failure": 0, "avg_ms": 3, "min_ms": 2, "max_ms": 5}`.
Any state other than `Complete` fails the task; a ping that never completes
expires with the task.

A leaf whose last segment ends in `password`, `passphrase`, `secret`, `psk` or
`key` (case-insensitive, `ParameterKey` excepted) is a secret: it is sealed
with `OPTICORE_SECRET_KEY` before it reaches the database, and is returned as
`••••` unless the request carries `?reveal=1`.

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

## Go client

`github.com/prawirdani/opticore/client` is a thin, standard-library-only
client for the HTTP API and the hook. It adds no logic of its own: every call
is one request, and everything Opticore decides is decided by the daemon.

### Install

```sh
go get github.com/prawirdani/opticore@v0.1.0
```

The module is versioned with the daemon. Point `go get` at the release the
daemon runs, and import:

```go
import "github.com/prawirdani/opticore/client"
```

It needs Go 1.26 or newer and pulls in no other module.

### Connect

```go
c, err := client.New("http://acs.internal:8081")
if err != nil {
	return err
}
```

There is no credential to pass: the daemon trusts whoever reaches its API
address. The default `http.Client` has no timeout so a `Watch` can stay open;
give every ordinary call a context deadline.

`client.WithHTTPClient(hc)` replaces the HTTP client, for a private CA or a
proxy.

### Read

```go
page, err := c.ListDevices(ctx, client.ListDevicesOptions{ProductClass: "HG8145V5", Limit: 50})
dev, err := c.GetDevice(ctx, "48575443BD7589AB", false) // id or serial; reveal=false
params, err := c.ListParams(ctx, dev.ID, "InternetGatewayDevice.WANDevice.", false)
tasks, err := c.ListTasks(ctx, dev.ID, client.ListTasksOptions{Status: "failed"})
events, err := c.ListEvents(ctx, dev.ID, client.ListEventsOptions{Limit: 20})
```

A device id is `OUI-ProductClass-Serial`; a bare serial is accepted everywhere.
`Param.Value` for a secret leaf is `••••` unless the call passes `reveal=true`.

### Write

```go
tasks, err := c.CreateTasks(ctx, dev.ID, []client.Task{
	client.Set(
		client.Value{Path: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", Value: "Home"},
		client.Value{Path: "InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.KeyPassphrase",
			Value: "correct horse", Type: "xsd:string"},
	),
	client.Get("InternetGatewayDevice.DeviceInfo.SoftwareVersion"),
}, "order-1234") // Idempotency-Key; "" for none
```

A call with several tasks is an ordered batch: they run in order inside one
session. The `Idempotency-Key` is kept per device forever, so a retried request
returns the original rows instead of queueing twice.

Builders: `Get(paths...)` (no path reads every parameter under the device
root), `Set(values...)`, `AddObject(path)`, `DeleteObject(path)`, `Reboot()`,
`Reset()`, `Ping(host, opts...)` with `PingCount`, `PingTimeout`, `PingSize`
and `PingInterface`.

Creating tasks wakes the device. To poll one to its end:

```go
t, err := c.GetTask(ctx, tasks[0].ID, false)
switch t.Status {
case "done":   // t.Result holds the output
case "failed": f := t.Fault()  // *client.Fault, nil unless a CWMP fault came back
}
```

Other calls: `Wake`, `CancelTask`, `RevertTask(deviceID, taskID, idemKey)`.
`RevertTask` queues a `set` that restores what a done `set` overwrote, using the
`prev` the daemon recorded in that session.

### Jobs

```go
job, err := c.CreateJob(ctx, client.JobSpec{
	Name:        "reboot-olt3",
	Task:        client.Reboot(),
	Select:      client.JobSelector{ProductClass: "HG8145V5"},
	MaxParallel: 10,
})
job, err = c.GetJob(ctx, job.ID)   // Status, Counts, Failures
err = c.CancelJob(ctx, job.ID)     // pending tasks cancelled, running ones finish
job, err = c.RevertJob(ctx, job.ID) // new job reverting the job's done sets
```

`Status` flips to `done` on the daemon's once-a-minute sweep; `Counts` is
current on every read.

### Watch

```go
events, errs := c.Watch(ctx, client.WatchFilter{Types: []string{"task", "job"}})
for e := range events {
	switch e.Kind {
	case "task":
		log.Println(e.Task.Status, e.Task.Fault())
	case "job":
		log.Println(e.Job.ID, e.Job.Status, e.Job.Counts)
	default: // an Inform event type, e.g. "0 BOOTSTRAP"
		log.Println(e.Kind, e.DeviceID, e.Detail)
	}
}
if err := <-errs; err != nil { /* *client.Error: 4xx other than 408/429 */ }
```

`Types` takes Inform event types plus `task` and `job`; `DeviceID` narrows
event and task frames, and job frames are aggregate so they are not sent when
it is set. `Watch` reconnects on a dropped stream, a 5xx or a 429 with
`Last-Event-ID` and backs off from 100 ms to 5 s; a 4xx other than 408 is
terminal and arrives as one `*client.Error`. Cancelling `ctx` closes both
channels. Set `LastEventID` to resume from a known point.

### Hook

The daemon POSTs a step to your provisioner during a live session; the SDK
serves it and gives you typed tasks back:

```go
http.Handle("/hook", client.HookHandler(secret, func(ctx context.Context, s client.Step) ([]client.Task, error) {
	if !client.Matches(s.Events, []string{"0 BOOTSTRAP", "1 BOOT"}) || s.Step > 1 {
		return nil, nil // an empty reply ends the hook
	}
	return []client.Task{client.Get("InternetGatewayDevice.DeviceInfo.")}, nil
}))
```

`secret` is `OPTICORE_HOOK_SECRET`. `s` carries the device identity, the Inform
events, the Inform parameters, the prefetched parameters and, from step 2, the
results of the previous step. `s.State` is whatever string you returned last
time, so keep per-device state in your reply. Set `OPTICORE_HOOK_URL` to this
endpoint and `OPTICORE_HOOK_EVENTS` to the events you want. Returning an error
sends 500, which the daemon treats as "hook failed, session continues".
`client.HandlerFunc` is the same handler with access to `state`.

### Errors

API errors are `*client.Error{Status, Code, Message}`, so match on the code:

```go
var ce *client.Error
if errors.As(err, &ce) && ce.Code == "DEVICE_NOT_FOUND" {
	// forget it
}
```

A task that ended with a CWMP fault has `Task.Fault() *client.Fault` carrying
`Code` and `String`, which are the device's own fault code and message.

## HTTP API

There is no authentication on `/v1`: whatever can reach `OPTICORE_API_ADDR`
can read devices, queue tasks, reveal secrets and factory-reset hardware. Treat
the listener address as the access control — bind it to the interface your
provisioning system uses, and keep it off any network with devices or end users
on it. The CWMP listener and the API are separate addresses for this reason.

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
| `POST /v1/devices/{id}/tasks/{taskId}/revert` | queue a `set` restoring a done `set`'s `prev` |
| `POST /v1/devices/{id}/wake` | connection request now |
| `DELETE /v1/devices/{id}` | forget a device; it re-registers if it informs again |
| `POST /v1/jobs` | bulk job |
| `GET /v1/jobs` | list jobs |
| `GET /v1/jobs/{id}` | progress and the per-device failure list |
| `DELETE /v1/jobs/{id}` | cancel: pending tasks become `cancelled`, running ones finish |
| `POST /v1/jobs/{id}/revert` | new job reverting the job's done `set` tasks |
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

`GET /v1/events` streams Inform events, task transitions and job progress.
Filter with `type=` (an event type, `task` or `job`) and `device_id=`, and
resume with `Last-Event-ID` (or `?last_event_id=`). Ids are `e<events.id>` for
events, `t<task id>:<unix nano>` for task updates and `j<job id>:<unix nano>`
for jobs; each frame's data is the event, task or job as JSON. A `job` frame is
the `GET /v1/jobs/{id}` body, sent when one of its tasks finishes and when the
job is cancelled or done. `device_id=` leaves job frames out.

## CLI

```
opticore [-env-file path] <command>            path defaults to ./.env
opticore serve [-d]                            run the daemon; -d detaches it
opticore stop                                  stop every running opticore serve
opticore migrate                               apply migrations
opticore devices [--serial P] [--class C]      list devices
opticore device <serial|id> [--prefix P]       one device and its parameters
opticore tasks <serial|id>                     task log
opticore task <serial|id> get [path]...        no path: every parameter
opticore task <serial|id> set <path> <value> [type]
opticore task <serial|id> reboot
opticore task <serial|id> ping <host>
opticore task <serial|id> revert <taskId>
opticore jobs | job <id> | job cancel <id> | job revert <id>
opticore wake <serial|id>
opticore version
opticore tui                                   operator console (also the no-arg default)
opticore logs [-n N] [-f]                      tail the daemon log file
```

Every listing subcommand takes `--json`. The CLI connects to the database
directly and never applies DDL except `migrate`.

## Console

`opticore tui` (or just `opticore`) opens the operator console when stdin and
stdout are a terminal; a pipe gets the usage text instead. It reads the local
database and the daemon log, never talks to a device, and keeps itself current:
the logs tab reloads every second, the other lists every five seconds, and a
detail screen only on `r`. The last refresh time is always shown.

| Key | Action |
|---|---|
| `↑ ↓` / `j k`, `g G` | move, jump to ends |
| `Enter` | open a device or job; expand one parameter level |
| `Esc` | collapse a level, then back to the device list |
| `/` | filter the rows: serial prefix, product class, version, freshness |
| `s` | cycle the task status filter (tasks screen) |
| `r` | refresh now; the last refresh time is shown |
| `w` | wake the selected device, after `y/n` |
| `b` / `x` | reboot / factory reset the open device, after `y/n` |
| `1 2 3 4`, `Tab` | devices, tasks, jobs, logs |
| `q`, `Ctrl+C` | quit |

Facts shown: serial, product class, software version, inform age (`fresh` within
two inform intervals, `stale` within six, `offline` beyond), last address,
whether connection-request credentials are set, cached parameters one level at
a time, tasks with fault codes, events, and jobs with their per-device
failures. Secrets never reach the screen: cached secret leaves are stored as a
marker, so the console shows `••••`.

The logs tab shows the daemon's own log, the same lines `opticore logs` prints,
so it only has content when `OPTICORE_LOG_FILE` is set. Reboot and factory reset
queue ordinary tasks through the same validation as the API, which also wakes
the device.

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
- **`/v1` has no authentication.** The API listener can read devices, queue
  tasks, reveal secrets and factory-reset hardware, so `OPTICORE_API_ADDR` is
  the access control: bind it to the interface your provisioning system uses,
  never to a network with devices or end users on it, and prefer a private
  address over `:8081`. Anyone who reaches it is you.
- **Secrets** are AES-256-GCM encrypted at rest and masked unless the request
  asks for `?reveal=1`. `OPTICORE_SECRET_KEY` is the one thing you must back up
  alongside the database; without it stored secrets cannot be read back.
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
- Logs are JSON on stderr. `OPTICORE_LOG_FILE` appends them to a 0600 file
  instead, which `opticore logs [-n N] [-f]` and the console's logs tab read.
  `OPTICORE_LOG_LEVEL=debug` adds the auth challenge and hook step detail.

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

## Not implemented yet

Planned, not built yet:

- Firmware: upload, a device-facing file server, the `download` task and
  `TransferComplete`.
- Creating a job from the CLI.
- Revealing a secret from the console, and creating jobs from it.

Out of scope on purpose: `ScheduleInform`, `ScheduleDownload`, `Upload`,
parameter attributes and notification flags, TLS on the CWMP port, clustering,
multi-tenancy, USP and TR-181-specific handling.

## Layout

```
cmd/opticore/     daemon wiring, CLI
internal/cwmp/    HTTP transport, sessions, XML scanner, SOAP decode/write, auth
internal/acs/     session engine, task service, jobs, wake self-heal
client/           public Go SDK: API client, task builders, SSE watch, hook handler
internal/hook/    the daemon's side of the hook; wire types alias client/
internal/api/     /v1 handlers, SSE
internal/store/   pgx store, secrets, migrations runner
internal/wake/    connection-request client (Digest + Basic)
migrations/       embedded goose migrations
```
