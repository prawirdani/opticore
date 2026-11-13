# AGENTS.md

CWMP (TR-069) ACS daemon. Go 1.26, PostgreSQL, stdlib `net/http`, pgx/v5, goose.

## Commands

```sh
make build                    # bin/opticore, version from git describe
make test                     # SKIPS every database test silently
make itest                    # the real suite: needs OPTICORE_TEST_DSN (set in .env)
gofmt -l . && go vet ./...   # what CI checks besides the tests
```

`./bin/opticore serve -d` detaches and `./bin/opticore stop` shuts it down
gracefully — no `nohup` and no `pkill`, which can match your own shell. The
pid file follows `OPTICORE_LOG_FILE`, so both commands agree.

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

`OPTICORE_LOG_FILE` in `.env` makes the daemon append JSON logs to a file;
`opticore logs [-n N] [-f]` tails it and the TUI's fourth tab shows the same
lines. Without it the daemon logs to stderr only.

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
- `internal/api` — `/v1` handlers, SSE, tokens.
- `internal/store` — hand-written SQL, secret sealing, migrations runner.
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
- Leaves ending in `password|passphrase|secret|psk|key` are sealed with
  `OPTICORE_SECRET_KEY` and rendered `••••` unless a `reveal`-scoped request
  asks otherwise. Do not log or return them.
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
