# Opticore

A small, own-brand TR-069 (CWMP) Auto Configuration Server for FTTH fleets.

Opti- (optical fibre) + -core (the core service): the thing every ONT on the
network talks to, and the thing that carries configuration into it.

Status: plan. Nothing built yet.

---

## 1. Goal

Replace a general-purpose ACS (GenieACS) with one Go service that covers only
the CWMP surface an FTTH operator actually uses:

- receive informs and keep a device registry;
- read and write device parameters;
- reboot and factory reset;
- add and delete object instances;
- firmware download and `IPPingDiagnostics`;
- wake a device on demand;
- bulk and scheduled tasks, and per-leaf rollback of sets.

One process, one PostgreSQL database. The CWMP listener, the HTTP API (§10)
and the TUI ship as one binary. No MongoDB, no presets/provisions engine, no
JavaScript sandbox.

**Opticore is pure CWMP and ACS.** It executes RPCs that a client asked for,
records what devices report, and tells clients what happened. It does not
decide what a device's configuration should be. There are no profiles, no
templates, no provisioning rules and no zero-touch logic inside Opticore; that
belongs to the client (console, billing, a provisioner service, a script),
which drives Opticore through the HTTP API using raw TR-069 paths (§7, §10).

**Daemon only.** Opticore ships as one stock binary configured by environment
variables. It is not a Go library to embed: there is no public `Config` struct,
no `Run`, no in-process plugin. Go clients use the SDK in `client/`, a thin
wrapper over the HTTP API; other languages use plain HTTP and lose nothing.

Non-goal: being a general TR-069 platform, and being part of any particular
console. Opticore knows nothing about subscribers, service plans, invoices or
work orders.

Anything outside the surface below is out of scope until there is a reason.

## 2. Why

GenieACS works, but for this product it is four processes plus a second
datastore, and every consumer of it already re-implements everything above it:

- its configuration layer (presets, provisions, ext scripts, tags) is
  executable logic inside the ACS, where it is hard to version and test; the
  client already knows what a device should look like and should own that logic;
- its sandbox forced three separate workarounds in a single week: arguments
  arrive stringified, there is no global `fetch`, and there is no fault-retry
  endpoint;
- the full parameter tree is retained per device, whether or not it is used;
- a second database must be run, backed up and upgraded per deployment.

The CWMP protocol itself has been reliable across the lab devices. The cost sits
in the configuration layer above it, and Opticore deletes that layer instead of
reimplementing it.

## 3. Scope — inbound

| RPC | Required |
|---|---|
| `Inform` | yes; the only entry point |
| `GetRPCMethods` | yes, trivial |
| `TransferComplete` | yes; firmware download ships in the first release |

Authentication: accept the `Inform` unauthenticated and identify the device by
OUI, product class and serial from the `DeviceId`, matching the effective
behaviour of the system it replaces. Every device that informs is registered;
none is rejected with a 401 (retry storms).

**Security assumption.** Without ACS-side authentication, anyone who can reach
the CWMP port can send an `Inform` carrying a real device's serial and receive
whatever tasks are queued for it, including a PPPoE password a client is
pushing, in clear HTTP. The CWMP port must therefore be reachable only from an
isolated management VLAN. Before production rollout, also do at least one of:

- check the Inform's source address against the expected management subnet,
  and refuse to run `set` tasks carrying secrets otherwise;
- enable ACS-side HTTP Digest once the fleet's credentials are known.

## 4. Scope — outbound

| RPC | Needed for |
|---|---|
| `GetParameterValues` | parameter reads; exact names batched 32 per request, subtrees as one partial path (trailing `.`) |
| `SetParameterValues` | parameter writes; starting `IPPingDiagnostics` |
| `GetParameterNames` | discover instance numbers and writability |
| `Reboot` | device action |
| `FactoryReset` | device action |
| `AddObject` / `DeleteObject` | object instances (e.g. WAN connections) |
| `Download` + file server | firmware |
| Connection Request | wake a device for queued work |

`IPPingDiagnostics` is not a separate RPC: it is a `SetParameterValues` on
`IPPingDiagnostics.*` with `DiagnosticsState=Requested`. The device then informs
with event `8 DIAGNOSTICS COMPLETE`, and a `GetParameterValues` reads the result.
Opticore runs that sequence as one `ping` task.

Out of scope: TR-181-specific handling, USP, XMPP, STUN, bulk data collection,
diagnostics other than `IPPingDiagnostics`, clustering, TLS on the CWMP port
(consumer ONTs commonly reject self-signed certificates; plan HTTP).

## 5. Waking the device (connection request)

CWMP has no server-initiated RPC. The device dials the ACS; the only fast way to
make it dial now is a **Connection Request**: an HTTP request to the
`ManagementServer.ConnectionRequestURL` the device reports in its Inform. It
answers immediately and opens a CWMP session to us within about a second.

Decision: HTTP Connection Request only, no persistent channel.

| Aspect | Choice |
|---|---|
| Transport | HTTP/1.1 GET to the reported URL (device port and random path), 2 s timeout, no redirects, one retry |
| Auth | unauthenticated first; on `401`, MD5 Digest (`qop=auth`) with the credentials Opticore set on the device (see below); Basic only if enabled |
| Speed | cache the Digest nonce per device and pre-send `Authorization`, skipping the 401 round trip; pool the HTTP client connections |
| Coalescing | one request in flight per device; tasks enqueued within a 500 ms window share one wake |
| Fallback | the next periodic inform (typically 300 s). Tasks expire no earlier than `run_at` + two inform intervals (§8) |
| Optional | UDP Connection Request (Annex G) only when the device advertises `UDPConnectionRequestAddress`; never depended on |

**Connection-request credentials are set, not reported.** The Inform carries
`ConnectionRequestURL`, but never the password; reading
`ConnectionRequestPassword` returns an empty string by specification. Waking is
the ACS's own job, so this is the one write Opticore makes on its own
initiative:

- on the first session with a device, and after every `0 BOOTSTRAP`, generate a
  per-device username and password, set them with `SetParameterValues`, and
  store them encrypted;
- until that set has succeeded, the device is reached only on its periodic
  inform.

Clients must not write `ConnectionRequestUsername` / `ConnectionRequestPassword`;
the API rejects sets on those paths.

**Reachability.** Connection requests go from the ACS host to each ONT's
private CR URL. The ACS host must route to the ONT management addresses over
the management VLAN; this is a network prerequisite, not something Opticore
can work around.

