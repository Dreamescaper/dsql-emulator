# Progress

Status log for the Aurora DSQL emulator. Append newest work at the top of
[Completed](#completed). The forward-looking design lives in
[PLAN.md](./PLAN.md).

## Current status

**M7 complete, M3 complete.** The record holds 65 probes; the emulator matches
all of them except the two M6 gaps (`CREATE INDEX ASYNC`, `sys.jobs`). Refusals
inside a transaction fail it exactly as Aurora DSQL does.

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

### M4 (part 1) — TLS termination and version reporting (2026-09-15)

- `internal/proxy/tls.go` — self-signed certificate generation and loading from
  `--tls-cert`/`--tls-key`.
- `internal/proxy/session.go` — `SSLRequest` is now answered by the emulator,
  which completes the TLS handshake and keeps intercepting, instead of falling
  back to a raw relay. `GSSENCRequest` is declined, and `--no-tls` declines TLS.
  This fixes a real blocker: a client using `sslmode=require` could not connect
  through the emulator at all.
- Version reporting: the `server_version` parameter is rewritten to
  `--server-version` (default `16`), and `SELECT version()` and
  `SHOW server_version` are rewritten to `PostgreSQL 16` and `16`.
- Conformance probes added for the environment (`env_server_version`,
  `env_version_function`, `env_current_database`, `env_current_schema`,
  `env_timezone`, `env_client_encoding`, `env_lc_collate`) to pin the real
  values on the next recording.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`. The integration
suite gained `tls_connection_is_intercepted`, which connects with
`sslmode=require`, runs a query, and confirms an unsupported statement is still
refused over TLS, and `reports_the_dsql_server_version`, which checks the
parameter and both rewritten statements.

Deliberate limitation: the client's password is forwarded to the backing server,
so an IAM auth token is accepted only insofar as the backing server accepts it.
Owning the client authentication exchange is the remaining M4 item.

### M7 (part 5) — enum conformance probes (2026-09-15)

Added a ten-probe `enum` group and recorded it (record now 102 cases).

DSQL has no user-defined types: `CREATE TYPE ... AS ENUM` is refused (`0A000`),
and so are `ALTER TYPE ... ADD VALUE`, `ALTER TYPE ... RENAME TO`, and
`DROP TYPE IF EXISTS`. A column or cast naming such a type fails as `42704`,
matching the `serial` behavior. The patterns applications use instead work: a
`text` column with `CHECK (m IN (...))`, and a `CREATE DOMAIN ... CHECK (...)`
whose domain is supported; both reject a bad label with `23514`.

The first comparison found three divergences, all the emulator letting
PostgreSQL answer: `ALTER TYPE` returned `42704`, and `DROP TYPE IF EXISTS`
succeeded. Two predicates were added (`remove_type`, `rename_type`) plus an
`alter_enum` rule, so all three are now refused with `0A000`.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `102 cases match the golden record`, and `--cleanup-only` confirms
no `baseline_` objects remain.

### M7 (part 4) — data-type conformance probes (2026-09-15)

Added a `types` group of 27 probes and recorded them, bringing the record to 92
cases.

- `types_supported_columns` and `types_alias_columns` declare a column of every
  type the documentation lists as supported, with aliases and precision.
- `types_roundtrip` inserts a row through all of them, and `types_runtime_array`,
  `types_runtime_inet`, and `types_runtime_json_ops` cover the query-runtime
  types the docs describe.
- Twenty-one `type_unsupported_*` probes try the types that are absent from the
  supported list, each cleaning up after itself in case it is unexpectedly
  accepted.

DSQL refused all twenty-one with `0A000` "datatype X not supported", including
array columns (`datatype integer[] not supported`). The emulator was allowing
them, so two rules were added: `unsupported_type` (a deny-list matching the
documented unsupported families) and `array_column` (a new `column_array`
predicate). The supported set needed no rule: the emulator forwards it.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `92 cases match the golden record`, and `--cleanup-only` confirms no
`baseline_` objects remain. A leak found mid-run — the two new type tables were
missing from the cleanup list — was fixed and the cluster re-checked clean.

Known limit: the `unsupported_type` rule is a deny-list of the tested types and
the documented families, not an allow-list. A type that is added to the
documented set, or one absent from both the doc and the rule, is not enforced.

### M7 (part 3) — re-record with the M3 and backlog probes (2026-09-15)

Recorded the twelve probes added since the first baseline, bringing the record
to 65 cases. The cluster was left clean (`--cleanup-only` reports no `baseline_`
objects).

The re-record confirmed the M3 predictions and found two more rules to add:

- `ROLLBACK TO SAVEPOINT` is refused with `0A000`; the emulator was letting
  PostgreSQL answer `25P01`. Rule `rollback_to_savepoint` added.
- `SET default_transaction_isolation` is refused with `0A000`; the emulator
  allowed it. Rule `set_isolation` added.
- Confirmed the M3 contract: `rejection_aborts_txn`, `aborted_txn_prefers_25P02`,
  `aborted_txn_recovers_after_rollback`, `rejection_outside_txn_does_not_abort`,
  `row_cap_boundary` (exactly 3000 rows is allowed), and `row_cap_discards_rows`
  all matched on the first comparison.
- Confirmed `LANGUAGE plpgsql` is refused, and cached identity and sequence
  `CACHE 1` are accepted.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `65 cases match the golden record` with only the two M6 gaps.

Also added `conformance.Unrecorded`, and the conformance test now lists cases
the emulator runs that the record does not cover, so a new probe cannot go
unnoticed.

### M3 — transaction coordinator (2026-09-15)

Delivered:

- `internal/proxy/session.go` — failed-transaction state and abort
  orchestration. A refusal inside an explicit transaction now fails the
  transaction; later statements are refused with `25P02`; COMMIT and ROLLBACK
  end it. The row cap is enforced where DSQL enforces it: at the statement that
  crosses it, with the statement's success withheld.
- The upstream transaction is failed by sending a deliberately failing
  statement (`SELECT 1/0`). PostgreSQL then answers COMMIT with the `ROLLBACK`
  command tag and refuses later statements, so the coordinator needs no response
  rewriting — the real server produces the exact behavior.
- Upstream writes are serialized (`upstreamMu`) now that both pumps can inject
  messages, and session state shared between the two pumps is mutex-guarded.
- `internal/txn/` — the row-limit message is now DSQL's wording,
  "transaction row limit exceeded".

Verification:

```
gofmt -l .                                     # no output
go build ./... && go vet ./... && go vet -tags integration ./...   # ok
go test -race ./...                            # ok: all internal packages
go test -tags integration -count=1 ./test/...  # ok
```

Conformance now reports `53 cases match the golden record` with only the two
known M6 gaps. Both previously failing cases closed:

- `row_cap` — `BEGIN | 54000 | 25P02`.
- `read_only` — `BEGIN | 0A000 | 25P02 | ROLLBACK`.

New tests: `TestSessionFailsTransactionWhenRowLimitCrossed`,
`TestSessionCommitOnFailedTransactionRollsBack`, and the integration subtests
`enforces_the_row_cap_and_permits_rollback` (now also asserts the rollback left
zero rows) and `rejection_aborts_the_transaction`.

Deliberate limitation:

- An implicit (single-statement) transaction that crosses the row cap is still
  not prevented: the statement commits before its row count is known.
  Preventing it needs implicit transactions to be wrapped in an explicit
  upstream transaction. Recorded in PLAN.md.

### M7 (part 2b) — golden fixtures split by group (2026-09-15)

Split the single `test/conformance/golden/dsql.json` into one fixture per case
group (`unsupported`, `backlog`, `isolation`, `transaction`, `supported`,
`index`) so no file grows without bound as the suite does.

- `conformance.SaveDir` writes one fixture per group and removes stale fixtures
  first, so removing a group cannot leave phantom cases behind.
- `conformance.LoadDir` merges the fixtures, rejects duplicate case names across
  files, and rejects files from a different suite.
- `cmd/dsql-baseline` now takes `--out-dir` (default `test/conformance/golden`)
  instead of `--out`; `make baseline` uses `GOLDEN_DIR`.
- `test/conformance` loads the directory and honours `GOLDEN_DIR`.

Verification: unit tests `TestSaveDirAndLoadDirRoundTrip`,
`TestSaveDirReplacesStaleFixtures`, `TestLoadDirRejectsDuplicateCaseNames`, and
`TestLoadDirMissingDirectory`; the integration run still reports
`53 cases match the golden record`.

### M7 (part 2) — baseline recorded and ruleset reconciled (2026-09-15)

Recorded `test/conformance/golden/` (one fixture per case group) from a real
Aurora DSQL cluster
(`eu-central-1`, server version `PostgreSQL 16`) and reconciled the ruleset
against it. The cluster was left clean: `--cleanup-only` reports no `baseline_`
objects.

Ruleset and comparator changes:

- New predicates `set_name`, `language_not`, `sequence_cache_min`,
  `identity_cache_min`, and `cache_allow`, plus the matcher logic for them.
- `SET TRANSACTION` (and `SESSION CHARACTERISTICS`) is now refused with `0A000`.
- Savepoints and `RELEASE SAVEPOINT` are refused with `0A000`.
- Synchronous `CREATE INDEX` is refused with `0A000` (`ASYNC` required).
- `CREATE SEQUENCE` and identity columns are refused unless `CACHE >= 65536` or
  `CACHE = 1`.
- `serial` now reports `42704` (type does not exist), matching DSQL.
- The `CREATE DOMAIN` rule was removed: domains are supported.
- The `CREATE FUNCTION` rule was narrowed to non-`sql` languages; `LANGUAGE sql`
  is supported.
- Rule messages were updated to mirror DSQL's wording.
- Isolation is now checked before the general rules, so an unsupported level
  still reports "Unsupported isolation level: X" even though DSQL refuses the
  whole `SET`.

Harness changes:

- `--token-file` reads the token from a file so it need not be pasted into a
  shell history or chat; `dsql.token` and `*.token` are gitignored.
- `--cleanup-only` drops the suite's objects and then fails if any `baseline_`
  object remains, using the new `conformance.Leftovers`.
- Cleanup uses `DROP DOMAIN` (DSQL rejects `DROP TYPE` for domains) and no
  longer issues drops for objects that can never be created.
- `RecordedCase` no longer embeds the whole `Case`; the golden file stores only
  the case name and observations, and the comparator takes suite metadata from
  the emulator's copy so editing the suite does not require re-recording.

Verification:

```
gofmt -l .                                 # no output
go build ./... && go vet ./... && go vet -tags integration ./...   # ok
go test ./...                              # ok: all internal packages
go test -tags integration -count=1 ./test/...                      # ok
go test -tags integration -count=1 -run TestConformanceAgainstEmulator ./test/conformance/
```

The conformance run reports `53 cases match the golden record`. Eight enforced
divergences found by the first diff were fixed; the remaining reported
differences are the intentional `KnownGap` cases:

- `row_cap` — DSQL fails the statement that crosses the cap and aborts the
  transaction (`54000` then `25P02`); the emulator lets the statement commit and
  refuses the next one (M3).
- `read_only` — the `SET` is now refused, but the aborted-transaction state is
  not modelled (M3).
- `create_index_async` and `sys_jobs` (M6).

Deliberate limitations:

- The baseline reflects one cluster in one region at one point in time; re-run
  `make baseline` as DSQL evolves.
- OCC conflicts are not covered: the suite uses a single connection, so
  `OC000` codes and commit-time conflict outcomes remain unverified.
- Non-`sql` function languages, cached identity/sequence acceptance,
  `ROLLBACK TO SAVEPOINT`, and `SET default_transaction_isolation` are assumed
  or narrow; probe cases for each were added and will be recorded next run.

### M7 (part 1) — conformance harness and golden-record plumbing (2026-09-15)

Delivered:

- `internal/conformance/` — the probe suite plus recording and comparison.
  `DefaultSuite` defines 53 cases across `unsupported`, `backlog`, `isolation`,
  `transaction`, `supported`, and `index` groups, each with the schema it needs
  and a `baseline_`-prefixed cleanup list. `Observe` captures outcome, SQLSTATE,
  message, command tag, columns, and rows.
- `cmd/dsql-baseline/` — records a golden record from a real cluster. Takes
  `--host`/`--token` (`DSQL_TOKEN`), builds a TLS DSN, prints the suite, and
  writes JSON. Supports `--dry-run`, `--sslmode`, and `--label`.
- `test/conformance/` — replays the suite through the emulator and diffs against
  the golden record; `KnownGap` and advisory message differences are reported
  rather than enforced.
- `Makefile` — `make baseline`, `make baseline-dry-run`, `make conformance`.

Bug found and fixed while validating the harness:

- **Transaction rules were missed for cached prepared statements.** Clients such
  as pgx prepare a statement once and then re-run it with Bind/Execute and no
  Parse. `BEGIN` reached the emulator as Bind-only after its first use, so the
  tracker never opened a transaction, and a DML after DDL was wrongly allowed.
  Rules are now applied on **Bind** (execution) instead of Parse, with a
  per-session statement-name map. `handleParse` still enforces the dialect rules
  and records each statement's kinds; `handleClose` removes them. Regression
  test: `TestSessionTracksReusedPreparedStatements`.

Verification:

```
gofmt -l .                                 # no output
go build ./...                             # ok
go vet ./...  && go vet -tags integration ./...   # ok
go test ./...                              # ok: internal/{classify,conformance,proxy,txn,wire}
go test -tags integration -count=1 ./test/conformance/
```

The integration run passes `records every step and cleans up`: all 53 cases
produce one observation per step, and the leftover check finds no `baseline_`
tables, sequences, types, or schemas. The golden comparison skips cleanly when
no record is present.

Emulator behavior observed during that run (pre-baseline, so unconfirmed against
real DSQL): the 21 unsupported-statement cases and both isolation cases return
`0A000`; `two_ddl_one_txn`, `ddl_then_dml`, and `row_cap` return `0A000`,
`0A000`, and `54000`; `CREATE VIEW`, `CREATE SEQUENCE`, `CREATE SCHEMA`, and
`SAVEPOINT` currently succeed; `create_index_async` and `sys_jobs` fail with
`42601` and `42P01`, as the known gaps expect.

Deliberate limitations:

- The emulator enforces rules on Bind, so a statement that is parsed but never
  bound is not counted. That matches what actually executes.
- Unparseable SQL (for example `CREATE INDEX ASYNC`) has no known kinds and is
  forwarded without rule checks.
- The golden record is not yet recorded; all ruleset items remain unverified.

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
| 2026-09-15 | No per-connection prepared-statement tracking in M1 | Not needed while classification happened where SQL text arrives. Superseded in M7: cached statements arrive as Bind with no Parse, so a statement-name map is required after all. |
| 2026-09-15 | TLS-negotiated connections fall back to raw relay | The emulator does not terminate TLS or hold the upstream key, so it cannot see inside a session the client encrypts end to end. |
| 2026-09-15 | Pin the backend to REPEATABLE READ via a startup parameter | `SetStartupParameter` rewrites the startup message; an injected `SET` would require a synchronous round trip that deadlocks when the client authenticates with a password. |
| 2026-09-15 | Row counts come from `CommandComplete` | The command tag already carries affected rows, so counting does not need statement tracking. The statement map is still needed, but for applying the rules at execution time instead. |
| 2026-09-15 | Limits are enforced at batch boundaries, not mid-exchange | A batch already executing cannot be un-sent, and a refusal is only safe before forwarding. Crossing the cap therefore fails the *next* batch, and ROLLBACK is always admitted so the client can escape. |
| 2026-09-15 | M2 split; transaction coordinator deferred to M3 | Auto-rollback and the aborted-transaction state need the same rollback orchestration the OCC adjudicator needs, so building it once avoids doing it twice. |
| 2026-09-15 | Transaction rules run on Bind, not Parse | Found by the conformance suite: clients cache prepared statements, so after first use `BEGIN` arrives as Bind with no Parse and the rules were silently skipped. Kinds are computed at Parse and admitted at Bind. |
| 2026-09-15 | Golden record stores no cluster endpoint | The record is committed, so it holds the target label and server version only, never the account's host. |
| 2026-09-15 | Error messages are advisory in the comparison | Wording drifts between DSQL builds and the emulator; SQLSTATE is the stable contract. Messages are still printed. |
| 2026-09-15 | `KnownGap` marks accepted divergence | Cases that the emulator is not expected to match yet (index ASYNC, `sys.jobs`) are replayed and reported without failing, so the suite stays honest and green. |
| 2026-09-15 | Each conformance case is followed by a ROLLBACK | A case can leave a transaction open or aborted; resetting keeps cases independent and the run repeatable. |
| 2026-09-15 | Conformance runs against the emulator in CI, not DSQL | Only `cmd/dsql-baseline` talks to a real cluster, and only when a developer runs it with a token. The normal test path needs Docker only. |
| 2026-09-15 | The golden record is committed; the token is not | The record holds only DSQL behavior and the target label, never the endpoint or credentials. `*.token` and `dsql.token` are gitignored. |
| 2026-09-15 | Golden fixtures are split by case group | A single record would grow without bound; one file per group keeps diffs readable and reviews small. `SaveDir` prunes stale fixtures so the split stays authoritative. |
| 2026-09-15 | Rules are reconciled to the baseline, not to documentation | Real DSQL diverged from the docs: `CREATE DOMAIN` and `LANGUAGE sql` functions are supported, `SET TRANSACTION` is refused outright, and `serial` fails as `42704`. Recorded behavior wins. |
| 2026-09-15 | `row_cap` and `read_only` are `KnownGap`s on M3 | DSQL fails the offending statement and aborts the transaction; reproducing that needs the rollback and aborted-state work already planned for M3, so a workaround would be thrown away. |
| 2026-09-15 | DSQL has two token commands | `generate-db-connect-auth-token` yields a `DbConnect` token that cannot connect as `admin`; `generate-db-connect-admin-auth-token` yields the `DbConnectAdmin` token that can. A non-admin token fails as `08006 access denied`. |
| 2026-09-15 | Fail a transaction by sending a deliberately failing statement | PostgreSQL's own aborted-transaction state then produces the `ROLLBACK` tag on COMMIT and `25P02` for later statements. Synthesizing these responses instead would mean reimplementing PostgreSQL's state machine and rewriting response frames. |
| 2026-09-15 | Withhold the CommandComplete of the statement that crosses the row cap | Aurora DSQL fails that statement, not the next one, so its success cannot be forwarded. This is the one place the coordinator suppresses an upstream message rather than injecting one. |
| 2026-09-15 | Forward COMMIT on a failed transaction | The upstream transaction is already aborted, so PostgreSQL answers with the `ROLLBACK` tag DSQL reports. Rewriting a cached prepared COMMIT would not be possible, since only its name is known at Bind. |

## Next up

### M4 — auth, TLS, and version emulation

- Terminate TLS so interception survives `sslmode=require`, instead of falling
  back to a raw relay.
- Accept an IAM-style token as the password.
- Report a DSQL-like `server_version`, single database, UTC, and C collation.

### Remaining fidelity gaps

- Wrap implicit transactions in an explicit upstream transaction so a
  single-statement row-cap breach is prevented (M3 remainder).
- M6: `CREATE INDEX ASYNC` rewriting and `sys.jobs`.
- M5: add concurrent-session probes to pin OCC behavior (`OC000`, `40001`).
  The suite currently uses a single connection, so it needs multi-session
  support first.

