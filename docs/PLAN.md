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
                  └─ OCC adjudicator   bounded lock waits → commit-time 40001
                         │
                         └──1:1──> postgres (repeatable read, lock_timeout=50ms)
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
3. **Adjudication** — the backend's own lock manager is the write-intent
   registry. Its lock waits are bounded, and a refused lock is reported at
   `COMMIT` as `40001` instead of at the statement.

### What the emulator does

1. **Native delegation (done).** The upstream runs at `REPEATABLE READ`, so a
   write-write conflict raises PostgreSQL's serialization failure. The emulator
   rewrites that error to DSQL's wording, `change conflicts with another
   transaction (OC000)`, keeping SQLSTATE `40001`. A refused lock (`55P03`) is
   rewritten the same way, because under a bounded wait it means the same
   thing. This is the fallback for what mode 3 declines to take over.
2. **Deterministic injection (done).** `occ.inject` rules name tables and an
   interval; when a transaction that touched one of them commits, the emulator
   rolls the upstream back instead of committing and reports the conflict. This
   is what lets an application unit-test its retry loop without a real race.
   Injection applies to explicit transactions only, since an implicit
   transaction has already committed by the time it ends.
3. **Adjudication (done).** No registry is built, because the backend already
   keeps one. PostgreSQL takes row locks where DSQL adjudicates, so the set of
   transactions waiting on each other *is* the write-intent graph; what the
   emulator adds is a bound on the wait and a different place to report it.

### How adjudication works

The design rests on a measurement rather than an assumption. A `BEFORE ROW`
trigger cannot be used to intercept a conflict, because `GetTupleForTrigger`
locks the tuple before the trigger fires: a trigger meant to detect the
conflict never runs. What does work is bounding the wait.

1. The session asks the backend for `lock_timeout` (`occ.lock_timeout_ms`,
   50ms). A wait that runs out becomes `55P03`, in tens of milliseconds rather
   than for as long as the other transaction lives. When the other transaction
   has already committed, `REPEATABLE READ` raises `40001` at the statement with
   no wait at all. Both codes mean the same thing: two transactions wanted the
   same rows.
2. When a transaction opens, the emulator establishes a savepoint on it,
   invisibly — the client's own `ReadyForQuery` is withheld until it is in
   place. One per transaction is enough; a transaction that reaches the
   savepoint is doomed, so the work it loses is discarded at `COMMIT` anyway,
   and one subtransaction is cheaper than one per statement.
3. On either code, the emulator rolls back to that savepoint, which leaves the
   transaction usable and its snapshot intact, and answers the refused
   statement the way DSQL answers it: as if it had run. The row count comes
   from a **shadow** — a read-only statement derived from the refused one's
   parse tree, which takes no locks and so cannot be refused in turn. An
   `UPDATE` or `DELETE` becomes `SELECT count(*)` over the same relation and
   predicate; an `INSERT ... VALUES` states its own count and needs no shadow;
   a locking `SELECT` is re-run with the locking clause removed, so the client
   gets its rows.

   A shadow keeps the parameters it still refers to and is bound with the
   values the client bound for them, which is what makes the shape application
   code actually writes work. They are **renumbered from `$1`**, because
   PostgreSQL infers a parameter's type from where it is used and cannot type
   one the shadow no longer mentions: `UPDATE t SET v = $1 WHERE id = $2`
   becomes `SELECT count(*) FROM t WHERE id = $1`, bound with the client's
   second value. No parameter types are declared, because every parameter a
   shadow keeps sits in the expression it was already used in and so is
   inferred from the same context. A parameter used both in a dropped clause
   and a kept one could in principle be inferred differently; the shadow then
   fails and the conflict is reported at the statement.
4. The transaction is marked doomed and fails at `COMMIT` with
   `40001 change conflicts with another transaction (OC000)`.

Outside an explicit transaction there is no commit to defer to — the implicit
transaction has already ended — so the statement itself reports the conflict,
with DSQL's wording. This is the one place the emulator reports a conflict at a
statement on purpose, and it is what DSQL does too.

### What adjudication does not reproduce

- **First writer wins, not first committer.** DSQL fails whichever transaction
  the cluster adjudicates second; the emulator fails whichever asked the
  backend for the row second. The two coincide when a transaction commits in
  the order it wrote, and diverge when it does not.
- **A conflict spanning several rows can leave no winner.** `occ_multirow_predicate`
  recorded DSQL failing *both* transactions that updated the same three rows.
  The emulator cannot: it adjudicates on the backend's row locks, and whichever
  transaction takes them first is by construction able to commit. The row counts
  match on both sides; how many transactions survive does not. The case carries
  a `KnownGap` rather than being made to pass, because reproducing it means
  modelling DSQL's adjudication rather than borrowing PostgreSQL's, which is the
  premise the whole layer rests on. It is one recording, so whether DSQL always
  fails both or did so because the two commits landed together is not known.
- **A doomed transaction does not read its own writes.** They were rolled back
  to the savepoint. It cannot commit, so nothing it reads can be acted on.
- **Some statements keep PostgreSQL's behavior.** A statement whose answer the
  emulator cannot reproduce exactly is not answered with a fabricated one: the
  conflict is reported where PostgreSQL raised it, with DSQL's wording and
  SQLSTATE. That covers `RETURNING`, `INSERT ... SELECT`, `ON CONFLICT`, and
  multi-statement simple queries. The same fallback catches a shadow that fails
  for any other reason, so nothing is ever answered from a guess.
- **`lock_timeout` applies to every statement**, not only to DML. A DDL that
  cannot take its lock in time is reported as a conflict too. DSQL does not
  wait for a DDL lock either, so this errs toward its model rather than
  PostgreSQL's, but the code is one DSQL would raise for a different reason.

The suite runs a case on several connections at once, so DSQL's conflict output
is recorded rather than assumed. The four conflicting cases are now replayed
against the emulator and enforced, marked `ConflictRace`: which transaction
loses is a race on both sides, so what is asserted is that as many transactions
lost, with the same SQLSTATE, at the step the conflict surfaces at.
Non-conflicting concurrency (disjoint writes, a non-key update against a
referencing insert) is replayed and enforced as before. Sessions step with a
fixed delay rather than barriers, and each step has a timeout so a blocking
target cannot stall a run.

### Foreign keys are an OCC source, not a reject rule

Aurora DSQL supports foreign keys (`NO ACTION`, `RESTRICT`, `CASCADE`,
`SET NULL`, `SET DEFAULT`; `MATCH FULL`/`MATCH SIMPLE`; deferrable). It enforces
them with snapshot verification plus commit-time adjudication by implicitly
applying `KEY SHARE` to referenced rows. Therefore:

- FK is a **conflict source** in the adjudicator, alongside write-write and
  `FOR UPDATE` / `FOR KEY SHARE`.
- Vanilla PostgreSQL enforces FKs with locks, so a concurrent
  delete-referenced-row / insert-referencing-row would block before resolving.
  The adjudicator removes the block: the FK's `KEY SHARE` lock on the parent is
  a write intent like any other, so the wait is bounded and reported at
  `COMMIT`. No deferred-constraint rewrite is needed.
- Conflict tracking is **key-column-aware** without the emulator implementing
  it. PostgreSQL's row-lock modes already draw that line: a non-key update
  takes `NO KEY EXCLUSIVE`, which does not conflict with the `KEY SHARE` a
  referencing insert takes, while a key change or a delete does. Both the
  conflicting and the non-conflicting FK probes are enforced against the
  emulator.

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
  `--server-version` (default `16.15`, what the cluster reports), and
  `SELECT version()` and `SHOW server_version` are rewritten to Aurora DSQL's
  values: `PostgreSQL 16` and `16.15`, the latter with the `SHOW` command tag.
  `current_setting('server_version')`, `current_setting('server_version_num')`,
  and `SHOW server_version_num` are rewritten too; a version read some other way
  still reports the backing engine, and the rewrites match whole statements
  only.
- **Backing engine version.** PostgreSQL 16, not the latest. The backing version
  is invisible on the rewritten paths, but the dialect is not: a newer engine
  accepts syntax Aurora DSQL rejects, turning "DSQL fails" into "emulator
  passes". 16 is the generation DSQL's dialect follows, and it still has
  everything the emulator needs (`GROUP BY DISTINCT`, deferrable foreign keys,
  generated identity, event triggers, PL/pgSQL).
- **Still to do.** The password is forwarded to the backing server, so an IAM
  auth token is accepted only to the extent the backing server accepts it.
  Accepting arbitrary tokens means the emulator must own the client
  authentication exchange instead of relaying it, and open its own upstream
  session with configured credentials.

## Container image

`Dockerfile` builds a single image that serves the whole emulator: PostgreSQL on
an internal port with the `docker/init` scripts applied, and the proxy in front
of it on 5432. `docker/init` is what provides `sys.jobs`, the row-cap trigger, and the `admin`
role clients connect as, so a container started from this image enforces the
same rules as the test suite with no extra setup.

`CREATE INDEX ASYNC` has to be rewritten before libpg_query will parse it,
because the grammar has no `ASYNC` keyword. The keyword is found with
PostgreSQL's own scanner rather than by matching text: the lexer reads it as an
ordinary identifier even though the grammar rejects the statement, so the token
stream gives its exact bounds. Everything after it, including a `WHERE`
predicate, an `INCLUDE` list, or `NULLS NOT DISTINCT`, is forwarded untouched,
and the qualified-name check happens before anything is sent.

Working from tokens is what makes a multi-statement simple query — what
`psql -c 'a; b'` sends — behave: the scanner knows where each statement ends, so
the keyword comes off wherever it sits and the dialect's own rules decide the
query, rather than PostgreSQL's parser refusing a keyword it has never heard of.
Two DDL in one such query are refused with `0A000` like any other pair. It also
keeps the word from being mistaken for the keyword inside a string literal, a
comment, or a quoted identifier, and settles where it is genuinely ambiguous:
`ALTER TABLE async ADD COLUMN b int` is a table named `async`, because every
`ALTER TABLE` action that can follow a name begins with a keyword, where the
dialect's form is followed by the table's name.

A simple query holding several statements answers with one result each, so the
emulator records which statement carried the keyword and splices the synthesized
`job_id` onto that statement's own `CommandComplete` rather than onto the first
one's.

`sys.jobs` records an `INDEX_BUILD` job for every `CREATE INDEX ASYNC`, because
an event trigger registered for `CREATE INDEX` fires for explicit index
creation but not for the index a `CREATE TABLE` makes for a key. Its columns,
lower-case statuses, and `sys.wait_for_job` being a procedure match what the
recording shows. The emulator picks the id, hands it to the client, and passes
it to the backing database in a marker comment on the statement itself
(`/* dsql_job=<uuid> */ CREATE INDEX ...`), so the id handed back is findable
without a round trip and each build gets its own id, as a real one does. The
marker is also what identifies an emulator-issued asynchronous `ALTER TABLE`,
so only the `ASYNC` form records a validation job. One deliberate difference
remains: DSQL additionally records `ANALYZE` and `DROP` jobs, which the emulator
does not.

- `DSQL_PORT` (default 5432) and `DSQL_PG_PORT` (default 5433) move the two
  listeners. `POSTGRES_USER`, `POSTGRES_DB`, and `POSTGRES_HOST_AUTH_METHOD`
  pass through to the backing database; trust is the default so a token works.
- Readiness: the emulator logs `proxy listening`, and the image has a
  `pg_isready` healthcheck on 5432.
- Build and publish with `make docker-build` and `make docker-push`
  (`IMAGE`/`TAG`, default `ghcr.io/dreamescaper/dsql-emulator:latest`). The
  `image` workflow publishes `linux/amd64` and `linux/arm64` to GHCR only when
  a release is created, using the repository's own token, and tags the image
  with the release version (for example `1.2.3`) plus `latest` for a
  non-prerelease. It can also be dispatched with a tag to re-publish one.
  Pushes to `main` do not publish.
  with the release version (for example `1.2.3`) plus `latest` for a
  non-prerelease. Pushes to `main` do not publish.
- Cut a release from the Actions tab with the `release` workflow and a
  `patch`/`minor`/`major` bump. It verifies the build and tests, computes the
  next version from the latest `v*` tag, creates the release with the default
  token, then **calls the `image` workflow directly** — a release created with
  `GITHUB_TOKEN` does not emit an event other workflows see, so relying on
  `release: published` there would publish nothing. No personal access token is
  needed. With no tags yet, the first release is `v0.1.0`.

With testcontainers-go:

```go
c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
    ContainerRequest: testcontainers.ContainerRequest{
        Image:        "ghcr.io/dreamescaper/dsql-emulator:latest",
        ExposedPorts: []string{"5432/tcp"},
        WaitingFor:   wait.ForLog("proxy listening"),
    },
    Started: true,
})
host, _ := c.Host(ctx)
port, _ := c.MappedPort(ctx, "5432")
dsn := fmt.Sprintf("postgres://admin:an-iam-token@%s:%s/postgres?sslmode=require",
    host, port.Port())
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
- DML row cap per transaction (3000). A row trigger in the backing database
  counts modifications per transaction and raises `54000` on the statement that
  crosses the cap, so that statement fails and nothing commits — for implicit
  and explicit transactions alike. The emulator passes the cap through the
  startup options as `dsql.row_cap`, and PostgreSQL's own aborted-transaction
  state then produces `25P02` and the `ROLLBACK` tag on COMMIT.