**Not every task is idempotent.** `get`, `set` and `delete_object` on a known
instance are safe to re-run. `reboot`, `reset`, `add_object`, `download` and
`ping` are not: if the session drops after the RPC was sent and before its
response arrived, the task is marked `unknown` and never retried automatically.
The client decides what to do next.

Latency budget on a lab Huawei: wake ~0.4 s, session establishment 0.5–2 s, then
the RPC cost is the device's (a full parameter walk takes seconds, one set under
a second).

Not available and not worth designing around: XMPP (Annex K), STUN/TCP
connection request, WebSocket, long-poll. Consumer Huawei/ZTE ONTs do not
implement them.

## 6. Protocol corners that must be handled

These are the ones that break naive implementations:

- **Envelope and CWMP version.** CWMP is SOAP 1.1
  (`http://schemas.xmlsoap.org/soap/envelope/`); SOAP 1.2 is not used by CWMP
  and is not supported (GenieACS does not support it either). The CWMP version
  comes from the Inform's `cwmp` namespace, as GenieACS does it:
  `urn:dslforum-org:cwmp-1-0` → 1.0, `cwmp-1-1` → 1.1, `cwmp-1-2` → 1.2 (1.3
  if a `sessionTimeout` header is present), `cwmp-1-3` → 1.4; anything else is
  rejected with `400`. Every response in the session uses that version's
  namespace set. Match elements by local name, never by prefix.
- **SOAP arrays.** `GetParameterValuesResponse` arrives either as a typed
  element list or as `soapenc:Array` / `soap-enc:Array` with
  `soapenc:arrayType`. Parse both and emit the form the device sent.
- **`MaxEnvelopes`.** Always send exactly one SOAP envelope per HTTP message and
  answer the Inform with `MaxEnvelopes=1`. Later TR-069 amendments fix the value
  at 1 and tell the ACS to ignore what the device advertises; verify against the
  amendment the fleet implements, but do not build multi-envelope support.
- **Session tracking.** A CWMP session is a sequence of HTTP POSTs. Identify it
  by the TCP connection first, and by a `session=<id>` cookie set on the
  `InformResponse` when the CPE opens a new connection per request (GenieACS
  does the same). An empty POST from the device means
  "no more requests from me; the ACS may send its RPCs now". Honour
  `HoldRequests` if the device sends it.
- **Confirming a set.** `SetParameterValuesResponse` carries `Status`: `0`
  means applied, `1` means accepted but applied only after a commit or reboot.
  Status `1` leaves the task `done` with `pending_reboot` set on the device
  row. Changes made by the ACS itself do **not** raise `4 VALUE CHANGE`, so
  never wait for that event. Opticore sends a `ParameterKey` of the task id with
  every set; it is a readable marker of the last applied set, nothing more.
- **Value-change informs.** `4 VALUE CHANGE` reports changes made by someone
  other than the ACS (local web UI, the subscriber). It arrives only for
  parameters whose notification attribute is set; Opticore does not set
  attributes. The listed parameters update the cache and are published as an
  event (§10), so a client can react.
- **Inform idempotency.** A device retries the same Inform on timeout
  (`RetryCount` shows it). The snapshot write is an upsert and therefore
  idempotent. Event rows are deduplicated on (device, event codes,
  `CurrentTime`), with `RetryCount > 0` as a hint. Do not key on `CurrentTime`
  alone: right after boot many CPEs report `0001-01-01` or 1970.
- **Fault codes.** Devices report faults in the `9000`–`9019` range (plus
  vendor codes `9800`–`9899`); the `8000`–`8005` range is for faults the ACS
  sends to the device. Retry policy:
  - retry with backoff: `9002` (internal error), `9004` (resources exceeded),
    and a session lost before the response arrived (idempotent tasks only, §5);
  - fail permanently: `9000` (method not supported), `9001` (request denied),
    `9003` (invalid arguments), `9005` (invalid parameter name), `9006`
    (invalid type), `9007` (invalid value), `9008` (read-only), and any other
    code. The fault is recorded on the task and on the device row
    (`last_fault`).

  Record the numeric code and fault string on the task. For
  `SetParameterValues`, also record the per-parameter `SetParameterValuesFault`
  list.
- **Empty-body session close.** The ACS terminates by answering `204` with no
  body. Some CPEs close the connection themselves first; treat that as normal.
- **Charset.** Take it from `Content-Type`, else from the `<?xml encoding?>`
  declaration (sniffing UTF-16LE and Latin-1 as GenieACS does), else UTF-8.
  Non-UTF-8 bodies are converted once before parsing; an unknown charset is
  `400`.
- **Compression.** Decode `Content-Encoding: gzip` / `deflate` request bodies
  and compress the response the same way; any other encoding is `415`.
- **HTTP.** Accept `Expect: 100-continue`, chunked request bodies and
  `Connection: close`. Answer `405` to anything but POST, `413` to bodies over
  `CWMP_MAX_BODY`, `503` + `Retry-After` when over capacity. Responses always
  carry `Content-Length` and `Content-Type: text/xml; charset="utf-8"`; never
  chunked.
- **Timeouts.** Per request `CWMP_REQUEST_TIMEOUT` (10 s), idle
  `CWMP_SESSION_TIMEOUT`, and a hard session cap of 300 s.

Deliberately not covered: setting parameter attributes and notifications,
diagnostics other than `IPPingDiagnostics` (`DownloadDiagnostics`, `UDPEcho`,
traceroute), `ScheduleInform`, `ScheduleDownload`, `Upload`,
XMPP/STUN/WebSocket, TLS, clustering, multi-tenancy.

## 7. Provisioning is the client's job

GenieACS does zero-touch provisioning with presets and provisions running
inside the ACS. Opticore has no equivalent, on purpose. Provisioning logic
lives in an external client — typically a small provisioner service owned by
the operator — and reaches devices in one of two ways:

- **inform hook** (preferred, lowest latency): Opticore calls the provisioner
  over HTTP during the live CWMP session and runs the tasks it returns in that
  same session (see *Inform hook* below);
- **API + SSE**: the client reacts to an event, enqueues tasks and wakes the
  device. This costs a second session (about 1–3 s plus a working connection
  request) and is the fallback whenever the hook is absent or fails.

Either way the client builds on three things Opticore provides:

1. **Events.** Every Inform's event codes are published on SSE (§10):
   `0 BOOTSTRAP` (new or factory-reset device, or ACS URL change), `1 BOOT`,
   `4 VALUE CHANGE` with the changed parameters, `8 DIAGNOSTICS COMPLETE`,
   `7 TRANSFER COMPLETE`. A client reacts to `bootstrap` by pushing that
   device's configuration.
