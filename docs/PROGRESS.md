# Progress

Status log for the Aurora DSQL emulator. Append newest work at the top of
[Completed](#completed). The forward-looking design lives in
[PLAN.md](./PLAN.md).

## Current status

**M0 complete.** The emulator is a working PostgreSQL wire-protocol relay: one
upstream connection per client, with unit and container-backed integration tests
passing.

Last updated: 2026-09-15.

## How to run

```sh
make up          # start PostgreSQL 17 (host port 5433)
make run         # start the emulator on 127.0.0.1:5432 -> upstream 5433
psql -h 127.0.0.1 -p 5432 -U postgres
```

Other targets:

```sh
make build              # build bin/dsql-emu
make test               # unit tests (no Docker)
make test-integration   # container-backed pgx round-trip (requires Docker)
make down               # stop Postgres and remove volumes
```

CLI flags: `--listen` (default `127.0.0.1:5432`), `--upstream` (default
`127.0.0.1:5433`), `--log-level` (`debug`|`info`|`warn`|`error`).

## Completed

### Repo bootstrap (2026-09-15)

Published the project to `github.com/Dreamescaper/dsql-emulator` (public) and
renamed the Go module from `dsql-emulator` to
`github.com/Dreamescaper/dsql-emulator`, updating imports in `cmd/dsql-emu`,
`internal/proxy`, and `test/integration`.

Verification: `go build ./...`, `go vet ./...`, and `go test ./...` re-run after
the rename; all pass.

### M0 — wire proxy + 1:1 pinning (2026-09-15)

Delivered:

- `cmd/dsql-emu/main.go` — flags, structured logging via `log/slog`, graceful
  shutdown on SIGINT/SIGTERM.
- `internal/proxy/proxy.go` — accept loop, one upstream dial per client
  connection (1:1 pinning), connection registry so shutdown closes in-flight
  sessions, `Addr()` for tests.
- `internal/proxy/relay.go` — bidirectional relay with correct half-close, so a
  client that shuts down its write side still receives the trailing response.
  Per-direction byte counters for observability.
- `internal/proxy/proxy_test.go` — echo round-trip, a half-close regression test,
  and a context-cancel shutdown test.
- `test/integration/proxy_test.go` — testcontainers PostgreSQL 17 + pgx through
  the proxy, covering simple query, extended protocol with parameters, DDL/DML,
  and an explicit transaction.
- `docker-compose.yml`, `Makefile`, `.gitignore`.

Verification:

```
go build ./...                             # ok
go vet ./...                               # ok
go test ./...                              # ok  github.com/Dreamescaper/dsql-emulator/internal/proxy
go test -tags integration -count=1 ./test/...  # PASS (4 subtests)
```

Test evidence — integration subtests all passing:

- `simple_query` — `select 1`
- `extended_protocol_with_params` — `select $1::int + $2::int`, proves the
  Parse/Bind/Execute path relays intact
- `ddl_and_dml_round_trip`
- `explicit_transaction`

Deliberate limitation: M0 relays raw bytes, so there is **no framing seam yet**.
Protocol interception (M1) replaces the relay internals with the `pgproto3`
codec inside `Proxy.serve`. This was intentional — it keeps M0 provably correct
with zero protocol assumptions.

## Decisions log

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-09-15 | Build in Go | `jackc/pgproto3` is purpose-built for transparent PG proxies; `pg_query_go` binds real libpg_query. Installed Go 1.27.1 via Homebrew. |
| 2026-09-15 | Module path `github.com/Dreamescaper/dsql-emulator` | Matches the published GitHub repo. |
| 2026-09-15 | Repo `Dreamescaper/dsql-emulator` is public | Matches the prior-art projects' approach. |
| 2026-09-15 | One upstream connection per client | Correct transaction and `SET` semantics; pooling deferred. |
| 2026-09-15 | Raw byte relay for M0 | Proves end-to-end connectivity with no protocol assumptions before adding interception. |
| 2026-09-15 | Foreign keys are a supported OCC feature | Recent DSQL addition; belongs in the adjudicator, not the reject list. See PLAN.md. |

## Next up

### M1 — AST classifier and rejection

- Add `pg_query_go` (libpg_query) and build `internal/classify`.
- Decode frontend messages with `pgproto3` inside `Proxy.serve`, including
  per-connection prepared-statement tracking for the extended protocol.
- Land the versioned ruleset loader in `internal/rules` plus `rules/*.yaml`.
- Reject unsupported statements with real SQLSTATEs (`0A000`, ...).
- Integration test: unsupported SQL is rejected with the expected code, and
  supported SQL still relays.