- Transaction age limit (30 minutes) → `54000`.

Failed transactions:

- Any refusal inside an explicit transaction fails it, as on a real server.
  Later statements are refused with `25P02` until the client ends it.
- The upstream transaction is failed by sending a deliberately failing
  statement. PostgreSQL then answers COMMIT with the `ROLLBACK` command tag and
  refuses later statements by itself, so no response rewriting is needed.
- ROLLBACK always ends a failed transaction, and a client can always escape.

The row cap is the one rule enforced in the storage engine rather than the
proxy, because the row count is only known while the statement runs. It needs
the backing database's `docker/init/02-rowcap.sql`, which the compose file and
the container-backed tests install.

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
  lock_timeout_ms: 50
  inject: []
```

The ruleset is refused rather than loaded when a rule could not do anything: a
rule with no id, code or message, a duplicate id, and the same for an `occ.inject`
entry, whose id must be present and unique, whose `every` may not be negative,
and whose tables may not be blank. An injection rule that is quietly ignored is
worse than one that is refused, because the transaction it was meant to fail
commits and the retry loop under test never runs. Unknown keys are refused too,
so a misspelled setting is reported rather than defaulted.

Available predicates: `relpersistence`, `column_type`, `column_array`,
`objtype`, `txn_kind`, `set_name`, `show_name`, `language_not`,
`sequence_cache_min`, `identity_cache_min`, `cache_allow`, `remove_type`,
`rename_type`, `locking`, `function`, `contains`, and `vacuum_kind`. `function`
and `contains` walk the whole parse tree, so a construct nested in an
expression or subquery is still found. The `since` field is reserved for version-gating a rule, and
`rewrites` are textual pre-parses for syntax libpg_query cannot read. Foreign keys carry no rule:
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

A case may set `SimpleProtocol`, which sends each step as a simple query rather
than through the extended protocol. That is the only way a step can hold more
than one statement — what `psql -c 'a; b'` sends — and the extended protocol
carries one statement per Parse, so nothing else in the suite reaches that path.
Every statement's answer is recorded, because the interesting one is rarely the
first: a `BEGIN; CREATE INDEX ASYNC ...; COMMIT` has to show the `job_id` on the
index build's own result rather than on the `BEGIN`.

The comparison enforces outcome, SQLSTATE, command tag, and rows, and for a
multi-statement step each of those per statement. Error *messages* are reported
but advisory, because server wording drifts; every recorded message matches
nonetheless, so a message note in a run is a change worth reading. A case may
set `IgnoreRows` (generated ids, `version()`) or `KnownGap` (an accepted
divergence); known gaps are reported and not enforced. Cases the emulator runs
that the record does not cover are listed as unrecorded, so a probe added since
the last baseline is never silently unverified. The emulator's copy of a case
supplies step text and suite metadata, so editing the suite takes effect without
re-recording, and what is replayed is the suite's decision rather than the
record's.

A recording writes only the fixtures whose content changed, where "content" is
what the record holds the emulator to rather than everything the run saw. A run
observes things that differ every time and are enforced against nothing: a
generated job id in a case marked `IgnoreRows`, and which of two transactions
lost a `ConflictRace`. Change detection waives exactly what the comparison
waives, so it is as sensitive as the replay it guards and no more. Re-recording is how
you find out whether the cluster still answers the same way, and usually it
does; a save that rewrote every fixture anyway would put a fresh timestamp in
every diff and bury the runs that found something. A fixture whose group
answered exactly as before is left on disk untouched, `recorded_at` included, so
the record's git history is a list of the runs that changed it. Comparison is on
content with the timestamp cleared, rendered through the same marshaller on both
sides, so a field added to the record later counts without an equality function
to keep in step. That a run happened at all is reported by the recorder, to be
logged in `PROGRESS.md` where runs belong.

`TestRerecordingAnUnchangedClusterWritesNothing` holds this to the committed
record itself, without a cluster or Docker: the recorded observations are put
back into the order a run produces them in, saved with a later timestamp, and
every fixture must come out byte-identical. The 2026-09-17 run bore it out: of
thirteen fixtures, ten were left untouched, two differed only in generated job
ids, and one carried a real change.

Safety, because the target is someone's cluster:

- every object is prefixed `baseline_` and dropped by `Suite.Cleanup`, which
  runs even when the run fails;
- every case is followed by a ROLLBACK, so a case that leaves a transaction open
  or aborted cannot poison the next one;
- the suite is roughly 70 statements, which keeps cost negligible;
- `--dry-run` prints the whole suite without connecting.

Re-run `make baseline` whenever DSQL changes; the record is a snapshot, not a
fixture to keep forever. A run that changes nothing costs only the run.

## Milestones

| ID | Deliverable | Status |
|----|-------------|--------|
| M0 | Wire proxy passthrough, 1:1 pinning, pgx round-trip test | done |
| M1 | AST classifier + rejection with real SQLSTATEs, versioned YAML rules | done |
| M2 | Session FSM: RR enforcement, 1-DDL, DDL/DML split, row cap, age | done |
| M3 | Transaction coordinator: backend rollback, aborted-transaction state | done |
| M4 | Auth/TLS/version emulation; single DB; UTC/C collation | done (tokens accepted via a trust-backed upstream, not validated) |
| M5 | OCC modes 1 + 2 + 3, OCC error codes, FK conflict fixtures | done |
| M6 | `CREATE INDEX ASYNC` rewrite + `sys.jobs` / `sys.wait_for_job` | done |
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
internal/occ/            conflict codes, intents, and shadow statements (M5)
docker/init/             backing init: sys.jobs, row cap, admin role (M6)
rules/                   embedded versioned ruleset and loader
test/integration/        container-backed tests
test/conformance/        emulator-vs-golden tests, golden/<group>.json (M7)
```