2. **Device facts.** OUI, product class, serial, software version and the
   Inform parameters are on the device record, so the client can pick the right
   TR-069 paths for that model. Path differences between vendors are the
   client's knowledge, not Opticore's.
3. **Ordered task batches.** `POST /devices/:id/tasks` accepts an array. The
   tasks form one batch: they run strictly in order, and the first `failed` or
   `unknown` task cancels the rest. A client can therefore send "add WAN
   instance, set PPPoE, set WiFi" and never get WiFi on a device whose WAN step
   failed. An `add_object` result (the new instance number) can be referenced
   by later tasks in the same batch as `{instance}`.

Opticore never retries a batch from scratch, never compares desired with
actual state, and never re-pushes configuration on its own. The only write it
initiates itself is the connection-request credentials (§5). Whatever the
provisioner decides is operator code, not Opticore logic.

### Inform hook

When `OPTICORE_HOOK_URL` is set, Opticore asks the provisioner what to do while
the device is still in session. The hook is a step loop over HTTP/JSON:

1. On an Inform whose events match `OPTICORE_HOOK_EVENTS` (default
   `0 BOOTSTRAP`), Opticore first reads `OPTICORE_HOOK_PREFETCH` paths from the
   device in the same session (default none), then POSTs step 1:
   `{device, events, params, prefetch}`.
2. The provisioner answers `{"tasks": [...], "state": "..."}` with an ordered
   batch (same task shapes and batch rules as the API), or `{"tasks": []}` for
   "nothing to do".
3. Opticore runs the batch in the same session and POSTs the next step with the
   results: `{device, step, results, state}`.
4. The provisioner answers with the next batch, or `{"tasks": []}` to end.
   `state` is an opaque string the provisioner may return; Opticore echoes it
   on the next step, so the provisioner can stay stateless.

Payload examples are in Appendix B.

Rules:

- **Session order:** connection-request credentials (§5) first, then the hook
  steps, then queued API tasks.
- **Deadline:** each step call has `OPTICORE_HOOK_STEP_TIMEOUT` (default `2s`);
  the whole hook has `OPTICORE_HOOK_TIMEOUT` (default `10s`). On either expiry,
  a failed call, or a non-`2xx` answer, Opticore records the fault on the device
  row, skips the rest of the hook, and continues the session normally.
- **Auth:** every hook request carries `Authorization: Bearer
  $OPTICORE_HOOK_SECRET`; the provisioner must reject anything else.
- **Everything is logged as tasks.** Hook batches become ordinary task rows
  (with `prev` for sets, §8), so the TUI, rollback, SSE and secret masking see
  them exactly like API tasks.
- **Fallback:** the Inform's events are published on SSE whether the hook
  succeeds or not, so the same provisioner (or another client) can still react
  through API + wake.
- **One HTTP client, kept alive:** Opticore pools connections to the hook URL;
  there is no TCP or TLS setup per step. Use a plain `http://` URL on the
  management host or `unix:/path.sock`.

The Go SDK (`client/`) provides `client.HookHandler(func(ctx, step) ([]Task,
error))`, so a Go provisioner is an ordinary `http.Handler`:

```go
// operator's provisioner service
h := client.HookHandler(secret, func(ctx context.Context, s client.Step) ([]client.Task, error) {
	if s.N > 1 || !s.Has("0 BOOTSTRAP") {
		return nil, nil
	}
	sub, err := billing.BySerial(ctx, s.Device.Serial)
	if err != nil {
		return nil, err
	}
	p := pathsFor(s.Device.ProductClass) // operator's per-model path map
	return []client.Task{
		client.AddObject(p.WANPPP),
		client.Set(client.V(p.PPPUser("{instance}"), sub.PPPoEUser),
			client.V(p.PPPPass("{instance}"), sub.PPPoEPass)),
	}, nil
})
http.ListenAndServe("127.0.0.1:9000", h)
```

### Why HTTP, not gRPC or a broker (decision)

Latency is dominated by the device, not the transport. Rough figures, to be
confirmed during the spike:

| Step | Cost |
|---|---|
| one device RPC round trip (GPV/SPV) | 100 ms – 1 s |
| CWMP session setup | 0.5 – 2 s |
| second session via connection request (no hook) | 1 – 3 s |
| HTTP/1.1 JSON call, kept-alive, LAN | ~0.2 – 0.5 ms |
| gRPC message on an open stream | ~0.05 – 0.2 ms |

Switching to gRPC would save well under a millisecond per step; avoiding one
device round trip saves 100 ms or more. So latency is cut where it matters:

1. same-session execution through the hook (no second session, no wake);
2. prefetch of declared paths before step 1, so the provisioner decides on
   current values without an extra read step;
3. one ordered batch per step with `{instance}` references, so a typical
   bootstrap is one hook step;
4. pooled keep-alive connections to the hook, optionally over a Unix socket.

Kept: HTTP/JSON for the API, SSE for push, HTTP for the hook — one protocol,
debuggable with `curl`, no protobuf toolchain. Rejected: gRPC (sub-millisecond
gain, second toolchain), NATS or another broker (extra process and hop),
Postgres `LISTEN/NOTIFY` (no request/reply), WebSocket (hand-rolled framing),
an in-process Go library or `plugin` (dual build modes, hook bugs inside the
CWMP process), embedded scripting (rebuilds the GenieACS sandbox).

Revisit only if a measured hook step, not a device RPC, shows up as the
bottleneck.

## 8. Bulk, scheduled, and rollback

Bulk operations are a layer on the task queue, not a new mechanism:

- a job (`jobs` table) expands to N device tasks carrying its `job_id` and
  records progress (`done` / `failed` / skipped);
- one session per device still holds; the job adds a concurrency cap
  (e.g. 10 devices at a time) so a mass reboot does not trip the fleet's inform
  storm;
- devices are selected by explicit id/serial list, or by product class and
  software version; never by anything Opticore would have to interpret;
- jobs are cancellable; cancelling marks pending tasks `cancelled` and lets
  running sessions finish;
- every bulk action shows the per-device failure list, not just a count.

Scheduling:

- tasks carry `run_at`; the wake scheduler picks them up at that time and
  coalesces per device;
- this covers reboot windows and firmware windows without implementing
  `ScheduleInform` or `ScheduleDownload`;
- expiry is measured from `run_at`, not from creation (§9), so a reboot
  scheduled for 02:00 is still valid at 02:00; a device that stays offline past
  its expiry does not accumulate a queue forever.

