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
  tags. The batch that crosses the cap has already run, so the *next* batch that
  is not a ROLLBACK is refused with `54000`.
- Transaction age limit (30 minutes) → `54000` on the next batch.
- A ROLLBACK is always admitted, so a client can escape a transaction that has
  already breached a limit.

Known gaps, tracked as the transaction-coordinator milestone:

- The backend transaction is not rolled back when a limit is breached; the
  client is expected to ROLLBACK, and no statement is allowed to commit
  meanwhile. There is no `25P02` aborted-transaction state.
- A row cap exceeded by an implicit (single-statement) transaction is reported
  but not prevented, because the backend has already committed it. Preventing it
  needs implicit transactions to be wrapped in an explicit backend transaction.

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

Available predicates: `relpersistence`, `column_type`, `objtype`, `txn_kind`.
The `since` field is reserved for version-gating a rule, and `rewrites` (for
`CREATE INDEX ASYNC`) lands in M6. Foreign keys carry no rule: they are
supported, so they are simply forwarded, and they appear only as an OCC source.

## Milestones

| ID | Deliverable | Status |
|----|-------------|--------|
| M0 | Wire proxy passthrough, 1:1 pinning, pgx round-trip test | done |
| M1 | AST classifier + rejection with real SQLSTATEs, versioned YAML rules | done |
| M2 | Session FSM: RR enforcement, 1-DDL, DDL/DML split, row cap, age | done |
| M3 | Transaction coordinator: backend rollback, aborted-transaction state, implicit-transaction wrapping | next |
| M4 | Auth/TLS/version emulation; single DB; UTC/C collation | planned |
| M5 | OCC modes 1 + 2, OCC error codes, FK conflict fixtures | planned |
| M6 | `CREATE INDEX ASYNC` rewrite + `sys.jobs` / `sys.wait_for_job` | planned |
| M7 | Conformance harness vs a real cluster (opt-in) | planned |

## Repository layout

```
cmd/dsql-emu/            CLI entry point
internal/proxy/          session handling, interception, raw relay fallback
internal/wire/           protocol framing and message decoding
internal/classify/       libpg_query AST → verdict and statement kinds
internal/txn/            transaction state machine and limits      (M2)
internal/occ/            conflict injection/adjudication           (M5)
internal/sysjobs/        sys schema emulation                      (M6)
rules/                   embedded versioned ruleset and loader
test/integration/        container-backed tests
test/conformance/        live-cluster fixtures                    (M7)
```

## Verification backlog

Confirm against a live cluster before trusting the ruleset:

- Whether savepoints are rejected. The ruleset does **not** reject them yet,
  because the evidence is not conclusive.
- Whether `CREATE VIEW` is rejected. Two prior-art projects list views as
  unsupported; the ruleset does not reject them.
- Whether `CREATE FUNCTION ... LANGUAGE sql` is rejected. The ruleset currently
  rejects every `CREATE FUNCTION`, which may be too broad.
- Whether `CREATE TABLE AS` is truly unsupported.
- Exact rejection message text. The ruleset mirrors AWS's meaning, not its
  wording.
- Whether `SET DEFAULT` and `CASCADE` conflict like `SET NULL` (they write the
  referencing table, so they should be OCC writers).
- Exact OCC code mapping (`OC000`, `OC001`, ...) and message text.
- Exact SQLSTATE for the DML row-cap violation.
- Reported `server_version` string.

## Prior art

- [tomodian/deesql](https://github.com/tomodian/deesql) — PG-wire proxy, rejects
  unsupported SQL with real SQLSTATEs, rewrites `CREATE INDEX ASYNC`, handles IAM
  token auth.
- [renebrandel/dsql-local-simulator](https://github.com/renebrandel/dsql-local-simulator)
  — Postgres extension that blocks unsupported DDL.
- [LocalStack DSQL](https://docs.localstack.cloud/aws/services/dsql/) — control
  plane APIs plus an embedded-Postgres data plane, including `sys.jobs`.