## Verification backlog

Answered by baseline runs on 2026-09-15 and 2026-09-16. The ruleset was
reconciled to match, and the emulator reproduces every recorded case. Rows
marked **Open** are questions no recording has answered yet:

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
| Job id shape | **Open.** Two recorded `sys.jobs` rows carry `yoeqoh5bcjgw7kcmwihktdbtgq` and `tpqrncdmjja4tdl3zxo2qqvh4y` — 26 characters each, which is what base32 of sixteen bytes looks like, not a dashed UUID. `CALL sys.wait_for_job('no-such-job')` answering `22P02` "Unable to convert text to UUID" says the id is decoded rather than compared as text. So DSQL appears to render a UUID in base32, and nothing observed confirms whether it would accept the dashed form. The emulator issues dashed UUIDs, which stay consistent with the `uuid` cast that reproduces the verified `22P02`; `sys_jobs_columns` sets `IgnoreRows`, so no probe pins the shape. Settling it needs a probe that calls `wait_for_job` with a real id, which cannot be written while the only observed call returns `42809`. |
| Types in `ALTER TABLE ADD COLUMN` | Answered 2026-09-16. The `CREATE TABLE` type lists apply: `money` is `0A000 datatype money not supported`, `text[]` is `0A000 datatype text[] not supported` — so an array is just another unsupported datatype there, not a separate rule — and `serial` is `42704 type "serial" does not exist`, the same split `CREATE TABLE` shows. |
| `ALTER TABLE ALTER COLUMN ... TYPE` | Answered 2026-09-16. Refused outright with `0A000 unsupported ALTER TABLE ALTER COLUMN ... SET DATA TYPE statement`, whatever the target type: `xml` and `varchar(20)` return the identical message, and it names no type where the `ADD COLUMN` refusal does. Rule `alter_column_type` added; the emulator previously allowed a retype to a supported type. |
| Query-runtime types | Arrays and `inet` work in expressions even though they cannot be columns. |
| Row locking | `FOR UPDATE` and `FOR KEY SHARE` are accepted; `FOR SHARE` and `FOR NO KEY UPDATE` are refused with `0A000`. Rules added. |
| Query features | Joins, set operations, `GROUP BY`/`HAVING`/`DISTINCT`, `ORDER BY ... NULLS`, `LIMIT`, CTEs, scalar/`IN`/`EXISTS`/correlated subqueries, `unnest`, `generate_series`, aggregates, `RETURNING`, upsert, `INSERT ... SELECT`, `EXPLAIN`, `ANALYZE`, and `SET CONSTRAINTS` all work. `RANK() OVER (PARTITION BY ...)` works; so do `GROUPING SETS`, `ROLLUP`, `CUBE`, `LATERAL`, `DISTINCT ON`, `ROW_NUMBER`, `LAG`, `WITH RECURSIVE`, and aggregate `FILTER`, which the documentation does not list. |
| `GROUP BY ALL` | The documentation lists it, but DSQL answers `42601 syntax error`, so the emulator's parse failure matches. |
| Text search | `to_tsvector`, `to_tsquery`, `websearch_to_tsquery`, and `@@` fail as `42704 text search configuration "english" does not exist`. Rule added. |
| Geometric functions | `line`, `circle`, and friends are refused with `0A000 datatype not supported`. Rule added. |
| `MERGE` and `TABLESAMPLE` | Refused with `0A000`. Rules added. |
| `SHOW lc_collate` | Refused with `42704 unrecognized configuration parameter`. Rule added. |
| `server_version` | `16.15`; `version()` returns `PostgreSQL 16`. |
| Enums | No user-defined types exist. `CREATE TYPE`, `ALTER TYPE` (add value and rename), and `DROP TYPE` are all refused with `0A000`; a column or cast naming one fails as `42704`. The workarounds work: a `text` column with a `CHECK (m IN (...))`, or a `CREATE DOMAIN ... CHECK (...)` whose domain is supported; a bad label raises `23514`. Rules added for the three statements. |
| `server_version` | `PostgreSQL 16`. |
| `ALTER TABLE` | `DROP COLUMN`, `ADD COLUMN ... STORAGE`, `SET STORAGE`, `ADD CONSTRAINT ... NOT VALID`, `RENAME`, and `SET SCHEMA` all match. A `CHECK` or `FOREIGN KEY` added by `ALTER TABLE` without `NOT VALID` is refused with `0A000 unsupported ALTER TABLE ADD CONSTRAINT statement`, and `VALIDATE CONSTRAINT` only through the `ASYNC` form (`0A000 unsupported ALTER TABLE VALIDATE CONSTRAINT statement` otherwise); the `ASYNC` form returns a `job_id`. Two divergences remain: dropping a primary-key column is refused by DSQL (needs catalog knowledge) and an index built by `CREATE INDEX ASYNC` is immediately valid, where DSQL's is still building, so `ADD CONSTRAINT ... UNIQUE USING INDEX` fails there with `55000`. |
| `ALTER TABLE` | `DROP COLUMN`, `ADD COLUMN ... STORAGE`, `SET STORAGE`, `ADD CONSTRAINT ... NOT VALID`, `RENAME`, and `SET SCHEMA` all match. A `CHECK` or `FOREIGN KEY` added by `ALTER TABLE` without `NOT VALID` is refused with `0A000 unsupported ALTER TABLE ADD CONSTRAINT statement`, and `VALIDATE CONSTRAINT` only through the `ASYNC` form (`0A000 unsupported ALTER TABLE VALIDATE CONSTRAINT statement` otherwise); the `ASYNC` form returns a `job_id`. `ALTER COLUMN ... TYPE` is refused whatever the target type, and the type lists apply to `ADD COLUMN`. Dropping a primary-key column is refused too, enforced by a guard in the backing database that reads object addresses rather than the statement, so the multi-action form is covered as well. One divergence remains: an index built by `CREATE INDEX ASYNC` is immediately valid here where DSQL's is still building, so `ADD CONSTRAINT ... UNIQUE USING INDEX` fails there with `55000`. |
| Partial indexes | Supported. `CREATE INDEX ASYNC ... WHERE`, index expressions, `INCLUDE`, `NULLS NOT DISTINCT`, and unnamed indexes all succeed with command tag `CREATE INDEX`. A schema-qualified index name is a syntax error (`42601`) because DSQL always puts the index in the table's schema; the emulator refuses it before forwarding, since PostgreSQL would accept it. |
| Rejection message text | Recorded verbatim in the golden file. |