Rollback:

- every `set` task records the previous value of each leaf it touched in
  `tasks.prev` (`{path: [value, type]}`), read with a `GetParameterValues` in
  the same session just before the set;
- revert is a new `set` task built from `prev`, per task or per job, so it obeys
  the same retry rules;
- there is no separate history table: the task log is the history, and the
  janitor's task retention bounds it;
- secret leaves are reverted from their encrypted `prev` and never displayed.

Secrets:

- values written to secret-named leaves (§9) — PPPoE passwords, WiFi
  passphrases — and the connection-request credentials are secrets. They appear
  in `tasks.args`, `tasks.prev` and `devices.cr_password`;
- encrypt them at the application level with AES-GCM (Go standard library),
  key from `OPTICORE_SECRET_KEY`; never `pgcrypto`, which puts the key into SQL
  and query logs;
- the parameter cache never stores a secret leaf's value, only a marker;
- never log secrets, and mask them on every read surface;
- the TUI decrypts locally, so it needs the same `OPTICORE_SECRET_KEY`; losing
  the key loses every stored secret, which mostly means re-setting
  connection-request credentials;
- back up the key separately from the database, and document both with the
  deployment notes.

## 9. Data

Device registry:

- one row per device that has ever informed, keyed by the ACS id
  (`OUI-ProductClass-SerialNumber`). The serial is assumed unique across the
  fleet, so API `:id` accepts either the ACS id or the serial. A device that
  has never informed does not exist, and tasks cannot be queued for it;
- last inform, last session time and source address, software version,
  periodic inform interval as last read;
- connection-request URL as reported; connection-request username and password
  as set by Opticore (§5), password encrypted;
- cached parameter map (lazy): path → value, type, writable, timestamp. It holds
  the Inform parameters and anything a `get` task has fetched, latest value
  only. A flat map serialised as JSONB per device is enough; subtree reads are
  prefix queries. Reads from the TUI and API show only what is cached; a full
  tree needs an explicit `get` of the root;
