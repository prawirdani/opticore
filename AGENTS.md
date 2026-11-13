# AGENTS.md

CWMP (TR-069) ACS daemon. Go 1.26, PostgreSQL, stdlib `net/http`, pgx/v5, goose.

## Commands

```sh
make build                    # bin/opticore, version from git describe
make test                     # SKIPS every database test silently
make itest                    # the real suite: needs OPTICORE_TEST_DSN (set in .env)
gofmt -l . && go vet ./...   # what CI checks besides the tests
```

`./bin/opticore serve` is a foreground process that logs to stderr; start it in
a background shell or tmux for a dev loop, and kill it by its pid (not `pkill
-f`, which can match your own shell). No pid file, no daemon mode.

The binary loads `./.env` by itself (`-env-file path` or `OPTICORE_ENV_FILE`
for another file; real environment variables win). `go test` does **not**, so
export the variables yourself before a DB suite unless CI-style env is already
set:

```sh
set -a; . ./.env; set +a
```

`make test` passing means little: `internal/store`, `internal/acs` and
`internal/api` skip themselves without `OPTICORE_TEST_DSN`. Use `make itest`,
or `go test -race -count=1 ./internal/acs/` for one package. Each test binary
creates its own PostgreSQL schema and drops it, so tests may share one database
and run in parallel.

`.env` is gitignored and holds the dev DSN (local container `pg18.1`, databases
`opticore` and `opticore_test`). Dev ports are `17547`/`18081` because a local
GenieACS owns `7547`; the built-in defaults are still `7547`/`8081`.

## Layout

- `internal/cwmp` — transport only: HTTP sessions, XML scanner, SOAP decode and
  precomputed writer, device auth. No domain logic.
- `internal/acs` — session engine, task service, jobs, hook wiring. Session
  state is `*acs.state` stored in `cwmp.Session.Data`.
- `client` — public Go SDK (API client, task builders, SSE `Watch`, hook
  handler). Standard library only, never imports `internal/`; hook wire types
  live here and `internal/hook` aliases them.
- `internal/hook` — the daemon's side of the hook call.
- `internal/api` — `/v1` handlers and SSE. No auth: the listener address is the
  boundary.
- `internal/store` — hand-written SQL, migrations runner.
- `internal/tui` — operator console over `Deps` interfaces; no SQL of its own,
  writes go through `acs.Service`. Its tests are pure (fake readers), so they
  run under plain `make test`.
- `migrations` — embedded goose SQL. Add `00004_*.sql` and up/down; `serve` and
  `migrate` apply them, no other command runs DDL.

## Invariants

- The ACS writes exactly one thing on its own initiative:
  `ManagementServer.ConnectionRequestUsername/Password` (see `isCRPath`). User
  `set` tasks touching those leaves are rejected, and there is no template or
  provisioning engine inside the daemon — that belongs to the caller.
- Wire shape must stay byte-compatible with GenieACS (unprefixed body children,
  `<string>` arrays, `OUI-ProductClass-Serial`). Never use `encoding/xml` in the
  production path; it is only the test oracle for the hand-written scanner.
- Leaves ending in `password|passphrase|secret|psk|key` are secrets. They are
  stored and served as written (no encryption, no masking, by decision), so a
  database dump or a `/v1` response leaks them. Do not log them.
- Handler calls for one session are sequential, and at most one RPC is in
  flight. Do not add per-session goroutines.
- One daemon per database, enforced by a PostgreSQL advisory lock. Never start
  a second `serve` against the same database.

## Tests

`internal/acs/session_test.go` has the fake CPE harness: `newHarness` /
`newHarnessWith` (options such as hook config), `fakeCPE.inform`, and
`informTpl` for hand-written SOAP. Extend it instead of building another fake
device. `internal/cwmp` tests are pure and need no database.

## Docs and release

- `README.md` is user documentation and must match the code — it was written
  from it. `PLAN.md` is the old design plan, gitignored and local only; do not
  reintroduce plan content into the README.
- Commits use Conventional Commit prefixes (`feat(api):`, `docs:`, `ci:`).
- `make release major|minor|patch` only creates the next `v*` tag. Pushing that
  tag triggers the release workflow: six OS/arch binaries, checksums, a GitHub
  release and a `ghcr.io` image. Tagging is publishing.