Answered by the concurrency probes (recorded 2026-09-15):

| Question | Answer |
|----------|--------|
| When does the loser fail? | At `COMMIT`, not at the statement: both writers' `UPDATE`s succeed and the second committer is rejected. The emulator now reproduces this. |
| Error wording | `40001 change conflicts with another transaction (OC000)` — identical to what the emulator synthesizes. |
| `FOR UPDATE` versus a write | Conflicts; the `FOR UPDATE` session committed first and the writer's commit failed. |
| `FOR KEY SHARE` versus deleting the key | Conflicts at commit. |
| Foreign key delete/insert | Conflicts at commit, as the documentation's example shows. |
| Non-key update versus a referencing insert | No conflict. |
| Writes to different rows | No conflict. |

Answered by the local measurements on 2026-09-17, which do not need a cluster:

| Question | Answer |
|----------|--------|
| Can a `BEFORE ROW` trigger intercept a conflict before the block? | No. `GetTupleForTrigger` locks the tuple before the trigger fires, so the trigger never runs; a database-side registry cannot avoid the wait. |
| What does a bounded wait cost? | A conflicting write that blocked for ~3s returns in ~50ms as `55P03`, and the transaction survives a rollback to a savepoint with its snapshot intact. |
| Does a non-key update block a referencing insert under a bounded wait? | No: `NO KEY EXCLUSIVE` and `KEY SHARE` do not conflict, so the insert commits in under a millisecond. |