- secret-named leaves (last path segment matches
  `(?i)password|passphrase|secret|psk|key$`, except `ParameterKey`, which is
  excluded by name because Go's RE2 has no lookbehind) are cached as
  `{"secret": true}` with no value.

Task queue:

- one row per RPC: device, name, args, status, fault code and string, created,
  `run_at`, expires, optional `job_id`, optional `batch_id` with an order within
  the batch;
- names: `get`, `set`, `add_object`, `delete_object`, `reboot`, `reset`,
  `download`, `ping`, `revert`;
- statuses: `pending`, `running`, `done`, `failed`, `unknown` (session lost
  mid-RPC on a non-idempotent task, §5), `cancelled`, `expired`;
- tasks run at the next inform, or immediately after a Connection Request;
  batches run in order, tasks within a batch by their order, unbatched tasks
  oldest first; one session per device at a time;
- `expires_at = run_at + 2 × the device's PeriodicInformInterval` unless the
  caller sets it later; `OPTICORE_TASK_EXPIRY` is the floor;
- retryable faults (§6) increment `attempts` and go back to `pending` until
  `OPTICORE_TASK_MAX_ATTEMPTS`; the janitor marks expired rows `expired` and
  deletes finished rows older than the retention.

Events: every Inform event code, plus firmware change, WAN IP change, and task
and job transitions — written in the same transaction as the change that caused
them, never by a second service. Freshness transitions (fresh → stale →
offline) cannot come from the inform handler, because staleness means no inform
arrived; the janitor writes them by comparing `last_inform` with the device's
interval.

Suggested schema:

```sql
-- Columns marked "enc" hold AES-GCM ciphertext (§8), or JSON whose secret
-- leaves are ciphertext.

CREATE TABLE devices (
  id               TEXT PRIMARY KEY,           -- OUI-ProductClass-Serial
  serial           TEXT NOT NULL UNIQUE,
  oui              TEXT NOT NULL,
  product_class    TEXT NOT NULL,
  software_version TEXT NOT NULL DEFAULT '',
  first_inform     TIMESTAMPTZ NOT NULL,
  last_inform      TIMESTAMPTZ NOT NULL,
  inform_interval  INT  NOT NULL DEFAULT 300,  -- seconds, as last read
  last_addr        TEXT NOT NULL DEFAULT '',
  cr_url           TEXT NOT NULL DEFAULT '',
  cr_username      TEXT NOT NULL DEFAULT '',
  cr_password      TEXT NOT NULL DEFAULT '',   -- enc
  cr_set_at        TIMESTAMPTZ,                -- NULL until credentials are set
  pending_reboot   BOOLEAN NOT NULL DEFAULT false, -- SPV returned Status 1
  last_fault       JSONB,                      -- {code, string, task_id, at}
  params           JSONB NOT NULL DEFAULT '{}' -- lazy parameter cache
);
CREATE INDEX devices_product_idx ON devices (product_class, software_version);

CREATE TABLE jobs (
  id           BIGSERIAL PRIMARY KEY,
  name         TEXT NOT NULL,
  spec         JSONB NOT NULL,               -- task template + device selector
  max_parallel INT NOT NULL DEFAULT 10,
  status       TEXT NOT NULL DEFAULT 'running'
               CHECK (status IN ('running','done','cancelled')),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tasks (
  id          BIGSERIAL PRIMARY KEY,
  device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  job_id      BIGINT REFERENCES jobs(id) ON DELETE SET NULL,
  batch_id    UUID,                         -- ordered batch (§7)
  batch_seq   INT NOT NULL DEFAULT 0,
  name        TEXT NOT NULL CHECK (name IN ('get','set','add_object','delete_object',
                                            'reboot','reset','download','ping','revert')),
  args        JSONB NOT NULL DEFAULT '{}',  -- enc leaves for secret values
  prev        JSONB,                        -- set: {path: [value, type]} (enc leaves)
  result      JSONB,                        -- get/ping output, new instance number, …
  status      TEXT NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending','running','done','failed',
                                'unknown','cancelled','expired')),
  attempts    INT  NOT NULL DEFAULT 0,
  fault_code  INT,
  fault       TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  run_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL
);
CREATE INDEX tasks_pending_idx ON tasks (device_id, run_at) WHERE status = 'pending';
CREATE INDEX tasks_job_idx ON tasks (job_id) WHERE job_id IS NOT NULL;

CREATE TABLE events (
  id        BIGSERIAL PRIMARY KEY,
  device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
  at        TIMESTAMPTZ NOT NULL,
  type      TEXT NOT NULL,
  detail    JSONB NOT NULL DEFAULT '{}'
);
CREATE INDEX events_device_at_idx ON events (device_id, at DESC);

CREATE TABLE firmware (
  id             BIGSERIAL PRIMARY KEY,
  file_name      TEXT NOT NULL UNIQUE,
  sha256         TEXT NOT NULL,
  size           BIGINT NOT NULL,
  product_classes TEXT[] NOT NULL,           -- allowlist
  version        TEXT NOT NULL,              -- expected SoftwareVersion after upgrade
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
  id         BIGSERIAL PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  hash       TEXT NOT NULL,                  -- SHA-256 of the token
  scopes     TEXT[] NOT NULL,                -- 'read', 'write', 'reveal'
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ
);
```

## 10. Integration surface (HTTP API)

Opticore is a standalone service. It does not import, embed, or share a schema
with any client. Anything that wants to drive it uses this API; a console is
one client among several. Opticore calls out over HTTP in one place only: the
inform hook (§7).

The API is the compatibility contract. Every path below is served under `/v1`
(e.g. `POST /v1/devices/:id/tasks`); a breaking change gets `/v2` alongside.
The Go SDK in `client/` is versioned with it and adds no logic of its own:
typed task builders, `*Fault` errors, token auth, `Watch(ctx, filter)` over SSE
with automatic `Last-Event-ID` resume, and `HookHandler` (§7).

Task-creating calls (`POST .../tasks`, `POST /jobs`) accept an
`Idempotency-Key` header. A retry with the same key within 24 h returns the
original result instead of enqueuing again, so a client retrying after a
network error cannot reboot a device twice.

**Control plane (HTTP/JSON, private port, bearer token):**

| Call | Purpose |
|---|---|
| `POST /devices/:id/tasks` | enqueue one task, or an array as an ordered batch (§7); optional `run_at`, `expires_at` |
| `POST /devices/:id/tasks/:taskId/revert` | revert a done `set` task from its `prev` |
| `DELETE /devices/:id/tasks/:taskId` | cancel a pending task (or the rest of its batch) |
| `POST /devices/:id/wake` | connection request now |
| `DELETE /devices/:id` | forget a device (decommissioned); it re-registers if it informs again |
| `POST /jobs` | bulk: a task template plus a device selector, `max_parallel`, optional `run_at` |
| `DELETE /jobs/:id` | cancel a job: pending tasks become `cancelled`, running ones finish |
| `POST /jobs/:id/revert` | revert every done `set` task of the job |
| `PUT /firmware/:file` | upload an image; stores SHA-256 and the product-class allowlist |

Task names, paths and values are TR-069 as-is; Opticore does not translate
them. `:id` is the ACS id or the serial.

**Read plane:** `GET /devices`, `/devices/:id`, `/devices/:id/params`,
`/devices/:id/tasks`, `/devices/:id/events`, `/tasks/:taskId`, `/jobs`,
`/jobs/:id`, `/firmware`.

**Push:** `GET /events` (SSE) streams Inform events, task and job transitions,
and freshness changes, so a client can react without polling. Each event
carries its `events.id`; a client reconnecting with `Last-Event-ID` resumes from
there, bounded by event retention. A `type` filter narrows the stream
(e.g. `?type=bootstrap,task`).

**Auth:** bearer service token per client, SHA-256-hashed at rest
(`api_tokens`), with scopes `read`, `write` and `reveal`. `reveal` is separate
from `write`, so a client that can push values cannot necessarily read secrets
back. No end-user authorization: the caller owns its own permissions.

**Firmware files** are owned by Opticore: images live in a local directory and
are served on `OPTICORE_FILE_ADDR` over plain HTTP to devices on the management
VLAN, under an unguessable path per download task. A `download` task is refused
unless the device's product class is on the image's allowlist. Success is
confirmed by `TransferComplete` and then by `SoftwareVersion` in the next
Inform matching `firmware.version`.

**CWMP** stays on its own port, device-facing only. The API never speaks CWMP.

Why not share a database with a client: it looks cheaper and becomes a
version-skew trap. Two services migrating one schema is exactly the coupling this
project exists to remove.

## 11. Operator surface — TUI

No web UI. The operator surface is a terminal client for operators of the ACS
itself, plus a non-interactive mode for scripts. External systems use the API
(§10).

The TUI reads the local database directly. Every TUI write (enqueuing a task,
waking a device) goes through the same `internal/acs` functions the API uses,
never through raw SQL, so validation and encryption cannot diverge.

| Screen | Content |
|---|---|
| Devices | every device seen: id, serial, OUI, product class, software version, last inform age, last address |
| Device | cached parameter tree (lazy, searchable), connection-request URL and whether credentials are set, last session, last fault, tasks, events |
| Tasks | pending, running, done, failed, unknown with fault codes and strings |
| Jobs | bulk jobs with progress and the per-device failure list |

Behaviour:

- filters: serial prefix, product class, software version, inform age
  (fresh / stale / offline); incremental search over loaded rows;
- refresh on demand, with the last refresh time shown; no background polling;
- device detail loads one parameter level at a time; `Enter` expands, `/`
  filters, `Esc` back;
- secrets masked by default, revealed only by an explicit, confirmed keypress;
- non-interactive on the same binary: `devices`, `device <serial>`, `tasks`,
  `jobs`, each with `--json` and a non-zero exit on error;
- task commands, through `internal/acs` like the API: `task <serial> get
  <path>...`, `task <serial> set <path> <value> [type]`, `task <serial>
  reboot`, `wake <serial>`. These are what the spike (§14) uses before the API
  exists;
- `token create <name> --scope read,write[,reveal]` prints a new API token once;
- all reads come from the local database; a list call never talks to a device.

## 12. Operational requirements

- one daemon per database: on start Opticore takes a PostgreSQL advisory lock
  (`pg_try_advisory_lock`) and refuses to start if another daemon holds it,
  because session locks and wake coalescing live in process memory. The lock
  is released automatically when the process dies;
- bind `CWMP_ADDR` to the management-VLAN address where possible
  (e.g. `10.0.0.5:7547`), not all interfaces, given the unauthenticated
  Inform (§3); publish 7547 directly, with no reverse proxy, so `last_addr`
  is the device's real address;
- one session per device; a second concurrent session gets a readable `400`.
  The session lock has a TTL equal to `CWMP_SESSION_TIMEOUT`, so a session
  that died without closing does not lock the device out;
- concurrency cap with `503` + `Retry-After`, so one slow device cannot exhaust
  session slots;
- rate-limit and log first-time serials; alert on a spike (a misconfigured
  DHCP scope or a spoofing attempt);
- firmware uploads: SHA-256 integrity and a per-product-class allowlist (§10);
- backup: the parameter cache is derived; the task log (rollback history),
  connection-request credentials and `OPTICORE_SECRET_KEY` are not;
- network: IPv4 only for this release. Bind the listeners dual-stack anyway;
  the connection-request client and `last_addr` handling assume IPv4;
- retention: expired tasks and old events cleaned by the hourly janitor;
- health: a `healthz`-style check that reports database reachability and the
  age of the newest inform across the fleet.

## 13. Cutover

ONTs learn the ACS URL from DHCP option 43 on the management VLAN. That fixes
how cutover can work:

- **Clients first.** Whatever provisioning logic lives in GenieACS presets and
  provisions must exist in the client (driving Opticore's API) before any
  device moves. Opticore will not provision a factory-reset device on its own.
- **Site cutover is a port swap.** Every ONT dials the same `host:port` from
  option 43. Publish Opticore on the port the old ACS used; the switch is
  stopping one container and starting another. Rollback is the reverse.
- **Canary is per device, through the old ACS.** Before the site swap, move
  single devices by having GenieACS write `ManagementServer.URL` to point at
  Opticore. The URL change makes the device send `0 BOOTSTRAP`; Opticore sets
  connection-request credentials and publishes the event, and the client
  decides whether to re-push configuration. A later DHCP renewal may push
  option 43 again and move the device back; check on the lab models which side
  wins, and use a per-MAC/vendor-class option 43 on the DHCP server if the
  canary must stick.
- **Credentials reset on move.** Every moved device gets new
  connection-request credentials from Opticore on its first session (§5), so
  wakes work from the second session on. Moving back to GenieACS leaves it with
  credentials GenieACS does not know; GenieACS must re-set them.
- Do not rely on reading `ManagementServer.URL` from the device: consumer ONTs
  report it as writable with no value.
- Factory reset wipes the ACS URL on the device, but option 43 hands it back on
  the next DHCP lease, so a reset device returns without a site visit. Verify
  this once per ONT model.
- Order: one device, one PON port, one OLT (per-device URL or scoped option 43),
  then the site (port swap). Delete the old engine only after a parity window on
  every ONT model.

## 14. Acceptance — 3-day spike

Three days, one ONT model, no firmware:

1. the device informs Opticore within its periodic interval, and after
   Opticore has set its connection-request credentials, on a Connection
   Request;
2. a parameter subtree read in the TUI returns device values;
3. setting one writable leaf applies on the device and is visible in the tree;
4. reboot lands, and the next inform is recorded;
5. rollback to the previous ACS requires only a container swap.

If (1) is not passing at the end of day 3, stop: the estimate is wrong and the
existing ACS stays.

## 15. Release stages after the spike

The first release is every stage below; the spike gate (§14) still comes first
and is unchanged.

1. HTTP API (control + read planes, tokens) and SSE;
2. inform hook (step loop, prefetch, timeouts, SSE fallback) and the Go SDK in
   `client/` including `HookHandler` (§7, §10); tag `v0.1.0`;
3. ordered task batches, `AddObject` / `DeleteObject` with instance references;
4. second and third ONT models; captured CWMP fixtures per model;
5. per-leaf rollback (`prev`, revert) and bulk jobs;
6. `IPPingDiagnostics`;
7. firmware download with the built-in file server and `TransferComplete`;
8. operator provisioner service (hook and/or API client, outside this repo) in
   place; parity window on all models, then removal of the old ACS and
   all its glue.

## 16. Stack

Chosen for the fastest CWMP handler and parser; behaviour on the wire follows
GenieACS (`lib/cwmp.ts`, `lib/soap.ts`), which also uses its own small XML
parser rather than a library.

| Layer | Choice |
|---|---|
| Language | Go (latest stable) |
| CWMP XML parse | own zero-copy byte scanner over `[]byte`: elements, attributes, text, entities; returns sub-slices of the body and allocates only to decode entities. No DTD, no schema. `encoding/xml` only as the test oracle |
| CWMP XML write | precomputed envelope prefix per CWMP version, `append` into pooled `[]byte`; no `xml.Marshal` |
| HTTP | `net/http`; `Server.ConnContext` carries per-connection session state |
| Charsets | UTF-8 fast path; `golang.org/x/text/encoding` otherwise |
| Compression | `compress/gzip`, `compress/flate` |
| Database | `pgx/v5` + `pgxpool`; one `pgx.Batch` per Inform; hand-written SQL, no ORM |
| Migrations | `goose`, embedded |
| JSON | `encoding/json`; JSONB passed through as `json.RawMessage` |
| Logs | `log/slog` JSON |
| TUI | `bubbletea` + `lipgloss` (after the spike) |
| CLI | standard `flag` with subcommands |

Not chosen: `fasthttp` (not `net/http`-compatible; the database, not the HTTP
layer, bounds a few thousand requests/s), third-party XML libraries (CWMP's XML
subset is small enough to scan directly), an ORM.

## 17. Suggested repository layout

```text
cmd/opticore/          # the daemon: CWMP + API + file server + TUI + subcommands
client/                # public Go SDK over the HTTP API, HookHandler
internal/config/       # environment loading and validation
internal/cwmp/         # transport, SOAP, session, RPC dispatch
internal/acs/          # devices, tasks, jobs, events, wake-up, freshness
internal/hook/         # inform hook caller: step loop, prefetch, timeouts
internal/api/          # HTTP control/read planes, SSE
internal/store/        # pgx repositories behind interfaces
internal/tui/          # terminal screens
migrations/            # goose SQL, embedded and applied on start
```

The public contract is the HTTP API (`/v1`), the hook payloads, and `client/`
which mirrors them. Everything under `internal/` can change freely.

## 18. Naming and configuration

- binary and compose service: `opticore`
- Go packages: `internal/cwmp` (protocol), `internal/acs` (domain)
- TUI: same binary, no args for interactive, subcommands for scripts
- env: `CWMP_*` for protocol settings (port, timeouts, batch size),
  `OPTICORE_*` for service settings (database, retention, wake retries)
- user-facing copy in any client still says "ACS" — nobody should have to learn
  a codename to use the product

## 19. Decisions

Resolved:

1. Opticore is pure CWMP and ACS: no profiles, templates or provisioning rules;
   clients own configuration and drive it through raw-path tasks (§7).
2. Daemon only: one stock binary, environment configuration, no embeddable
   library. Go clients use the `client/` SDK over HTTP (§1, §10).
3. Presets are replaced by an operator-owned provisioner service reached
   through the HTTP inform hook, which runs tasks in the live session; API +
   SSE + wake is the fallback (§7).
4. Transport stays HTTP/JSON for the API, SSE and the hook. Latency is cut by
   same-session execution, prefetch and batching, not by the wire protocol;
   gRPC, brokers, WebSocket, in-process plugins and scripting are rejected
   (§7, *Why HTTP*).
5. Firmware ships in the first release (§10, §15).
6. Lazy parameter cache: Inform parameters and fetched subtrees; latest value
   only (§9).
7. Faults are columns, not a model: `tasks.fault_code` / `fault`, and
   `devices.last_fault` (§9).
8. `IPPingDiagnostics` ships in the first release (§4).
9. IPv4 only; listeners bound dual-stack (§12).
10. Opticore owns firmware files: upload, SHA-256, per-product-class allowlist,
   built-in file server (§10).
11. Rollback history lives in `tasks.prev`; no separate history table (§8).
12. Opticore sets connection-request credentials itself; it is the only write it
   initiates (§5).

Still open:

1. Does option 43 override a per-device `ManagementServer.URL` on renewal, per
   ONT model? Decides how canaries stick (§13).
2. Source-address check vs ACS-side Digest for the Inform (§3): pick one before
   production rollout.
3. Should Opticore push a fleet-wide `PeriodicInformInterval` itself (like the
   CR credentials), or is that the client's job too? The plan assumes the
   client's.

---

## Appendix A — API shapes

All paths are under `/v1`; the prefix is omitted below for brevity. All
responses are `application/json`. Errors:

```json
{"error": {"code": "DEVICE_NOT_FOUND", "message": "no device with id 00259E-...", "details": {}}}
```

Codes: `VALIDATION` (400), `UNAUTHORIZED` (401), `FORBIDDEN` (403), `DEVICE_NOT_FOUND` (404), `TASK_NOT_FOUND` (404), `JOB_NOT_FOUND` (404), `CONFLICT` (409, e.g. task already running), `FIRMWARE_NOT_ALLOWED` (422), `DEVICE_UNREACHABLE` (502).

Single task:

```json
POST /devices/48575443BD7589AB/tasks
{"name": "set", "args": {"values": [["InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", "HomeNet", "xsd:string"]]}}
```

```json
POST /devices/48575443BD7589AB/tasks
{"name": "reboot", "args": {}, "run_at": "2026-11-14T02:00:00+07:00"}
```

```json
POST /devices/48575443BD7589AB/tasks
{"name": "ping", "args": {"host": "8.8.8.8", "count": 4, "timeout_ms": 1000}}
```

```json
POST /devices/48575443BD7589AB/tasks
{"name": "download", "args": {"file": "HG8145V5_V5R020C10S280.bin"}}
```

Ordered batch (stops at the first failure; `{instance}` is the number returned
by the batch's `add_object`):

```json
POST /devices/48575443BD7589AB/tasks
[
  {"name": "add_object", "args": {"path": "InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection."}},
  {"name": "set", "args": {"values": [
    ["InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.{instance}.Username", "s0004@isp", "xsd:string"],
    ["InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.{instance}.Password", "…", "xsd:string"],
    ["InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.{instance}.Enable", "true", "xsd:boolean"]
  ]}},
  {"name": "set", "args": {"values": [
    ["InternetGatewayDevice.LANDevice.1.WLANConfiguration.1.SSID", "HomeNet", "xsd:string"]
  ]}}
]
```

Bulk job:

```json
POST /jobs
{"name": "reboot-olt3", "task": {"name": "reboot", "args": {}},
 "select": {"product_class": "HG8145V5", "serials": ["48575443BD7589AB", "…"]},
 "max_parallel": 10, "run_at": "2026-11-14T02:00:00+07:00"}
```

Device:

```json
GET /devices?product_class=HG8145V5&freshness=stale&limit=50
{
  "data": [
    {
      "id": "00259E-HG8145V5-48575443BD7589AB",
      "serial": "48575443BD7589AB",
      "oui": "00259E",
      "product_class": "HG8145V5",
      "software_version": "V5R020C10S271",
      "last_inform": "2026-11-13T01:24:00Z",
      "last_addr": "10.20.3.41",
      "freshness": "stale",
      "cr_ready": true,
      "pending_reboot": false,
      "last_fault": null
    }
  ],
  "meta": {"next_cursor": null, "total": 1}
}
```

`freshness`: `fresh` (informed within one interval), `stale` (within three),
`offline` (longer). `cr_ready`: connection-request credentials are set (§5).

Parameter map entry (inside `GET /devices/:id/params?prefix=…`):

```json
{"path": "InternetGatewayDevice.DeviceInfo.SoftwareVersion",
 "value": "V5R020C10S271", "type": "xsd:string",
 "writable": false, "updated_at": "2026-11-13T01:24:00Z"}
```

Secret-named leaves are not cached (§9) and always return `"value": "••••"`.
Secret values in `tasks.args` / `prev` return masked unless the caller sends
`?reveal=1` with a token holding the `reveal` scope.

SSE event:

```text
id: 81234
event: bootstrap
data: {"device_id": "00259E-HG8145V5-48575443BD7589AB", "at": "2026-11-13T01:24:00Z", "codes": ["0 BOOTSTRAP", "1 BOOT"]}
```

## Appendix B — configuration

Environment variables only; there is no config file and no programmatic
configuration. The daemon validates everything before binding any port
(missing DSN, key not 32 bytes, unparsable durations or addresses) and exits
non-zero with a readable error. The TUI and subcommands need only
`OPTICORE_DB_DSN` and `OPTICORE_SECRET_KEY`.

| Variable | Default | Purpose |
|---|---|---|
| `OPTICORE_DB_DSN` | — | PostgreSQL DSN |
| `OPTICORE_SECRET_KEY` | — | AES-256-GCM key (base64, 32 bytes); required by daemon and TUI |
| `CWMP_ADDR` | `:7547` | device-facing listener; prefer the management-VLAN address (§12) |
| `CWMP_GPV_BATCH` | `32` | parameter names per `GetParameterValues` |
| `CWMP_SESSION_TIMEOUT` | `30s` | idle session limit, also the session-lock TTL |
| `CWMP_CR_TIMEOUT` | `2s` | connection-request timeout |
| `CWMP_REQUEST_TIMEOUT` | `10s` | per-request read/write limit |
| `CWMP_MAX_BODY` | `10MiB` | largest accepted request body (after decompression) |
| `CWMP_MAX_SESSIONS` | `1000` | concurrent sessions before `503` + `Retry-After` |
| `OPTICORE_API_ADDR` | `:8081` | HTTP API (`/v1`) and SSE |
| `OPTICORE_FILE_ADDR` | `:8082` | firmware file server, device-facing |
| `OPTICORE_FIRMWARE_DIR` | `/var/lib/opticore/firmware` | firmware image storage |
| `OPTICORE_HOOK_URL` | — | inform hook endpoint (`http://…` or `unix:/path.sock`); unset = no hook |
| `OPTICORE_HOOK_SECRET` | — | bearer secret sent on every hook call; required when the URL is set |
| `OPTICORE_HOOK_EVENTS` | `0 BOOTSTRAP` | comma-separated Inform event codes that trigger the hook |
| `OPTICORE_HOOK_PREFETCH` | — | comma-separated paths (partial paths allowed) read before step 1 |
| `OPTICORE_HOOK_STEP_TIMEOUT` | `2s` | deadline per hook call |
| `OPTICORE_HOOK_TIMEOUT` | `10s` | deadline for the whole hook in one session |
| `OPTICORE_TASK_EXPIRY` | `10m` | minimum pending task lifetime; actual is `run_at` + 2 × inform interval |
| `OPTICORE_TASK_MAX_ATTEMPTS` | `5` | retries for retryable faults |
| `OPTICORE_TASK_RETENTION` | `720h` | finished task rows (and rollback history) kept by the janitor |
| `OPTICORE_EVENT_RETENTION` | `720h` | event rows kept by the janitor |
| `OPTICORE_LOG_LEVEL` | `info` | structured JSON logs to stderr |

Hook payloads:

```json
// Opticore -> provisioner, step 1
POST {OPTICORE_HOOK_URL}
Authorization: Bearer {OPTICORE_HOOK_SECRET}
{"step": 1,
 "device": {"id": "00259E-HG8145V5-48575443BD7589AB", "serial": "48575443BD7589AB",
            "oui": "00259E", "product_class": "HG8145V5", "software_version": "V5R020C10S271"},
 "events": ["0 BOOTSTRAP", "1 BOOT"],
 "params": {"InternetGatewayDevice.ManagementServer.ConnectionRequestURL": {"value": "http://10.20.3.41:7547/abc", "type": "xsd:string"}},
 "prefetch": {"InternetGatewayDevice.WANDevice.1.WANConnectionDevice.1.WANPPPConnection.": {}}}

// provisioner -> Opticore: an ordered batch (same task shapes as Appendix A), optional state
{"tasks": [{"name": "add_object", "args": {"path": "…WANPPPConnection."}},
           {"name": "set", "args": {"values": [["…WANPPPConnection.{instance}.Username", "s0004@isp", "xsd:string"]]}}],
 "state": "opaque-string"}

// Opticore -> provisioner, step 2
{"step": 2, "device": {…}, "state": "opaque-string",
 "results": [{"name": "add_object", "status": "done", "result": {"instance": 2}},
             {"name": "set", "status": "done", "result": {"status": 0}}]}

// provisioner -> Opticore: end of hook
{"tasks": []}
```

## Appendix C — first week

1. `go mod init`, layout, `cmd/opticore`, `internal/config` (environment
   loading and validation), advisory lock, `healthz`.
2. Migrations: the spike needs devices, tasks, events; jobs, firmware and
   api_tokens can land with their stages (§15).
3. Store interfaces + pgx implementations; table tests where the repo pattern
   allows, otherwise plain unit tests on query builders.
4. CLI: `devices`, `device`, `tasks`, `task … get|set|reboot`, `wake`; the
   interactive TUI (read-only devices list and detail) can follow the spike.
5. CWMP: envelope parse, `Inform`, `InformResponse`, session cookie, session
   close, snapshot write, single-session lock with TTL.
6. Point one lab ONT at the service (port swap, or a per-device
   `ManagementServer.URL` from GenieACS, §13) and pass spike gate 1.
7. Then GPV/SPV, setting connection-request credentials, and connection request
   (gates 2–3), reboot (gate 4), rollback check (gate 5).

## Appendix D — testing

- Capture real XML from the lab once per ONT model and keep it as fixtures:
  `Inform` with a parameter list, `GetParameterValuesResponse`,
  `SetParameterValuesResponse` (with `Status` 0 and 1), `AddObjectResponse`, a
  `cpeFault` with `SetParameterValuesFault` entries, an
  `8 DIAGNOSTICS COMPLETE` Inform, and a `TransferComplete`.
- Golden tests for the response envelope (`cwmp:ID` echo, namespace set per
  CWMP version 1.0–1.4).
- Differential fuzz test (`go test -fuzz`): the CWMP scanner and
  `encoding/xml` must agree on every fixture and fuzz input; benchmarks report
  ns/op and allocs/op for Inform and a large `GetParameterValuesResponse`.
- Table tests for the path parser: exact, partial, instance, `{instance}`
  substitution.
- Task state machine: pending → running → done / failed / unknown, expiry from
  `run_at`, retry only on retryable `9xxx` faults (§6), no automatic retry of
  non-idempotent tasks, and batch cancellation after the first failure.
- Connection-request credentials: set on first session and after
  `0 BOOTSTRAP`, not after `1 BOOT`; client sets on those paths are rejected.
- Inform hook: an `httptest` provisioner against captured device fixtures —
  prefetch before step 1, `{instance}` from step 1 usable in the same batch,
  `state` echoed, `[]` ends the hook, step and total timeouts and non-`2xx`
  answers skip the rest but still end the session with `204`, every hook task
  appears as a task row, queued API tasks run after the hook, the SSE event
  fires either way, and a wrong secret is rejected by `client.HookHandler`.
- Config: defaults, and the daemon exits non-zero without binding a port on a
  missing DSN, a key that is not 32 bytes, or a hook URL without a secret.
- Advisory lock: a second daemon against the same database refuses to start.
- Secret regex: `Password`, `KeyPassphrase`, `PreSharedKey` match;
  `ParameterKey` does not.
- Store tests run against a real PostgreSQL in CI; skip without a DSN. Each
  run uses its own schema, but advisory locks are per database, so stop any
  running daemon on the same database first (or point the tests at another
  one).
- No test talks to a device. The lab cutover is a manual gate, not a unit test.
