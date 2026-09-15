# Progress

Status log for the Aurora DSQL emulator. Append newest work at the top of
[Completed](#completed). The forward-looking design lives in
[PLAN.md](./PLAN.md).

## Current status

**M2 complete.** On top of the M1 classifier, the emulator now enforces Aurora
DSQL's transaction rules per session: fixed `REPEATABLE READ`, one DDL per
transaction, DDL/DML separation, the 3000-row DML cap, and the 30-minute
transaction age limit. Unit and container-backed integration tests pass.

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

### M2 — transaction state machine (2026-09-15)

Delivered:

- `internal/txn/` — `Tracker` for Aurora DSQL's transaction rules, plus
  `RowsFromCommandTag` for reading affected-row counts out of `CommandComplete`
  tags.
- `rules/` — new `isolation.supported` setting; the ruleset now states which
  isolation levels are accepted.
- `internal/classify/` — `Classify` now returns a `Result` carrying the verdict
  *and* a `Kind` per statement (`select`, `dml`, `ddl`, `begin`, `commit`,
  `rollback`). Isolation requests are detected in `BEGIN`/`START TRANSACTION`
  options, `SET TRANSACTION`, `SET SESSION CHARACTERISTICS`, and
  `SET [default_]transaction_isolation`, and refused with `0A000` and
  "Unsupported isolation level: <LEVEL>".
- `internal/wire/` — `SetStartupParameter` re-encodes the startup message so the
  upstream session is pinned to `REPEATABLE READ`.
- `internal/proxy/session.go` — startup rewrite, per-batch `Admit` before
  forwarding, and `CommandComplete` row accounting in the backend pump.
- Tests: `internal/txn/txn_test.go` (rule matrix, age limit, row cap,
  `RowsFromCommandTag`), `internal/classify/classify_test.go` (statement kinds,
  isolation accept/reject), `internal/proxy/session_test.go` (rule enforcement
  through the wire protocol).

Verification:

```
gofmt -l .                                 # no output
go build ./...                             # ok
go vet ./...  && go vet -tags integration ./...   # ok
go test ./...                              # ok: internal/{classify,proxy,txn,wire}
go test -tags integration -count=1 ./test/...
```

Integration run — all fifteen subtests pass, including the six new ones:

- `isolation_is_repeatable_read` — `SHOW transaction_isolation` returns
  `repeatable read`, proving the startup rewrite reaches the backend
- `rejects_unsupported_isolation_level` — `BEGIN ISOLATION LEVEL SERIALIZABLE` → `0A000`
- `rejects_second_DDL_in_a_transaction` → `0A000`
- `rejects_DDL_and_DML_in_one_transaction` → `0A000`
- `allows_DDL_and_DML_in_separate_transactions` — both succeed
- `enforces_the_row_cap_and_permits_rollback` — a 3001-row insert makes the next
  statement return `54000`, and `ROLLBACK` still works

Deliberate limitations (now the M3 transaction-coordinator milestone):

- The backend transaction is not rolled back when a limit is breached. The
  client is expected to ROLLBACK, and no statement is allowed to commit
  meanwhile; there is no `25P02` aborted-transaction state.
- A row cap exceeded by an implicit single-statement transaction is not
  prevented, because the backend has already committed it.
- DDL/DML and row counts are attributed at `Parse` time, so re-executing one
  prepared statement several times in a transaction counts rows but not
  statements.
- The exact text of DSQL's rejection messages still mirrors meaning, not wording.

### M1 — AST classifier and rejection (2026-09-15)

Delivered:

- `internal/wire/` — protocol framing. `ReadTagged` and `ReadStartup` frame
  messages without altering them, so the proxy can inspect traffic and forward
  the original bytes; `DecodeFrontend` maps a frame body to its `pgproto3`
  message.
- `rules/` — versioned ruleset package (`rules.go` plus embedded
  `dsql-2026.09.yaml`). Unknown YAML fields are rejected and rule ids, codes,
  and messages are validated at load.
- `internal/classify/` — libpg_query (real parser, `pg_query_go/v6`) evaluates
  each statement against the ruleset. A parse error is returned as an error, not
  a rejection: unparseable SQL (including DSQL-only syntax like `CREATE INDEX
  ASYNC`) is forwarded for the backing server to answer.
- `internal/proxy/session.go` — startup relay with SSLRequest/GSSENC handling,
  client message classification, and rejection injection. Simple-protocol
  rejections emit `ErrorResponse` + `ReadyForQuery`; extended-protocol rejections
  emit `ErrorResponse` and skip ahead to the next `Sync`. The session tracks
  `ReadyForQuery` transaction status so injected status is accurate. When a
  client negotiates TLS directly with the upstream, the session falls back to a
  raw relay and interception is disabled for that connection.
- 22 rejection rules covering TRUNCATE, extensions, triggers, extra databases,
  temp/unlogged tables, serial types, materialized views, `CREATE TABLE AS`,
  custom types, tablespaces, foreign tables, VACUUM, LISTEN/NOTIFY/UNLISTEN,
  ALTER SYSTEM, and user-defined functions.
- Tests: `internal/wire/framing_test.go`, `internal/classify/classify_test.go`
  (23 rejection cases, 10 allow cases, unknown-field validation),
  `internal/proxy/session_test.go` (white-box rejection over in-memory pipes),
  `internal/proxy/relay_test.go` (raw relay fallback).

Verification:

```
gofmt -l .                                 # no output
go build ./...                             # ok
go vet ./...                               # ok
go vet -tags integration ./...             # ok
go test ./...                              # ok: internal/{classify,proxy,wire}
go test -tags integration -count=1 ./test/...
```

Integration run — all nine subtests pass, including the new rejection paths and
a foreign-key regression guard:

- `rejects_unsupported_simple_protocol_statement` — TRUNCATE returns `0A000`
- `rejects_unsupported_extended_protocol_statement` — CREATE TRIGGER returns `0A000`
- `rejects_serial_column` — serial returns `0A000`
- `connection_usable_after_rejection` — the session is not wedged by a rejection
- `foreign_keys_are_supported` — FK DDL is forwarded and succeeds

Deliberate limitations:

- Ruleset contents are not yet verified against a live cluster. Provisional
  items (savepoints, views, `CREATE FUNCTION ... LANGUAGE sql`, `CREATE TABLE
  AS`) are recorded in the PLAN.md verification backlog.
- `CREATE INDEX ASYNC` is neither rewritten nor accepted; it reaches Postgres,
  which rejects it as a syntax error. Rewriting is M5.
- Rejection messages mirror AWS's meaning, not its exact wording.
- Extended-protocol rejections drop messages until `Sync`, so statements
  batched ahead of a rejected `Parse` are abandoned, matching PostgreSQL's
  error semantics.

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
| 2026-09-15 | Ruleset lives in `rules/` as a package | `go:embed` cannot reach outside its package directory, so the loader and the YAML share `rules/` instead of splitting across `internal/rules` and `rules/`. |
| 2026-09-15 | Parse errors are forwarded, not rejected | DSQL-only syntax such as `CREATE INDEX ASYNC` does not parse with stock libpg_query. Treating parse failure as incompatibility would wrongly reject valid DSQL. |
| 2026-09-15 | Extended-protocol rejection drops messages until `Sync` | Mirrors PostgreSQL: after an error the server ignores messages until the next `Sync`, and this keeps the upstream connection in step with the client. |
| 2026-09-15 | No per-connection prepared-statement tracking in M1 | Classification happens where the SQL text arrives (`Query`, `Parse`), so the name-to-SQL map is not yet needed. |
| 2026-09-15 | TLS-negotiated connections fall back to raw relay | The emulator does not terminate TLS or hold the upstream key, so it cannot see inside a session the client encrypts end to end. |
| 2026-09-15 | Pin the backend to REPEATABLE READ via a startup parameter | `SetStartupParameter` rewrites the startup message; an injected `SET` would require a synchronous round trip that deadlocks when the client authenticates with a password. |
| 2026-09-15 | Row counts come from `CommandComplete`, not statement tracking | The command tag already carries affected rows, so the name-to-SQL map is not needed for the row cap. Tracking is deferred until something else needs it. |
| 2026-09-15 | Limits are enforced at batch boundaries, not mid-exchange | A batch already executing cannot be un-sent, and a refusal is only safe before forwarding. Crossing the cap therefore fails the *next* batch, and ROLLBACK is always admitted so the client can escape. |
| 2026-09-15 | M2 split; transaction coordinator deferred to M3 | Auto-rollback and the aborted-transaction state need the same rollback orchestration the OCC adjudicator needs, so building it once avoids doing it twice. |

## Next up

### M3 — transaction coordinator

- Roll the backend transaction back when a limit is breached, instead of relying
  on the client to ROLLBACK.
- Model the aborted-transaction state: after any rejection inside an explicit
  transaction, refuse later statements with `25P02` until ROLLBACK, and answer
  COMMIT with a `ROLLBACK` command tag.
- Wrap implicit transactions in an explicit backend transaction so a row-cap
  breach is prevented rather than merely reported.
- Widen the startup rewrite into the place where TLS termination and IAM-token
  auth will land (M4).

### Carried forward

- Verify the provisional ruleset entries against a live cluster (see the PLAN.md
  verification backlog).