Answered by the baseline run on 2026-09-17:

| Question | Answer |
|----------|--------|
| Do DSQL's row counts for a losing statement match the emulator's shadow? | Yes for the probed shapes: the loser reports `UPDATE 1`, `DELETE 1`, `INSERT 0 1` and `SELECT 1` exactly as the shadow synthesizes them, and the emulator matches all 212 cases against the run. |
| Is the loser a race, or does DSQL pick deterministically? | A race. `occ_fk_delete_insert` failed the other session than the 2026-09-16 run did, from the same probe. |

Answered by the multi-statement probes (recorded 2026-09-17):

| Question | Answer |
|----------|--------|
| Does DSQL run a multi-statement simple query at all? | Yes, and it answers once per statement: `SELECT 1 AS a; SELECT 2 AS b` comes back as two results with their own columns. |
| Two DDL in one such query? | `0A000 multiple ddl statements not supported in a transaction` — the query is one implicit transaction, exactly as a multi-step case is. |
| DDL and DML in one? | `0A000 ddl and dml are not supported in the same transaction`. |
| A table and its `CREATE INDEX ASYNC` in one query? | `0A000 multiple ddl statements not supported in a transaction`, which is what the emulator now reports where it used to forward the keyword and collect a syntax error. |
| `BEGIN; CREATE INDEX ASYNC ...; COMMIT` in one query? | Accepted, and the `job_id` comes back on the index build's own result, not on the `BEGIN`. |
| `BEGIN; INSERT ...; COMMIT` in one query? | Accepted, three results. |

