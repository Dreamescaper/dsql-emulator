# Aurora DSQL Emulator — Plan

Local emulator that lets applications develop and test against Aurora DSQL
semantics without touching a real cluster.

## Goal

The primary fidelity target is **transaction semantics and retry testing**, not
just dialect rejection. Rejecting unsupported SQL is table stakes and already
exists in several projects (see [Prior art](#prior-art)). The differentiator is
being able to exercise the behavior that actually breaks migrations to DSQL:

- optimistic concurrency control (OCC) and `40001` serialization failures
- lock-free conflict at commit, including foreign-key conflicts
- transaction rules (DDL/DML separation, row limits, transaction age)
- deterministic, injectable conflicts so applications can unit-test retry loops

## Non-goals (for now)

- The DSQL **control plane** (cluster CRUD, tags, IAM token issuance). LocalStack
  covers that. This project focuses on the data plane.
- Real multi-region / distributed behavior.
- Byte-for-byte catalog parity with a live cluster.

## Why a proxy

Aurora DSQL speaks the PostgreSQL wire protocol, so a proxy in front of a
standard PostgreSQL container reuses a real SQL engine and only has to emulate
what differs. That is cheap to stand up and gets dialect enforcement almost for
free.

The known limitation: a pass-through proxy **cannot** reproduce true lock-free
OCC, because vanilla PostgreSQL takes row locks and blocks. It can reproduce the
*outcome* (`40001`) by running the backend at `REPEATABLE READ`, but not the
zero-wait latency. This is documented as a fidelity gap, not hidden.

## Architecture

```
client ──TLS──> [dsql-emu]
                  ├─ startup/auth      TLS, IAM token, DSQL server_version, single DB
                  ├─ protocol mux      simple Query AND extended Parse/Bind/Execute
                  ├─ classifier        libpg_query AST → allow | reject(code) | rewrite
                  ├─ session FSM       txn status, ddl/dml counts, row count, age
                  ├─ rewriter          CREATE INDEX ASYNC, BEGIN→RR, version probes
                  └─ OCC adjudicator   commit-time 40001 injection
                         │
                         └──1:1──> postgres (default_transaction_isolation=repeatable read)
```

One dedicated PostgreSQL connection per client session. Transactional state and
`SET` semantics make connection pooling a correctness trap; pooling is deferred.

## Layers of difference from PostgreSQL

1. **Dialect / surface** (easy) — unsupported statements, rejected with real
   SQLSTATEs. Classify with a real parser, never regex.
2. **Transaction semantics** (hard — this is the real DSQL) — OCC, fixed
   `REPEATABLE READ`, lock-free, conflicts at `COMMIT` as `40001`, DML row cap,
   transaction age limit, one DDL per transaction, DDL and DML in separate
   transactions.
3. **Protocol / auth** (medium) — TLS required, IAM token auth, single `postgres`
   database, UTF-8 with C collation, UTC, fixed `server_version`.

## The OCC layer

Three stacked modes, each explicit about what it fakes:

1. **Native RR delegation** — backend runs at `REPEATABLE READ`, so PostgreSQL
   itself raises `40001` ("could not serialize access due to concurrent update")
   on write-write conflict. Outcome-accurate; not lock-free.
2. **Deterministic injection** — config matchers (`table`, `stmt`, `nth`,
   `probability`, `per-session`) force a transaction to fail at `COMMIT` with
   `40001` plus a synthetic OCC code. This is the feature the other emulators
   lack, and the reason the project exists.
3. **Adjudication shim** — a global write-intent registry keyed by
   `(table, key predicate)`; at commit, inject `40001` on overlap using a fake
   commit timestamp. Approximates lock-free OCC. Start coarse, refine later.

### Foreign keys are an OCC source, not a reject rule

Aurora DSQL supports foreign keys (`NO ACTION`, `RESTRICT`, `CASCADE`,
`SET NULL`, `SET DEFAULT`; `MATCH FULL`/`MATCH SIMPLE`; deferrable). It enforces
them with snapshot verification plus commit-time adjudication by implicitly
applying `KEY SHARE` to referenced rows. Therefore:

- FK is a **conflict source** in the adjudicator, alongside write-write and
  `FOR UPDATE` / `FOR KEY SHARE`.
- Vanilla PostgreSQL enforces FKs with locks, so a concurrent
  delete-referenced-row / insert-referencing-row **blocks** before resolving.
  DSQL fails lock-free at commit on both sides. Outcome and winner can differ.
- Partial mitigation: rewrite non-deferred FKs to `DEFERRABLE INITIALLY
  DEFERRED` on the backend so PostgreSQL's check also lands at `COMMIT`. Timing
  gets closer; statement-time locking remains. True lock-free needs the
  adjudicator.
- Conflict tracking must be **key-column-aware**: changing a non-key column on a
  referenced row does not conflict with a referencing insert; changing the key
  does.

Reference error format to emit:

```
ERROR:  change conflicts with another transaction (OC000)
SQLSTATE: 40001
```

## Connection layer

- **TLS termination.** The emulator answers `SSLRequest` with `S` and completes
  the handshake itself using a certificate from `--tls-cert`/`--tls-key`, or a
  self-signed one generated at startup. Interception therefore survives
  `sslmode=require`, which previously could not connect at all. `sslmode=verify-full`
  does not work against the generated certificate, because a real cluster
  presents a CA-issued one. `--no-tls` declines and stays plaintext; `GSSENC`
  is always declined.
- **Version reporting.** The `server_version` parameter is rewritten to
  `--server-version` (default `16`), and `SELECT version()` and
  `SHOW server_version` are rewritten to return Aurora DSQL's values
  (`PostgreSQL 16` and `16`).
- **Still to do.** The password is forwarded to the backing server, so an IAM
  auth token is accepted only to the extent the backing server accepts it.
  Accepting arbitrary tokens means the emulator must own the client
  authentication exchange instead of relaying it, and open its own upstream
  session with configured credentials.

## Session FSM rules

Enforced by parsing, not regex. A "batch" is one simple `Query` (which may hold
several statements) or one prepared statement; a batch outside an explicit
transaction is its own implicit transaction.

- Force `REPEATABLE READ`. The startup message is rewritten to carry
  `default_transaction_isolation = repeatable read`, and any statement that asks
  for another level is rejected with `0A000` and
  "Unsupported isolation level: <LEVEL>".
- Exactly one DDL per transaction.
- DDL and DML must be in separate transactions.
- DML row cap per transaction (3000) — rows are summed from `CommandComplete`
  tags. The statement that crosses the cap fails with `54000`, and its success
  is withheld from the client.
- Transaction age limit (30 minutes) → `54000`.

Failed transactions:

- Any refusal inside an explicit transaction fails it, as on a real server.
  Later statements are refused with `25P02` until the client ends it.
- The upstream transaction is failed by sending a deliberately failing
  statement. PostgreSQL then answers COMMIT with the `ROLLBACK` command tag and
  refuses later statements by itself, so no response rewriting is needed.
- ROLLBACK always ends a failed transaction, and a client can always escape.

Remaining gap: an implicit (single-statement) transaction that crosses the row
cap is still not prevented, because the statement commits before its row count
is known. Preventing it needs implicit transactions to be wrapped in an explicit
upstream transaction.

## Ruleset

The feature matrix is a moving target — foreign keys moved from unsupported to
supported, and stale docs still contradict each other. Rules are therefore
data-driven and **versioned**, never hardcoded.

`rules/dsql-<version>.yaml` names a libpg_query statement node (`stmt`, the
snake_case oneof name) plus optional predicates. A rule rejects when every
predicate it sets holds. A statement with no matching rule is forwarded.

```yaml
dsql_version: "2026.09"

unsupported:
  - id: truncate
    stmt: truncate_stmt
    code: "0A000"
    message: "TRUNCATE is not supported; use DELETE FROM instead"
  - id: temporary_table
    stmt: create_stmt
    relpersistence: ["t"]
    code: "0A000"
    message: "temporary tables are not supported"
  - id: serial
    stmt: create_stmt
    column_type: ["serial", "bigserial", "smallserial"]
    code: "0A000"
    message: "serial types are not supported; use GENERATED ... AS IDENTITY or a uuid"
  - id: materialized_view
    stmt: create_table_as_stmt
    objtype: ["OBJECT_MATVIEW"]
    code: "0A000"
    message: "materialized views are not supported"

isolation:
  supported: ["repeatable read"]

limits:
  dml_rows_per_txn: 3000
  txn_age_seconds: 1800

occ:
  sources: [write_write, select_for_update, select_for_key_share, fk_key_share]
  key_columns_only_for: [fk_key_share]
  error: "change conflicts with another transaction (OC000)"
  sqlstate: "40001"
  inject: []
```

Available predicates: `relpersistence`, `column_type`, `column_array`,
`objtype`, `txn_kind`, `set_name`, `language_not`, `sequence_cache_min`,
`identity_cache_min`, `cache_allow`, `remove_type`, and `rename_type`. The `since` field is reserved for version-gating a rule, and
`rewrites` (for `CREATE INDEX ASYNC`) lands in M6. Foreign keys carry no rule:
they are supported, so they are simply forwarded, and they appear only as an OCC
source.

Rules are checked against a recorded baseline; see
[Conformance suite](#conformance-suite-golden-record).

## Conformance suite (golden record)

The ruleset is only as good as its evidence, so real DSQL behavior is captured
rather than assumed.

- `internal/conformance` holds the probe suite and the recording and comparison
  logic, shared by both halves.
- `cmd/dsql-baseline` records how a real cluster answers the suite and writes a
  golden record (default `test/conformance/golden/`, one fixture per case
  group so no single file grows without bound).
- `test/conformance` replays the same suite through the emulator and diffs it
  against that record. It needs Docker but never touches a cluster.

The comparison enforces outcome, SQLSTATE, command tag, and rows. Error
*messages* are reported but advisory, because server wording drifts. A case may
set `IgnoreRows` (generated ids, `version()`) or `KnownGap` (an accepted
divergence); known gaps are reported and not enforced. Cases the emulator runs
that the record does not cover are listed as unrecorded, so a probe added since
the last baseline is never silently unverified. The emulator's copy of a case
supplies step text and suite metadata, so editing the suite takes effect without
re-recording.

Safety, because the target is someone's cluster:

- every object is prefixed `baseline_` and dropped by `Suite.Cleanup`, which
  runs even when the run fails;
- every case is followed by a ROLLBACK, so a case that leaves a transaction open
  or aborted cannot poison the next one;
- the suite is roughly 70 statements, which keeps cost negligible;
- `--dry-run` prints the whole suite without connecting.

Re-run `make baseline` whenever DSQL changes; the record is a snapshot, not a
fixture to keep forever.

## Milestones

| ID | Deliverable | Status |
|----|-------------|--------|
| M0 | Wire proxy passthrough, 1:1 pinning, pgx round-trip test | done |
| M1 | AST classifier + rejection with real SQLSTATEs, versioned YAML rules | done |
| M2 | Session FSM: RR enforcement, 1-DDL, DDL/DML split, row cap, age | done |
| M3 | Transaction coordinator: backend rollback, aborted-transaction state | done |
| M4 | Auth/TLS/version emulation; single DB; UTC/C collation | in progress (TLS and version done; IAM token auth pending) |
| M5 | OCC modes 1 + 2, OCC error codes, FK conflict fixtures | planned |
| M6 | `CREATE INDEX ASYNC` rewrite + `sys.jobs` / `sys.wait_for_job` | planned |
| M7 | Conformance harness: golden record + emulator diff | done |

## Repository layout

```
cmd/dsql-emu/            emulator CLI
cmd/dsql-baseline/       records a golden record from a real cluster   (M7)
internal/proxy/          session handling, interception, raw relay fallback
internal/wire/           protocol framing and message decoding
internal/classify/       libpg_query AST → verdict and statement kinds
internal/txn/            transaction state machine and limits          (M2)
internal/conformance/    probe suite, recording, and comparison        (M7)
internal/occ/            conflict injection/adjudication               (M5)
internal/sysjobs/        sys schema emulation                          (M6)
rules/                   embedded versioned ruleset and loader
test/integration/        container-backed tests
test/conformance/        emulator-vs-golden tests, golden/<group>.json (M7)
```

## Verification backlog

Answered by baseline runs on 2026-09-15. The ruleset was reconciled to match,
and the emulator now reproduces all 65 cases except the two deliberate M6 gaps:

| Question | Answer |
|----------|--------|
| Savepoints | Rejected, `0A000`. Rule added. |
| `CREATE VIEW` | Supported. No rule. |
| `CREATE FUNCTION ... LANGUAGE sql` | Supported. Rule narrowed to non-`sql` languages. |
| `CREATE FUNCTION ... LANGUAGE plpgsql` | Rejected, `0A000`. Confirmed. |
| `CREATE TABLE AS` / materialized view | Rejected, `0A000`. |
| `CREATE SEQUENCE` | Rejected unless `CACHE >= 65536` or `CACHE = 1`. Rule added; `CACHE 1` confirmed allowed. |
| Synchronous `CREATE INDEX` | Rejected, `0A000`; ASYNC required. Rule added. |
| `serial` column | Rejected as `42704` "type does not exist", not `0A000`. |
| Identity column | Rejected unless `CACHE >= 65536` or `CACHE = 1`. Rule added; cached identity confirmed allowed. |
| `CREATE DOMAIN` | Supported. Rule removed. |
| `SET TRANSACTION` | Refused entirely, `0A000`. Rule added. |
| `SET default_transaction_isolation` | Refused, `0A000`. Rule added. |
| `ROLLBACK TO SAVEPOINT` | Rejected, `0A000`. Rule added. |
| DML row cap | `54000` "transaction row limit exceeded"; the statement itself fails and the transaction is aborted (`25P02`). Exactly 3000 rows is allowed. |
| Aborted transaction | Later statements report `25P02`, `ROLLBACK` ends it, and COMMIT reports the `ROLLBACK` command tag. |
| A refusal outside a transaction | Does not fail anything; the next implicit transaction runs normally. |
| Data types | The documented supported set is accepted, including aliases and precision. Every type absent from it is refused with `0A000` "datatype X not supported", and array columns are refused too. Rule added; the deny-list covers the tested set. |
| Query-runtime types | Arrays and `inet` work in expressions even though they cannot be columns. |
| Enums | No user-defined types exist. `CREATE TYPE`, `ALTER TYPE` (add value and rename), and `DROP TYPE` are all refused with `0A000`; a column or cast naming one fails as `42704`. The workarounds work: a `text` column with a `CHECK (m IN (...))`, or a `CREATE DOMAIN ... CHECK (...)` whose domain is supported; a bad label raises `23514`. Rules added for the three statements. |
| `server_version` | `PostgreSQL 16`. |
| Rejection message text | Recorded verbatim in the golden file. |

Still open:

- OCC behavior: the suite uses one connection, so `OC000` codes and
  commit-time conflict outcomes are unverified. Needs concurrent-session
  probes (M5).
- Whether `SET DEFAULT` and `CASCADE` conflict like `SET NULL` (M5).

## Prior art

- [tomodian/deesql](https://github.com/tomodian/deesql) — PG-wire proxy, rejects
  unsupported SQL with real SQLSTATEs, rewrites `CREATE INDEX ASYNC`, handles IAM
  token auth.
- [renebrandel/dsql-local-simulator](https://github.com/renebrandel/dsql-local-simulator)
  — Postgres extension that blocks unsupported DDL.
- [LocalStack DSQL](https://docs.localstack.cloud/aws/services/dsql/) — control
  plane APIs plus an embedded-Postgres data plane, including `sys.jobs`.