Answered by the conflict probes recorded on 2026-09-17:

| Question | Answer |
|----------|--------|
| Does a referential action conflict through the child row it rewrites? | Yes, and the same way for all three: `ON DELETE CASCADE`, `SET NULL` and `SET DEFAULT` each fail the parent delete against a concurrent write to the child. The emulator matches, because PostgreSQL's action takes the child's row lock. |
| What row count does a loser report when its predicate spans several rows? | The true count under its own snapshot, `UPDATE 3` for three rows, which is what the shadow computes. |
| Who loses when the conflict spans several rows? | **Both.** DSQL failed every side of it, where the emulator leaves a winner. Recorded as a known gap; see below. |

Nothing in the backlog is unanswered. The suite is the place to add the next
question.

## Prior art

- [tomodian/deesql](https://github.com/tomodian/deesql) — PG-wire proxy, rejects
  unsupported SQL with real SQLSTATEs, rewrites `CREATE INDEX ASYNC`, handles IAM
  token auth.
- [renebrandel/dsql-local-simulator](https://github.com/renebrandel/dsql-local-simulator)
  — Postgres extension that blocks unsupported DDL.
- [LocalStack DSQL](https://docs.localstack.cloud/aws/services/dsql/) — control
  plane APIs plus an embedded-Postgres data plane, including `sys.jobs`.
