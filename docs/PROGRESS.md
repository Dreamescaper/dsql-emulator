# Progress

Status log for the Aurora DSQL emulator. Append newest work at the top of
[Completed](#completed). The forward-looking design lives in
[PLAN.md](./PLAN.md).

## Current status

**Every milestone is done, the record is fresh, and the verification backlog is
empty.** Every refusal the record
holds matches Aurora DSQL's wording, not only its SQLSTATE. The OCC adjudicator
handles parameterised statements, which is the shape application code writes. A multi-statement simple
query spelled with `CREATE INDEX ASYNC` is now answered by the dialect's rules
rather than by a PostgreSQL syntax error. The baseline was
re-recorded against the cluster on 2026-09-17 and the emulator matches all 218
cases against it, the adjudicator included. A baseline run now rewrites only the
fixtures whose answers changed — measured by what the record enforces, so a
generated job id or a flipped race does not count — and that run touched one
fixture. M5 closed with the OCC adjudicator: conflicts are
resolved at `COMMIT` without waiting for locks, across write-write,
`FOR UPDATE`, `FOR KEY SHARE` and foreign-key overlap. The four conflicting
conformance probes are no longer record-only — they are replayed against the
emulator and enforced. The golden record holds all 218 probes the suite
defines; the emulator matches every one except the accepted
`alter_unique_using_index` gap, and nothing is unrecorded. Refusals inside a
transaction fail it exactly as Aurora DSQL does, `CREATE INDEX ASYNC` and
`sys.jobs` are implemented. Three mechanisms that read SQL as text now read
structure instead, which closed the limitations that came with them.

Last updated: 2026-09-17.

## How to run

```sh
make up          # start PostgreSQL 16 (host port 5433)
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

### Recorded the conflict probes: both questions answered, one divergence found (2026-09-17)

Ran `dsql-baseline` against the cluster in `eu-central-1`. 222 cases recorded,
48 objects dropped, no cleanup skips; the referential-action fixtures applied
cleanly, so the setup risk flagged last time did not materialise.

**Referential actions conflict, all three the same way.** `ON DELETE CASCADE`,
`SET NULL` and `SET DEFAULT` each fail the parent delete against a concurrent
write to the child the action would rewrite. The emulator matches all three,
because PostgreSQL's action takes the child's row lock and the adjudicator reads
that as the conflict it is.

**A multi-row conflict can leave no winner.** Both sessions updated the same
three rows behind `k = 1`. DSQL reported `UPDATE 3` on both sides — the row
count a loser reports is the true count under its own snapshot, which is what
the shadow computes — and then **failed both transactions**. The emulator fails
one. It cannot do otherwise: it adjudicates on the backend's row locks, so
whichever transaction takes them first is by construction able to commit.

That is recorded as a `KnownGap` rather than chased. Reproducing it means
modelling DSQL's adjudication instead of borrowing PostgreSQL's, which is the
premise the whole layer rests on. It is also one sample: whether DSQL always
fails both, or did so because the two commits landed together, is not known.
The probe is what makes the question askable, and the gap is now visible in
every conformance run rather than assumed away.

**The run also caught a bug in the change detection.** `multi_statement` was
rewritten for nothing but a regenerated job id, in a case marked `IgnoreRows`.
The waiver added on 2026-09-17 clears `Observation.Rows`, but a multi-statement
step keeps its rows in `Observation.Results[n].Rows`, which the waiver was never
taught about when `Results` was added later the same day. Fixed, with a test,
and the fixture restored to what the fixed code would have left.

Files: `internal/conformance/suite.go`, `internal/conformance/record.go`,
`internal/conformance/conformance_test.go`,
`test/conformance/golden/occ_conflict.json`, `docs/PLAN.md`, `README.md`.

**Verification.**

```
Recorded 222 cases against aurora-dsql
Changed 2 fixture(s) in test/conformance/golden: multi_statement, occ_conflict

$ make conformance
    known gap alter_unique_using_index: ...
    known gap occ_multirow_predicate: conflict_losers step 0: golden=2 emulator=1
    conformance_test.go:114: 222 cases match the golden record

$ make build && make vet && make test && make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	5.436s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	8.298s
```

After the waiver fix only `occ_conflict` was a real change; `multi_statement`
was restored, so the run's lasting effect on the record is the four new probes.

**A note on the token.** The file was the one from the previous run with about
150 seconds left rather than a fresh one. The run fit, but the concurrency
probes open new connections late, so a longer suite would not have. Worth
checking `X-Amz-Date` before the next one.

### Probes for the two open backlog questions (2026-09-17)

Both questions PLAN.md carried were unanswerable because nothing probed them.
Four probes and their fixtures close that, and run against the emulator; the
answers need a metered run.

**Do referential actions conflict?** The FK conflict probe deletes a referenced
row against a referencing insert, on a plain foreign key. These ask the other
half: a referential action rewrites the *child* row, so does that conflict with
a concurrent write to it? `baseline_fk_action` gets a parent row per action and
a child table per action -- `ON DELETE CASCADE`, `SET NULL`, and `SET DEFAULT`,
the last defaulting to a parent row nothing deletes, so the action has somewhere
to point. Each probe deletes the parent in one session and updates the child in
the other.

The emulator conflicts in all three, failing the parent delete, which is what
PostgreSQL's locking gives: the action takes the child's row lock. Whether DSQL
agrees is the question.

**What row count does a loser report?** Every other conflict probe matches one
row by primary key, so the shadow that counts a refused statement's rows has
never been tested against more than one. `baseline_span` holds three rows behind
`k = 1`, and `occ_multirow_predicate` has both sessions update all three. The
emulator reports `UPDATE 3` for the loser.

All four are marked `ConflictRace`, which is safe whichever way the answer goes:
it asserts how many transactions lost, so a pair that turns out not to conflict
compares zero against zero.

Files: `internal/conformance/suite.go`, `docs/PLAN.md`.

**Verification.** The probes run against the emulator, the setup applies with no
failures and no cleanup skips, and the recorded 218 still match:

```
recorded occ_fk_cascade_vs_child_write     session 0: BEGIN | DELETE 1 | error 40001 || session 1: BEGIN | UPDATE 1 | COMMIT
recorded occ_fk_set_null_vs_child_write    session 0: BEGIN | DELETE 1 | error 40001 || session 1: BEGIN | UPDATE 1 | COMMIT
recorded occ_fk_set_default_vs_child_write session 0: BEGIN | DELETE 1 | error 40001 || session 1: BEGIN | UPDATE 1 | COMMIT
recorded occ_multirow_predicate            session 0: BEGIN | UPDATE 3 | error 40001 || session 1: BEGIN | UPDATE 3 | COMMIT
    conformance_test.go:114: 218 cases match the golden record
```

`make build`, `make vet`, `make test`, `make test-integration` and `-race` all
pass. The golden record is untouched.

**Not done: the probes are unrecorded**, so both questions are still open. The
answers above are the emulator's, not a cluster's.

**One risk in the next run.** The new fixtures put `ON DELETE CASCADE`,
`SET NULL` and `SET DEFAULT` in `Suite.Setup`, and a setup statement that fails
aborts the whole run before any probe executes. The documentation lists all
three as supported and the emulator accepts them, so this is unlikely; if it
happens the run writes nothing and the record is left as it was, costing a token
and reporting which statement failed.

### Refusal messages mirror Aurora DSQL's wording (2026-09-17)

Messages were advisory in the comparison and had been left to drift, on the
grounds that the SQLSTATE is the contract. The fresh record showed 39 of them
saying something other than what the cluster says. All 39 now match; the
comparison still treats messages as advisory, but nothing in the record differs,
so a message note in a future run is a real change rather than background noise.

Most of the gap was one missing capability: **DSQL names the thing it refused**,
and the ruleset could only state a fixed sentence. A rule's message may now carry
`{type}` or `{language}`, filled from the statement that was refused. The type is
spelled the way PostgreSQL displays it rather than the way the statement wrote
it, which is what the record shows: `varbit(8)` is refused as
`datatype bit varying not supported` and `int[]` as `datatype integer[] not
supported`. Only the aliases the parser always rewrites are mapped; a type
written as its displayed name needs no entry.

That also resolved an inconsistency the emulator had invented. An array column
was refused as `array columns are not supported`, but DSQL treats an array as
just another unsupported datatype and names it, so the two array rules now carry
the same message as every other type rule.

The rest were plain wording, in the ruleset and in `txn.go`:

| what | Aurora DSQL |
|------|-------------|
| two DDL in a transaction | `multiple ddl statements not supported in a transaction` |
| DDL with DML | `ddl and dml are not supported in the same transaction` |
| `FOR SHARE` / `FOR NO KEY UPDATE` | `locking clauses other than FOR UPDATE/FOR KEY SHARE are not supported` |
| `ALTER TYPE ... ADD VALUE` | `ALTER TYPE not supported` |
| `ALTER TYPE ... RENAME` | `unsupported object in RENAME statement` |
| sequence and identity cache | `... cache size. please define CACHE ...` |
| a non-SQL function language | `CREATE FUNCTION with language plpgsql not supported` |

One came from the backing database rather than the ruleset: `sys.wait_for_job`
cast its argument to `uuid` and let PostgreSQL's message through, which names the
offending value. The cast is now wrapped so the refusal carries DSQL's
`Unable to convert text to UUID` with the same SQLSTATE.

Files: `internal/classify/message.go` (new), `internal/classify/classify.go`,
`internal/classify/classify_test.go`, `internal/txn/txn.go`,
`rules/dsql-2026.09.yaml`, `docker/init/01-sys.sql`, `README.md`, `docs/PLAN.md`.

**Verification.** A conformance run reported 39 message notes before and none
after, with `218 cases match the golden record` throughout. The golden record was
not touched: this changes what the emulator says, not what was recorded.

```
$ make build && make vet && make test && make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	4.050s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	8.187s
```

**Note for anyone running an old container.** The `sys.wait_for_job` change is
in an init script, so it applies to a database created after it. An existing one
keeps PostgreSQL's wording until it is recreated.

### Recorded the multi-statement probes; every inference held (2026-09-17)

Ran `dsql-baseline` against the cluster in `eu-central-1` with a fresh token,
through `--token-file`. 218 cases recorded, 43 objects dropped, no cleanup
skips. **The emulator matches all 218**, with the single accepted
`alter_unique_using_index` gap and nothing unrecorded.

The six `multi_statement` probes had never been answered by a cluster, and what
the emulator did with them was inferred from its own transaction rules. The
cluster agreed with every one:

| probe | Aurora DSQL |
|-------|-------------|
| `SELECT 1 AS a; SELECT 2 AS b` | two results, own columns each |
| two `CREATE TABLE` | `0A000 multiple ddl statements not supported in a transaction` |
| `CREATE TABLE` then `INSERT` | `0A000 ddl and dml are not supported in the same transaction` |
| `CREATE TABLE` then `CREATE INDEX ASYNC` | `0A000 multiple ddl statements not supported in a transaction` |
| `BEGIN; CREATE INDEX ASYNC ...; COMMIT` | accepted; `job_id` on the index build's own result |
| `BEGIN; INSERT ...; COMMIT` | accepted, three results |

Two of those are worth naming. The fourth is the exact case the README carried
as a limitation this morning, and it confirms the fix reports what DSQL reports
rather than merely something better than a syntax error. The fifth confirms the
job-id splicing: DSQL puts the id on the second result, which is where the
emulator puts it, and the probe would have caught it on the `BEGIN`.

**The save hygiene held up under the case it was built for.** A run that added
six probes wrote exactly one fixture:

```
Recorded 218 cases against aurora-dsql
Changed 1 fixture(s) in test/conformance/golden: multi_statement
```

Twelve fixtures were left untouched, timestamps included, through a run that
regenerated every `sys.jobs` id and re-raced every conflict probe. `recorded_at`
now reads 2026-09-16 on ten of them, 2026-09-17T11:20 on `occ_conflict`, and
2026-09-17T14:54 on `multi_statement` — one date per run that changed something.

Files: `test/conformance/golden/multi_statement.json` (new), `docs/PLAN.md`,
`README.md`.

**Verification.**

```
$ make conformance
    conformance_test.go:114: 218 cases match the golden record
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	3.837s

$ make build && make vet && make test && make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	3.994s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	7.803s
```

**Correction.** An earlier note said recording these probes would also settle
PLAN.md's two open backlog questions. It did not: neither has a probe, so both
are still open. The run answered the multi-statement questions only, which are
now recorded in PLAN.md's backlog.

### The suite can probe multi-statement queries (2026-09-17)

`Observe` ran every step through the extended protocol, which carries one
statement per Parse, so no probe could send `a; b` and nothing recorded what
Aurora DSQL answers for one. That was a hole under the multi-statement ASYNC
work shipped earlier today: the emulator's behavior there was inferred from its
own recorded transaction rules rather than measured.

A case can now set `SimpleProtocol`, which sends each step through
`PgConn().Exec(...).ReadAll()` instead. `Observation` gains `Results`, one entry
per statement, because the interesting answer is rarely the first one — and the
comparison enforces the tag, columns and rows of each.

Six probes were added as the `multi_statement` group: two `SELECT`s (does a
multi-statement query run at all, and answer once per statement), two DDL, DDL
then DML, a table and its `CREATE INDEX ASYNC` in one query, the same index
build inside `BEGIN`/`COMMIT`, and an explicit transaction of DML.

Files: `internal/conformance/record.go`, `internal/conformance/suite.go`,
`internal/conformance/compare.go`, `internal/conformance/conformance_test.go`,
`docs/PLAN.md`, `README.md`.

**Verification.** The probes run against the emulator, and the harness captures
what they exist to capture — the `job_id` rides on the index build's own result,
not on the `BEGIN`:

```
recorded multi_two_selects            SELECT 1 + SELECT 1
recorded multi_two_ddl                error 0A000
recorded multi_ddl_then_dml           error 0A000
recorded multi_ddl_then_async_index   error 0A000
recorded multi_async_index_in_txn     BEGIN + CREATE INDEX + COMMIT
recorded multi_dml_in_txn             BEGIN + INSERT 0 1 + COMMIT

multi_async_index_in_txn => [{"outcome":"ok","results":[
  {"command_tag":"BEGIN"},
  {"command_tag":"CREATE INDEX","columns":["job_id"],"rows":[["77014860-..."]]},
  {"command_tag":"COMMIT"}]}]
```

`make build`, `make vet`, `make test`, `make test-integration` all pass, and the
existing `212 cases match the golden record`.

**Not done: the probes are unrecorded.** `make conformance` reports them as
`6 case(s) not covered by the golden record`, which is a log rather than a
failure, by design. Until a baseline run answers them, what DSQL does with a
multi-statement query is still unknown — the mechanism to find out exists, the
answer does not. The run needs a fresh token and costs money.

### The adjudicator handles parameterised statements (2026-09-17)

A conflict on `UPDATE t SET v = $1 WHERE id = $2` was reported at the statement
rather than at `COMMIT`, because the shadow that answers a refused statement is
a different statement and could not carry the client's bound values. That left
the shape application code actually writes as the one shape the adjudicator
declined, in a feature whose point is letting an application test its retry loop.

The plan was to capture parameter type OIDs from the backend's
`ParameterDescription` so a shadow could declare them. **That turned out to be
unnecessary.** The problem it solved is that PostgreSQL cannot infer a type for a
parameter the shadow no longer mentions — `SELECT count(*) FROM t WHERE id = $2`
leaves `$1` declared and unused, which is an error. Renumbering removes the
problem instead of working around it: the shadow keeps only the parameters it
still refers to, renumbered from `$1`, and records which of the client's
positions those were. `UPDATE t SET v = $1 WHERE id = $2` becomes
`SELECT count(*) FROM t WHERE id = $1`, bound with the client's second value.

No parameter types are declared at all. Every parameter a shadow keeps sits in
the expression it was already used in — the `WHERE` clause is carried over
verbatim — so it is inferred from the same context as in the statement that was
refused. This also means no new protocol machinery: no `Describe` tracking, no
`ParameterDescription` interception, no per-statement type cache.

Files: `internal/occ/occ.go`, `internal/occ/occ_test.go`,
`internal/proxy/adjudicator.go`, `internal/proxy/adjudicator_test.go`,
`internal/proxy/session.go`, `test/integration/occ_test.go`, `docs/PLAN.md`,
`README.md`.

**Verification.** `TestOccAdjudicatesParameterisedStatements` runs four shapes
against a container, with pgx binding integers in binary format: a parameterised
`UPDATE`, one with several predicate parameters renumbered together, a
parameterised `DELETE`, and a parameterised `SELECT ... FOR UPDATE` that must
still return its row. Each loser's statement succeeds with the right command
tag, does not block, and fails at `COMMIT` with `40001`.
`TestSessionRunsAParameterisedShadowWithTheBoundValues` pins the wiring at the
protocol level: the backend receives the renumbered shadow and a Bind carrying
the row's id, not the value the `SET` list dropped.

```
$ make build && make vet && make test
ok  	github.com/Dreamescaper/dsql-emulator/internal/occ	0.682s
ok  	github.com/Dreamescaper/dsql-emulator/internal/proxy	1.779s

$ make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	21.187s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	25.015s

$ go test -race -count=1 ./...
(no races)
```

`TestSessionReportsTheConflictWhenTheShadowFails` was added alongside: it covers
the fallback every remaining limitation rests on, which had been claimed but
never tested. A shadow that cannot run leaves the conflict reported where
PostgreSQL raised it, with DSQL's wording, and the transaction failed.

**Deliberate limitation.** A parameter used both in a clause the shadow drops
and one it keeps could be inferred as a different type than the client encoded
it for. The shadow then fails and the conflict is reported at the statement,
which is the behavior the statement would have had anyway.

### The ASYNC keyword comes off with the scanner, not a regex (2026-09-17)

A multi-statement simple query holding `CREATE INDEX ASYNC` — what
`psql -c 'a; b'` sends — reached PostgreSQL with the keyword still on it and
came back as a syntax error, where Aurora DSQL answers with its own rule. The
two regexes that stripped the keyword were anchored to the start of the string,
so only a lone statement ever matched.

Both are replaced by PostgreSQL's own scanner. `pg_query.Scan` tokenizes a
string the grammar will not parse — to the lexer `ASYNC` is an ordinary
identifier — so the token stream gives the keyword's exact bounds *and* the
statement boundaries. The keyword now comes off wherever it sits, the query
parses, and the dialect's rules decide it: two DDL in one query are refused with
`0A000` like any other pair.

**Two bugs fell out of it**, both present in the regexes:

- `ALTER TABLE async ADD COLUMN b int` — a table actually named `async` — was
  mangled into `ALTER TABLE ADD COLUMN b int`. The token shape settles it: every
  `ALTER TABLE` action that can follow a name begins with a keyword, where the
  dialect's form is followed by the table's name. A test for it is what caught
  this.
- The word inside a string literal, a comment, or a dollar-quoted body was
  matchable in principle; the scanner cannot confuse those for a keyword.

A simple query answers with one result per statement, so `jobResult` now records
how many results precede the asynchronous one and `claimJob` counts them down.
Without it the synthesized `job_id` row would have spliced onto the `BEGIN` of
`BEGIN; CREATE INDEX ASYNC ...; COMMIT` — a quieter wrong answer than the syntax
error it replaced, which is why it was worth doing rather than deferring.

Files: `internal/proxy/rewrite.go`, `internal/proxy/rewrite_test.go`,
`internal/proxy/session.go`, `internal/proxy/session_test.go`,
`test/integration/async_test.go`, `docs/PLAN.md`, `README.md`.

**Verification.** `TestMultiStatementAsyncQuery` drives the real path against a
container: two DDL in one query and DDL mixed with DML are both refused with
`0A000`, and `BEGIN; CREATE INDEX ASYNC async_idx ON async_t (a); COMMIT` is
accepted, returns three results, carries the `job_id` on the second one, builds
the index, and records a `completed` job under that id.

Reading all three results needs `PgConn().Exec(...).ReadAll()`: pgx's own
`Query` surfaces only the first result of a multi-statement query, which is what
made the first version of the test fail while the emulator was already correct.

```
$ make build && make vet && make test
ok  	github.com/Dreamescaper/dsql-emulator/internal/proxy	1.110s
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	0.567s

$ make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	4.004s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	6.521s

$ go test -race -count=1 ./...
(no races)
```

**Deliberate limitation.** The conformance suite cannot cover this: `Observe`
uses the extended protocol, which carries one statement per Parse, so no probe
can send a multi-statement query and no recording says what DSQL answers for
one. The refusals the emulator now gives come from its own transaction rules,
which are themselves recorded; that a real cluster answers a multi-statement
query the same way is inferred, not measured.

### The README describes the product, not the diff (2026-09-17)

The OCC work left the README explaining that a conflicting write "no longer
waits" and that some conflicts are "still reported at the statement". Both are
written from the point of view of someone who knows what the emulator did last
week. A reader has no such point of view: a sentence phrased as a delta
describes something they cannot see, and it ages into a lie the moment the thing
it contrasts with is forgotten.

Both were rewritten to state the behavior plainly, and a third "still" in the
`server_version` bullet was dropped as ambiguous. The README now contains no
change-relative wording.

`AGENTS.md` gains a `README.md` section alongside the `PLAN.md` and
`PROGRESS.md` ones, so this does not have to be caught by review again. It
carries the distinction that makes the rule usable: contrasting with Aurora DSQL
or PostgreSQL is the README's whole job, while contrasting with a previous
version of the emulator is what to avoid.

Files: `README.md`, `AGENTS.md`.

**Verification.** `grep -nE "no longer|previously|used to|\\bstill\\b|\\bnow\\b" README.md`
returns nothing. `make build`, `make vet` and `make test` pass unchanged; no code
was touched.

### Re-recorded against the cluster; the adjudicator is confirmed (2026-09-17)

Ran `dsql-baseline` against the cluster in `eu-central-1` with a fresh token,
through `--token-file` so the token never reached a process list. All 212 cases
recorded, 38 objects dropped, no cleanup skips. **The emulator matches all 212
against the new record**, which closes the gap the adjudicator work was left
with: its row counts and commit-time failures were previously checked against a
record made when it could not produce them.

The cluster confirmed the adjudicator's shape directly. Every conflicting probe
came back with both statements succeeding and one transaction failing at
`COMMIT`, and `occ_fk_delete_insert` picked the *other* session as its loser
than the 2026-09-16 run did — which is the race `ConflictRace` exists to
tolerate, demonstrated rather than argued.

**The new save process did its job, and showed where it did not go far enough.**
Of thirteen fixtures, three were written:

| fixture | why |
|---------|-----|
| `occ_conflict` | real: the `record_only` → `conflict_race` / `ignore_rows` metadata change |
| `alters` | noise: two generated `sys.jobs` ids |
| `index` | noise: five generated `sys.jobs` ids |

Both noise fixtures hold cases already marked `IgnoreRows` — the record does not
enforce those values and the emulator is never held to them, yet they would have
rewritten two fixtures on every run forever. So change detection now waives
exactly what the comparison waives: rows and row-count tags of an `IgnoreRows`
case, and which session lost a `ConflictRace` (the finals are pooled and sorted,
so *how many* lost is still enforced, and every earlier step stays positional).

With that rule the run touches one fixture. `alters.json` and `index.json` were
restored to what they were, which is what the fixed recorder would have left.

Files: `internal/conformance/record.go`,
`internal/conformance/conformance_test.go`,
`test/conformance/golden/occ_conflict.json`, `docs/PLAN.md`.

**Verification.**

```
$ go run ./cmd/dsql-baseline --host <cluster>.dsql.eu-central-1.on.aws \
      --token-file dsql.token --out-dir test/conformance/golden
recorded occ_write_write        session 0: BEGIN | UPDATE 1 | COMMIT || session 1: BEGIN | UPDATE 1 | error 40001
recorded occ_fk_delete_insert   session 0: BEGIN | DELETE 1 | error 40001 || session 1: BEGIN | INSERT 0 1 | COMMIT
cleanup: dropping 38 objects

Recorded 212 cases against aurora-dsql
Changed 3 fixture(s) in test/conformance/golden: alters, index, occ_conflict

$ make conformance
    conformance_test.go:114: 212 cases match the golden record
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	21.211s

$ make build && make vet && make test && make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	3.758s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	5.805s
```

The new rule was checked against the run's own output before the two fixtures
were restored: saving each freshly recorded file over the committed one reports
`alters changed=false`, `index changed=false`, `occ_conflict changed=true`.

**Still open.** Ten fixtures now carry 2026-09-16 and three carry 2026-09-17, so
`recorded_at` is per-group rather than per-run. Nothing reads it, and `LoadDir`
takes the first file's for the merged record, which is now arbitrary; if the
date of the last full run matters, it belongs here rather than in the fixtures.

### A recording that finds nothing changed now writes nothing (2026-09-17)

`make baseline` rewrote every fixture on every run, so the only difference a
re-recording usually produced was thirteen new timestamps. That is the opposite
of what the record is for: its git history should be the list of runs that found
the cluster answering differently.

`Save` now compares the record it is about to write with the one on disk, with
`recorded_at` cleared on both, and skips the write entirely when they match. The
comparison renders both sides through the same marshaller rather than comparing
field by field, so a field added to the record later is covered without an
equality function to keep in step with the struct. `Save` returns whether it
wrote, `SaveDir` returns the groups that changed (a pruned fixture counts), and
`dsql-baseline` reports the run either way — including `Nothing changed`, which
is a result worth seeing after paying for a run.

That a run happened at all is no longer in the fixtures. It belongs here, in the
log, which is where runs are recorded.

Files: `internal/conformance/record.go`, `internal/conformance/conformance_test.go`,
`cmd/dsql-baseline/main.go`, `test/conformance/golden_test.go`, `docs/PLAN.md`,
`AGENTS.md`, `README.md`.

**Verification.** The property is held to the committed record itself, with no
cluster and no Docker, so it is checked on every `make test`:
`TestRerecordingAnUnchangedClusterWritesNothing` puts the recorded observations
back into the order a run produces them in, saves them with a timestamp a day
later, and requires every fixture to come out byte-identical.

Writing that test found something worth keeping: `LoadDir` sorts cases by name
so two records can be compared, while a run appends in suite order, so a plain
load-and-save round trip is not a fixed point and would have tested the wrong
thing. The test reconstructs suite order, which also pins the record to the
order its probes ran in — what makes a case that reads what an earlier one wrote
readable at all.

```
$ make build && make vet && make test
ok  	github.com/Dreamescaper/dsql-emulator/internal/conformance	0.205s
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	0.184s
...

$ make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	21.233s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	22.999s
```

`git status test/conformance/golden/` is clean throughout: the tests save into a
copy, so a failure cannot disturb the record.

**Known gap.** Nothing was re-recorded yet, so the change is verified against
the existing record rather than against a run. (Closed the same day; see the
entry above, which is also where the rule turned out to need widening.)

### The OCC adjudicator: conflicts at COMMIT, without the block (2026-09-17)

Mode 3 is built, and not the way the plan proposed. No write-intent registry
was written, because PostgreSQL already keeps one: its lock manager. What the
emulator adds is a bound on the wait and a different place to report it.

**Two candidate designs were measured before either was built**, against a
throwaway PostgreSQL 16:

| Question | Result |
|----------|--------|
| Baseline: two `REPEATABLE READ` sessions, same row | the loser blocked **2984ms**, then failed at its `UPDATE` |
| A `BEFORE ROW` trigger taking `pg_try_advisory_xact_lock` | **ruled out** — the trigger never fired and the loser still blocked 2963ms. `GetTupleForTrigger` locks the tuple before a `BEFORE ROW` trigger runs, so a database-side registry cannot see the conflict it exists to prevent |
| `lock_timeout` + a savepoint | **works** — the refused statement returned in 50ms as `55P03`, `ROLLBACK TO SAVEPOINT` left the transaction usable with its snapshot intact, and it committed |
| All four DSQL conflict shapes under a bounded wait | detected in 51–56ms; the must-not-conflict control (non-key update against a referencing insert) committed in 0.6ms |

**What was built.** `internal/occ/` decides which statements a refusal can be
reported for and builds the read-only **shadow** that reports what the refused
one would have: an `UPDATE`/`DELETE` becomes `SELECT count(*)` over the same
relation and predicate, deparsed from the statement's own parse tree; an
`INSERT ... VALUES` states its own count; a locking `SELECT` is re-run without
its locking clause. `internal/proxy/adjudicator.go` runs the hidden exchanges:
a savepoint established once per transaction behind the client's own
`ReadyForQuery`, then, on `55P03` or a statement-time `40001`, a rollback to it
followed by the shadow, after which the client is answered as if its statement
had run. The transaction is marked doomed and fails at `COMMIT`.

Files: `internal/occ/occ.go`, `internal/occ/occ_test.go`,
`internal/proxy/adjudicator.go`, `internal/proxy/adjudicator_test.go`,
`internal/proxy/session.go`, `internal/proxy/session_test.go`,
`internal/conformance/compare.go`, `internal/conformance/suite.go`,
`test/integration/occ_test.go`, `rules/rules.go`,
`rules/dsql-2026.09.yaml`, `docs/PLAN.md`, `README.md`.

**The four conflict probes are now enforced.** They were `RecordOnly` because a
replay would hang. They now carry `ConflictRace` instead: which transaction
loses is a race on both sides, so the replay has to reproduce that one lost,
with the same SQLSTATE, at the step the conflict surfaces at — not which one.
`CompareSuites` now takes the replay policy from the suite rather than from the
record, so this took effect without re-recording; the recorded observations were
not touched. The two probes that read a row also ignore its value, because what
a previous case leaves there depends on which of its sessions won.

**Verification.**

```
$ make build && make vet && make test
ok  	github.com/Dreamescaper/dsql-emulator/internal/classify
ok  	github.com/Dreamescaper/dsql-emulator/internal/conformance
ok  	github.com/Dreamescaper/dsql-emulator/internal/occ
ok  	github.com/Dreamescaper/dsql-emulator/internal/proxy
ok  	github.com/Dreamescaper/dsql-emulator/internal/txn
ok  	github.com/Dreamescaper/dsql-emulator/internal/wire

$ make test-integration
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	3.728s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	5.773s

$ go test -race -count=1 ./internal/... && go test -race -tags integration -count=1 ./test/...
ok  	github.com/Dreamescaper/dsql-emulator/internal/proxy	2.120s
ok  	github.com/Dreamescaper/dsql-emulator/test/conformance	5.275s
ok  	github.com/Dreamescaper/dsql-emulator/test/integration	7.467s
```

The adjudicator adds a second goroutine's worth of state to the session, so the
suite was run under `-race` as well. It found one real problem, in the test
helper rather than the emulator: the goroutine that answers a rowset shadow
shares the backend encoder with the test that started it, so the wait is now
taken explicitly at the point it is safe.

The conformance run reports `212 cases match the golden record`, and the four
conflict probes now replay with DSQL's shape, for example:

```
recorded occ_write_write     session 0: BEGIN | UPDATE 1 | COMMIT || session 1: BEGIN | UPDATE 1 | error 40001
recorded occ_fk_delete_insert session 0: BEGIN | DELETE 1 | COMMIT || session 1: BEGIN | INSERT 0 1 | error 40001
```

`TestOccAdjudicatesConflictsAtCommit` covers all five conflict shapes against a
real backend and fails any statement that takes longer than 3s, so a regression
back to blocking is caught rather than merely slow.

**Deliberate limitations.**

- **First writer wins, not first committer.** DSQL fails whichever transaction
  it adjudicates second; the emulator fails whichever asked for the rows
  second. They coincide when a transaction commits in the order it wrote.
- **A doomed transaction does not read its own writes**; they were rolled back
  to the savepoint. It cannot commit, so nothing it reads can be acted on.
- **Parameterised statements are not taken over.** A shadow is a different
  statement, so the client's bound values cannot be carried to it. The same
  applies to `RETURNING`, `INSERT ... SELECT`, `ON CONFLICT`, and
  multi-statement simple queries. These report the conflict where PostgreSQL
  raised it, with DSQL's wording and SQLSTATE. Closing the parameterised case
  means capturing parameter types from the backend's `ParameterDescription`.
- **`lock_timeout` is session-wide**, so a DDL that cannot take its lock in
  time is reported as a conflict too.
- `occ.sources` and `occ.key_columns_only_for` are still read by nothing, but
  they are no longer aspirational: the backend's row-lock modes draw the same
  lines, down to a non-key update not conflicting with a referencing insert.
  They are now commented as the statement of what is being emulated.

### Re-recorded the baseline; ALTER COLUMN TYPE was wrong (2026-09-16)

Ran `dsql-baseline` against the cluster in `eu-central-1` with a fresh token.
The record now holds all 212 probes the suite defines and the emulator matches
every one of them, with the single accepted `alter_unique_using_index` gap. The
run drops everything it creates and reported no cleanup skips.

**The four ALTER TABLE type probes confirmed the rules added earlier**, codes
and all:

| probe | DSQL |
|-------|------|
| `ADD COLUMN bad_money money` | `0A000 datatype money not supported` |
| `ADD COLUMN bad_array text[]` | `0A000 datatype text[] not supported` |
| `ADD COLUMN bad_serial serial` | `42704 type "serial" does not exist` |
| `ALTER COLUMN a TYPE xml` | `0A000 unsupported ALTER TABLE ALTER COLUMN ... SET DATA TYPE statement` |

Two things the messages settled that the codes alone did not. An array column is
just another unsupported datatype to DSQL (`datatype text[] not supported`),
not the separate concept the emulator's wording implies. And the retype refusal
**names no type**, where the `ADD COLUMN` refusal names one — so DSQL refuses
the `ALTER COLUMN ... TYPE` form itself, not the type it targets.

**That last one was a real divergence, and the reasoning that produced it was
mine.** Earlier today the emulator was given the type lists for
`ALTER COLUMN ... TYPE` on the assumption that they applied there as they do to
`CREATE TABLE`, and a test asserted `ALTER COLUMN c TYPE bigint` was *allowed*.
It is not. A probe with a supported type
(`ALTER COLUMN a TYPE varchar(20)`, added and recorded in a second run) comes
back with the identical refusal. Rule `alter_column_type` now refuses the form
outright, ordered ahead of the type rules so its wording is the one reported.
The conformance suite had not caught it because its only retype probe used
`xml`, which both sides refused for different reasons.

**A setup ordering bug surfaced on the first attempt.** `setupStatements()`
dropped `baseline_parent` before `baseline_alter_fk`, which carries the foreign
key `alter_add_fk_not_valid` adds to it, so a re-run against a cluster holding
the previous run's objects failed at setup with `2BP01`.
`cleanupStatements()` already ordered dependents first; setup now does too.

**The save guard added earlier today did its job on that failure.** The run
aborted during setup with zero cases recorded, and the command exited with
`nothing written; the record in test/conformance/golden was left as it was`.
Under the previous code `SaveDir` would have deleted all thirteen fixtures
before discovering it had nothing to write.

Verification:

```
$ go build ./... && go vet ./... && go vet -tags integration ./... && gofmt -l .
(no output)
$ go test -race ./...
ok  github.com/Dreamescaper/dsql-emulator/internal/classify     1.5s
ok  github.com/Dreamescaper/dsql-emulator/internal/conformance  1.9s
ok  github.com/Dreamescaper/dsql-emulator/internal/proxy        2.8s
ok  github.com/Dreamescaper/dsql-emulator/internal/txn          2.9s
ok  github.com/Dreamescaper/dsql-emulator/internal/wire         2.1s
$ go test -tags integration -count=1 ./test/...
212 cases match the golden record
ok  github.com/Dreamescaper/dsql-emulator/test/conformance  2.6s
ok  github.com/Dreamescaper/dsql-emulator/test/integration  2.8s
```

Still open: the job id shape. A second sample (`tpqrncdmjja4tdl3zxo2qqvh4y`)
confirms DSQL's ids are 26 characters, the length base32 of sixteen bytes
produces, while the emulator issues dashed UUIDs. `wait_for_job` reporting
`22P02 Unable to convert text to UUID` says it decodes rather than compares, so
DSQL is most likely rendering a UUID in base32 — but nothing observed says
whether it would accept the dashed form, and the only recorded call with a real
id returns `42809` because it is a procedure. Left as an open row in
`PLAN.md` rather than changed on a guess.

### Replaced three text-matching workarounds with structural ones (2026-09-16)

A follow-up to the review: each of these worked by reading SQL as text, and each
carried a documented limitation because of it. All three now read structure that
PostgreSQL already reports. Every claim below was checked against a real server
before the change was written.

**The primary-key-column guard** (`docker/init/05-alter-guard.sql`) matched
`current_query()` with a regex, so it inspected only the single-action form and
a `DROP COLUMN a, DROP COLUMN id` could still lose a key. It is now two event
triggers: `ddl_command_start` snapshots every primary key column as
`<table oid>:<attnum>`, and `sql_drop` compares that against the object
addresses of the columns the command actually dropped. No statement text is
read, so quoting, `IF EXISTS`, `ONLY`, schema qualification and multi-action
commands all fall out for free. `sql_drop` fires after the drop but inside the
same transaction, so raising there fails the statement and aborts the
transaction exactly as refusing up front did.

**The `ASYNC` rewrite** (`internal/proxy/rewrite.go`) used one regex to strip the
keyword libpg_query cannot parse and a second to dig the index name back out.
Only the first was forced. The stripped statement is now parsed, which supplies
everything the second regex did — correctly folded by PostgreSQL rather than by
a hand-written `unquoteIdent` — and, more importantly, means the ruleset is
applied to an asynchronous statement at all. It was not before:
`forwardAsyncJob` never called `Classify`, so any rule keyed on `index_stmt`
silently did not apply to the `ASYNC` form. The two rules that exist only to
require `ASYNC` are marked `unless_async: true` in the ruleset and skipped
through the new `classify.AsyncRewritten()` option; every other rule now
applies. A schema-qualified index name is no longer special-cased in Go: neither
grammar accepts one, so the statement is forwarded and PostgreSQL answers with
the same `42601 syntax error at or near "."` the dialect reports.

**The job id** (`docker/init/04-jobs.sql`, `internal/proxy/rewrite.go`) was
`md5(object name)` computed identically on both sides. That failed when there
was no name to derive from, and made a rebuilt index reuse its old id where a
real cluster issues a fresh one. The emulator now picks the id and passes it
down in a marker comment on the statement itself
(`/* dsql_job=<uuid> */ CREATE INDEX ...`), which the event trigger reads from
`current_query()`. One statement, no extra round trip, and the unnamed-index
case works. The marker also identifies an emulator-issued asynchronous `ALTER
TABLE`, which replaced a second regex over `current_query()` that looked for
`VALIDATE CONSTRAINT`.

Deleted with them: `asyncIndexNamePattern`, `asyncAlterNamePattern`,
`unquoteIdent`, the `ident` pattern, `jobIDForIndex`, `jobIDForValidation`, the
`asyncIndex`/`asyncAlter` structs, `forwardAsyncIndex`, `forwardAsyncAlterTable`,
and the hard-coded `42601` rejection.

Verification:

```
$ go build ./... && go vet ./... && go vet -tags integration ./... && gofmt -l .
(no output)
$ go test -race ./...
ok  github.com/Dreamescaper/dsql-emulator/internal/classify     1.4s
ok  github.com/Dreamescaper/dsql-emulator/internal/conformance  0.3s
ok  github.com/Dreamescaper/dsql-emulator/internal/proxy        2.0s
ok  github.com/Dreamescaper/dsql-emulator/internal/txn          0.9s
ok  github.com/Dreamescaper/dsql-emulator/internal/wire         0.5s
$ go test -tags integration -count=1 ./test/...
ok  github.com/Dreamescaper/dsql-emulator/test/conformance  2.4s
ok  github.com/Dreamescaper/dsql-emulator/test/integration  2.9s
```

Conformance still reports 207 cases matching, the one known gap unchanged, and
the four probes added earlier today still uncovered.

Coverage added to `test/integration/proxy_test.go`: four forms of the primary
key column drop (several columns in one statement, a composite key member, a
quoted mixed-case name, schema-qualified with `ONLY` and `IF EXISTS`), a check
that `DROP TABLE` does not trip the guard, and an unnamed `CREATE INDEX ASYNC`
whose returned id must be findable in `sys.jobs` and accepted by
`wait_for_job`. The multi-column case fails against the previous guard and
passes against this one; the other three the regex already handled, which the
run confirmed rather than assumed.

Known gaps, unchanged or newly visible:

- `CREATE INDEX ASYNC IF NOT EXISTS` on an index that already exists still
  returns an id with no `sys.jobs` row, because nothing is built and
  `pg_event_trigger_ddl_commands()` reports no command. This is no longer about
  deriving a name.
- The recorded `sys.jobs` row shows a real DSQL job id of
  `yoeqoh5bcjgw7kcmwihktdbtgq` — 26 characters, the length base32 of sixteen
  bytes produces, not a dashed UUID. The emulator issues UUIDs, which stay
  consistent with the `uuid` cast in `wait_for_job` that reproduces the
  verified `22P02` for a malformed id. Recorded as an open question in
  `PLAN.md` rather than guessed at; `sys_jobs_columns` sets `IgnoreRows`, so
  nothing in the suite pins the shape either way.

### Repository review: fixed nine findings (2026-09-16)

A read of the whole repository, with each behavioral finding confirmed by a
throwaway test before it was fixed.

Correctness:

- `cmd/dsql-baseline/main.go`, `internal/conformance/record.go` — a failed
  baseline run used to save its partial record, and `SaveDir` deleted every
  fixture in the directory before writing. A failure at any point in a 207-probe
  run therefore destroyed the golden record, which costs a metered cluster run
  to rebuild. The command now exits without saving when the run fails, `SaveDir`
  refuses a record with no cases, and it prunes stale fixtures only after every
  new one is on disk. The same change removes a nil dereference on the
  `RunSuite` connect-failure path, where `golden.Target` was assigned before the
  error was checked.
- `internal/proxy/session.go` — a `CREATE INDEX ASYNC` that the backend refused
  left the synthesized job armed, so the next successful statement had a second
  `RowDescription` and a `job_id` row spliced into its result set, which is not
  legal protocol. The job is now dropped on `ErrorResponse`, and on
  `ReadyForQuery` as a backstop.
- `internal/proxy/session.go` — an async DDL was admitted to the transaction
  rules at Parse *and* again at Bind, so `BEGIN; CREATE INDEX ASYNC ...;` over
  the extended protocol was refused by the emulator's own one-DDL-per-transaction
  rule. It is now admitted once, at Bind, like every other prepared statement,
  and the statement carries its job id so a cached prepared statement executed
  again still answers with one.
- `internal/classify/eval.go`, `rules/dsql-2026.09.yaml` — the supported-type
  list was enforced only on `CREATE TABLE`, so `ALTER TABLE ... ADD COLUMN c
  money`, `text[]`, and `serial`, and `ALTER COLUMN ... TYPE xml`, all passed
  through. Column extraction now covers `AT_AddColumn` and `AT_AlterColumnType`,
  and the rules are duplicated onto `alter_table_stmt` through YAML anchors so
  the two type lists cannot drift.
- `internal/proxy/session.go` — OCC injection rules shared one commit counter,
  so N rules matching a commit advanced it N times and a rule's `every: N`
  fired early. Each rule now counts its own matches.
- `internal/proxy/session.go` — `newJobID` returned 26 hex characters where a
  derived id is a UUID. `sys.wait_for_job` casts to `uuid` first, so the
  unnamed-index path answered with an id that failed as malformed (`22P02`)
  rather than unknown. It is now a UUID.
- `internal/proxy/session.go` — removed `inExtended`, `markExtended`, and
  `getInExtended`: the field was written at every frontend message and never
  read.

Suite and documentation:

- `internal/conformance/suite.go` — four probes added to the `alters` group for
  the ALTER TABLE type rules above, each on its own column so one that
  unexpectedly succeeds cannot change what the next observes. They are
  **unrecorded**: the fix matches the CREATE TABLE behavior the record already
  pins, but what a real cluster answers for the ALTER forms has not been
  recorded. The conformance test reports them as uncovered until the next
  baseline run.
- `AGENTS.md`, `docs/PROGRESS.md` — the backing engine was described as
  PostgreSQL 17 in three places; compose, the Dockerfile, and all three
  testcontainers call sites use `postgres:16-alpine`.
- `docs/PROGRESS.md` — **Current status** and **Next up** described a state
  several sessions old (65 probes, `CREATE INDEX ASYNC` and `sys.jobs` as open
  gaps, the suite as single-connection).
- `.gitignore` — `.vscode/` was untracked and unignored, so it showed dirty in
  every `git status`.

Verification:

```
$ go build ./... && go vet ./... && go vet -tags integration ./... && gofmt -l .
(no output)
$ go test -race ./...
ok  github.com/Dreamescaper/dsql-emulator/internal/classify     1.5s
ok  github.com/Dreamescaper/dsql-emulator/internal/conformance  1.8s
ok  github.com/Dreamescaper/dsql-emulator/internal/proxy        3.3s
ok  github.com/Dreamescaper/dsql-emulator/internal/txn          2.1s
ok  github.com/Dreamescaper/dsql-emulator/internal/wire         2.5s
```

Regression tests added: `TestSessionAllowsPreparedAsyncIndexInTransaction`,
`TestSessionDropsJobWhenAsyncIndexFails`,
`TestSessionCountsEachOccInjectionSeparately`, `TestNewJobIDIsAUUID`,
`TestSaveDirRefusesToEmptyTheRecord`, and four rejection cases plus four
acceptance cases in `TestClassifyRejectsUnsupportedStatements` /
`TestClassifyAllowsSupportedStatements`. Each of the first three fails against
the code as it was.

Known gap: the four new conformance probes are unrecorded, so the ALTER TABLE
type refusals are reasoned from the CREATE TABLE behavior rather than observed
on a cluster.

### Released v0.1.1 and verified the published image (2026-09-15)

Pushed, cut `v0.1.1` with the release workflow, and checked the artifact rather
than assuming it.

- The release published `0.1.1` and `latest` for `linux/amd64` and
  `linux/arm64`, and `latest` resolves to `0.1.1`. This also confirms the fix
  for the tag-resolution bug: `v0.1.0` had published with only `latest`.
- Pulled `0.1.1` and exercised the recent work through it: `version()` reports
  `PostgreSQL 16`, a **partial index** returns a UUID `job_id` and appears in
  `sys.jobs` as `INDEX_BUILD|completed`, dropping a **primary-key column** is
  refused with `cannot drop primary key column id`, a non-key drop still works,
  and a foreign key added without `NOT VALID` is refused with `0A000`.

One gap surfaced while doing it: the `ASYNC` rewrite is anchored to a whole
statement, so a multi-statement simple query containing `CREATE INDEX ASYNC` —
what `psql -c 'a; b'` sends — is not rewritten and PostgreSQL answers with a
syntax error where DSQL answers with its own error. Recorded in README and
PLAN; not worth rewriting the anchored match for, since DSQL refuses more than
one DDL per transaction in that form anyway.

### Guard against dropping a primary-key column (2026-09-15)

Closed the more serious of the two `ALTER TABLE` divergences: DSQL refuses to
drop a column that is part of a primary key, and the emulator used to perform
the drop and silently remove the key with it.

A pre-flight query in the proxy would have meant making a synchronous request
inside the streaming relay — exclusive access to the upstream, a known-idle
point that pipelining makes unreliable, response capture, and client
backpressure — which is a lot of hang risk to serve one check. Instead the
guard lives in the backing database, which already has the catalog:
`docker/init/05-alter-guard.sql` registers a `ddl_command_start` event trigger
that reads the statement, resolves the table with `to_regclass`, and raises
`0A000 cannot drop primary key column <name>` when the column is in
`pg_index.indisprimary`. It fires before the drop, while the key is still there.

The trigger reads statement text, because an event trigger at that point has no
parse tree, so only the single-action form is inspected: `DROP COLUMN c`,
`IF EXISTS`, `ONLY`, a `*`, `RESTRICT`/`CASCADE`, quoting, and schema
qualification all work, but a multi-action `ALTER TABLE ... DROP COLUMN a, DROP
COLUMN b` is left alone. It fails open, so it cannot refuse a statement it does
not understand. Checked against a real database before any Go changed, and the
`alter_drop_pk_column` probe is now enforced rather than a known gap.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `207 cases match the golden record` with one known gap left, the
asynchronous index build. The integration subtest asserts the drop is refused
and that the key survives.

### Validated the recently announced dialect features (2026-09-15)

Checked each feature Aurora DSQL announced recently. Recording twelve
`ALTER TABLE` probes (record now 207 cases) produced the answers.

| Announcement | Emulator |
|---|---|
| `ALTER TABLE ... DROP COLUMN` | Supported, matching. |
| Indexes on expressions | Already covered (`create_index_async_expression`). |
| `SELECT ... FOR KEY SHARE` | Already covered (`occ_for_key_share`). |
| Character compression (`STORAGE`) | `ADD COLUMN ... STORAGE` and `SET STORAGE` both match. |
| Foreign keys | Supported; the new `ALTER TABLE` forms are below. |
| Partial indexes | Covered in the previous entry. |

Work the recording prompted:

- `ALTER TABLE ASYNC ... VALIDATE CONSTRAINT` is DSQL-only syntax, so the
  parser refused it with `42601`. The `ASYNC` rewrite now covers `ALTER TABLE`
  as well as `CREATE INDEX`, and the statement is answered with the `job_id`
  row DSQL returns.
- The dialect requires `NOT VALID` on a `CHECK` or `FOREIGN KEY` added by
  `ALTER TABLE`, and validates only through the `ASYNC` form. Both are refused
  with `0A000`, from two new predicates over `AlterTableCmd` subtypes, so the
  proxy decides them statically.
- The job trigger now records constraint validation as well as index builds,
  detecting the validation from `current_query()` (verified readable in the
  trigger). It cannot over-record, because the synchronous form is refused
  before it reaches the database, and `wait_for_job` accepts the returned id.

Two divergences are documented rather than emulated:

- **Dropping a primary-key column**: DSQL refuses with `0A000 cannot drop
  primary key column <name>`. Deciding it needs catalog knowledge the proxy
  does not keep, so the emulator performs the drop and silently loses the key.
  Recorded as a known gap.
- **`ADD CONSTRAINT ... UNIQUE USING INDEX` after `CREATE INDEX ASYNC`**: DSQL
  refuses with `55000 index ... is not valid` because the build is still
  running. The emulator builds synchronously, so the index is valid at once and
  the constraint is added. Recorded as a known gap.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `207 cases match the golden record` with only those two known gaps.

### Fixed the image tag resolution in the publish workflow (2026-09-15)

The first release, `v0.1.0`, published an image tagged only `latest`: the
version tag was missing.

Inside a reusable workflow `github.event_name` is the **caller's** event
(`workflow_dispatch`), not `workflow_call`. The tag resolution branched on
`workflow_call`, so it took the release path, found no `release` payload, and
produced an empty tag; `latest` came from the branch's own default and was
the only tag pushed. The log showed a single tag, which is what gave it away.

Tag resolution now branches on whether an input tag was supplied, which does
not depend on event-name semantics, and the workflow gained a
`workflow_dispatch` with a `tag` input so an existing tag can be re-published
without cutting a new release. `v0.1.0` was then re-published with both tags.

### Partial indexes (2026-09-15)

Aurora DSQL added partial indexes, so the emulator was checked against them.

- The feature needed no work: `rewriteAsyncIndex` only strips the `ASYNC`
  keyword, so the `WHERE` predicate, index expressions, `INCLUDE`, and
  `NULLS NOT DISTINCT` are forwarded untouched and PostgreSQL builds them.
  Confirmed by an integration test that reads the predicate back from
  `pg_indexes`, and by six new probes that all succeed with tag `CREATE INDEX`.
- One restriction did need work. The documentation says an index name cannot be
  schema-qualified, since DSQL always puts the index in the table's schema.
  PostgreSQL would accept one, so the emulator now refuses it before
  forwarding. The recording shows DSQL reports a **syntax error (`42601`,
  `syntax error at or near "."`)**, not the `0A000` that was guessed first.
- Job ids are now shaped as UUIDs, derived the same way from the index name.
  The recording showed `wait_for_job` **converts its id to a UUID** — a bad id
  is `22P02`, not "unknown job" — so an md5-hex id could not have been passed to
  the emulator's own `wait_for_job`. `jobIDForIndex` and the recording trigger
  both format md5 as `8-4-4-4-12`, and the procedure casts to `uuid` so an
  invalid id fails the same way.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `195 cases match the golden record` with nothing unrecorded and no
known gaps.

Known limitation: an unnamed index gets no derivable name, so the `job_id`
returned for it is random and will not be found in `sys.jobs`.

`sys_jobs_columns` now reads `SELECT * FROM sys.jobs LIMIT 1`. The cluster's job
log only grows, and that probe had already reached 63 stored rows, so the
fixture grew with every recording. The columns are the point and the rows are
ignored, so one row is enough.

### Reconciled against DSQL's real sys.jobs and version paths (2026-09-15)

Recording the four probes that were waiting on a cluster paid off; every one
taught something.

- **`sys.jobs` has nine columns**, not the four the emulator guessed:
  `job_id, status, details, job_type, class_id, object_id, object_name,
  start_time, update_time`, with lower-case statuses (`completed`), `job_type`
  `INDEX_BUILD`, `class_id` 1259 (the `pg_class` catalog), and `object_name`
  schema-qualified. The table and the recording trigger now match, so
  `SELECT * FROM sys.jobs` agrees.
- **DSQL also records `ANALYZE` and `DROP` jobs.** The emulator records only
  index builds; the difference is documented rather than emulated.
- **`sys.wait_for_job` is a procedure**, so `SELECT sys.wait_for_job(...)` fails
  with `42809 ... is a procedure` on both DSQL and the emulator, because
  PostgreSQL generates that error itself once the routine is a procedure. The
  emulator's stub function became a procedure to match.
- **`CALL` with a subquery argument is refused** on both with `0A000 cannot use
  subquery in CALL argument` — again PostgreSQL's own message.
- **The version paths are `16.15` and `160015`.** `current_setting` forms and
  `SHOW server_version_num` are now rewritten, so no version probe leaks the
  backing engine any more. `serverVersionNum` encodes `16.15` as `160015`, the
  way DSQL does.

A comparison fix was needed too: `IgnoreRows` skipped the rows of
`SELECT *`-style probes but not the command tag that counts them, and DSQL's
count grows as its job history accumulates. A row-count tag is now ignored
alongside the rows.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `187 cases match the golden record` with one new probe unrecorded
and no known gaps. The integration subtest now asserts the recorded job's shape
(`completed`, `INDEX_BUILD`, `public.widget_async_idx`), that `CALL` works, and
that a `SELECT` against the procedure fails with `42809`.

### sys.jobs lifecycle for async index builds (2026-09-15)

`CREATE INDEX ASYNC` now leaves a completed `INDEX_BUILD` job in `sys.jobs`, and
`sys.wait_for_job` reports its status, closing the gap the README listed.

- `docker/init/04-jobs.sql` adds an event trigger registered for `CREATE INDEX`
  that inserts the job. Registering for `CREATE INDEX` is what keeps it correct:
  it fires for explicit index creation, including `UNIQUE`, but not for the
  index a `CREATE TABLE` creates for a primary key or unique constraint
  (verified against a real PostgreSQL before writing any Go).
- The job id is `md5(index name)` on both sides. The emulator derives it from
  the statement it rewrites, so the id handed to the client is findable in
  `sys.jobs` without reading it back — a round trip that would need the
  recording-and-swallowing machinery the row cap needed. DSQL's ids are random;
  deriving ours is the price of that simplicity.
- `rewriteAsyncIndex` now also extracts the index name, handling quoting,
  escaped quotes, and schema qualification, with unit tests for each.
- `sys.jobs.job_id` became `text` to hold the derived id, and `sys.wait_for_job`
  looks the job up and rejects an unknown id.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`. The integration
subtest `async index records a job` asserts that the id returned by
`CREATE INDEX ASYNC` is in `sys.jobs` with `INDEX_BUILD`/`COMPLETED`, that
`sys.wait_for_job` returns it, and that the index really exists. The conformance
run still reports `181 cases match the golden record`.

Two probes were added and are unrecorded: `sys_jobs_columns` and
`sys_wait_for_job`, both known gaps, to pin DSQL's real table shape and contract
on the next baseline.

Known limitation: `CREATE INDEX ASYNC IF NOT EXISTS` on an index that already
exists returns a job id with no row, because no build happened.

Also refreshed the README: how images are versioned and published (multi-arch,
on release), a Releases section, the CI checks, the full list of init scripts,
and the unrecorded-probe behaviour.

### README, MIT license, and the admin role (2026-09-15)

- `README.md` — what the emulator is for, quick starts for the published image,
  running from source, and testcontainers, a table of what it emulates, an
  explicit list of what it does **not** do, configuration, the conformance
  harness, and prior art. It carries a Stand With Ukraine badge.
- `LICENSE` — MIT.
- Writing the README exposed a real bug: the Docker example connects as DSQL's
  `admin` user, but the backing database only ever had `postgres`, so any
  DSQL-shaped client failed with `role "admin" does not exist`. Added
  `docker/init/03-admin.sql`, which creates the role, and verified against the
  rebuilt image that `admin` connects over TLS with a token and that the row cap
  still applies.
- The container-backed tests now glob `docker/init/*.sql` instead of naming
  scripts, so a new init file cannot be missed the way `03-admin.sql` would have
  been.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, and `go test -tags integration ./test/...` all pass.

### Automatic release versions, without a personal token (2026-09-15)

Added a `release` workflow you run from the Actions tab with a
`patch`/`minor`/`major` bump. It verifies the build and unit tests, computes
the next version from the latest `v*` tag (the first release is `v0.1.0`),
creates the release with `gh release create --generate-notes`, then publishes
the image.

The publishing step calls the `image` workflow directly rather than relying on
`release: published`. GitHub suppresses events created with the default
`GITHUB_TOKEN`, so a release made by the workflow would never have fired the
`image` workflow — the release would appear with no image. Calling it directly
avoids a personal access token entirely. `image` still listens for
`release: published` too, so a release created by hand in the UI also publishes.

The version arithmetic was checked against the cases that matter: no tags →
`v0.1.0`; `patch`/`minor`/`major` on `v0.1.0` → `v0.1.1`/`v0.2.0`/`v1.0.0`; a
prerelease tag `v1.2.3-rc.1` → `v1.2.4`; and multi-digit parts carry correctly
(`v9.9.9` minor → `v9.10.0`).

### Image publishes on release (2026-09-15)

The `image` workflow now runs on `release: published` instead of on pushes to
`main`, so an image always corresponds to a released version. Tags are the
release version (`1.2.3` from tag `v1.2.3`) and `latest` for a non-prerelease;
a prerelease gets only its version tag. Publishing still builds `linux/amd64`
and `linux/arm64` and authenticates with the repository's own token, so no
personal package scope is needed.

The `test` workflow still runs on every push and pull request; only publishing
is restricted to releases.

### Run formatting, vet, unit, integration, and image checks in CI (2026-09-15)

Added `.github/workflows/test.yml` with three jobs: `unit` (gofmt gate,
`go build`, `go vet` on both tag sets, `go test -race ./...`), `integration`
(the container-backed suites, which use the committed golden fixtures and never
a real cluster), and `image-build` (builds the Dockerfile with `push: false`, so
a broken image fails a pull request while the `image` workflow owns publishing).
It uses least-privilege permissions, per-job timeouts, and cancel-in-progress
concurrency.

Verification: both workflow files parse as YAML, and every command in the job
steps was run locally and passes, including `go test -tags integration
./test/...`. GitHub Actions itself cannot be executed locally.

### Backing engine set to PostgreSQL 16 (2026-09-15)

Switched the backing database from 17 to 16 in the Dockerfile, compose file,
and both test suites. 17 was an arbitrary early default, never justified
against the alternatives.

- 16 is the generation Aurora DSQL's dialect follows, so the backend is less
  likely to accept syntax DSQL rejects. 18 would be worse: newer than both
  DSQL's dialect and the parser, so it would silently turn "DSQL fails" into
  "emulator passes".
- Matching 16 does not fully align the reported version: the rewrites are
  exact-match, so `current_setting('server_version')`, `server_version_num`,
  and `version()` inside a larger expression still leak the backing engine. Two
  `environment` probes record that as a known gap.
- The image workflow now builds `linux/amd64` and `linux/arm64`. The first
  publish succeeded but was amd64-only, which Apple Silicon cannot pull.

Verification: `go test -tags integration ./test/...` passes on 16, and the
conformance run reports `181 cases match the golden record`.

### Published container image (2026-09-15)

Added `Dockerfile` and `docker/entrypoint.sh`, which bundle PostgreSQL with the
`docker/init` scripts and run the proxy in front of it, so one container is a
working DSQL endpoint. `make docker-build` and `make docker-push` build and
publish it; `IMAGE` and `TAG` default to
`ghcr.io/dreamescaper/dsql-emulator:latest`.

Verification: built the image, ran it, and connected from a second container on
a shared Docker network as a DSQL client would, with `sslmode=require` and a
password that is not a real credential:

- `select 1` connects; `select version()` returns `PostgreSQL 16` and
  `show server_version` returns `16.15`.
- `truncate` is refused with `unsupported statement: Truncate`.
- An autocommit insert of 3001 rows fails with `transaction row limit exceeded`
  and leaves zero rows, so the bundled init scripts are applied.
- The container reports `proxy listening` about two seconds after start, which
  is the testcontainers wait strategy.

Publishing is blocked on registry permissions: the `gh` token has `repo` but not
`write:packages`, so `docker push ghcr.io/...` is denied. Running
`gh auth refresh -h github.com -s write:packages` unblocks it.

### M5 (part 3) — OCC behavior recorded (2026-09-15)

Recorded the concurrency probes. The record now covers 181 cases, matches with
no known gaps, and the cluster is clean.

DSQL's conflict behavior, now pinned rather than assumed:

- **The loser fails at `COMMIT`, not at its statement.** In every conflicting
  case both writers' statements succeed and the second committer is rejected.
- **The error is `40001 change conflicts with another transaction (OC000)`** —
  byte-for-byte what the emulator already synthesizes for PostgreSQL's
  serialization failure, so the wording needed no change.
- `FOR UPDATE` versus a write, and `FOR KEY SHARE` versus deleting the key, both
  conflict at commit.
- A foreign-key delete-referenced-row versus insert-referencing-row conflicts at
  commit, exactly as the documentation's example shows.
- A non-key update does not conflict with a referencing insert, and writes to
  different rows do not conflict; both are replayed and enforced.

The remaining divergence is only *when* and *how* the loser fails: PostgreSQL
blocks the second writer at its statement, so the emulator errors there rather
than at commit. That is why the four conflicting probes stay `RecordOnly`.

Also fixed a misleading log in `runSessions`: it reported a timeout whenever a
step errored, because it checked the step context after cancelling it.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `181 cases match the golden record`, and `--cleanup-only` confirms
no `baseline_` objects remain.

### M5 (part 2) — concurrent sessions and OCC probes (2026-09-15)

The conformance suite can now run a case on several connections at once, and
six concurrency probes were added. DSQL's conflict output can be recorded rather
than assumed.

- `Case.Sessions` gives each entry its own connection; the sessions run
  concurrently, each step list in order, separated by a fixed delay rather than
  barriers. Every step has a timeout so a blocking target cannot stall a run.
- `Case.RecordOnly` records a case against a real cluster but never replays it
  against the emulator. Conflicting cases use it, because PostgreSQL blocks
  where DSQL is lock-free and a replay would hang.
- `RunSuite` takes a `Connector` instead of one connection, so it can open the
  extra connections a case needs. The comparator is session-aware, and
  record-only cases are skipped in both directions.
- Probes: `occ_write_write`, `occ_for_update_vs_write`,
  `occ_for_key_share_vs_delete`, and `occ_fk_delete_insert` (record-only), plus
  `occ_disjoint_writes` and `occ_fk_nonkey_update`, which are replayed and
  enforced once recorded. Conflict data lives in dedicated
  `baseline_conflict` tables so the other probes' expectations are untouched.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `175 cases match the golden record` with the two enforced
concurrency cases listed as unrecorded, and cleanup leaves nothing behind. New
unit tests: `TestCompareConcurrentSessions` and
`TestCompareSkipsRecordOnlyCases`.

Pending: the six new probes need a baseline run to record DSQL's behavior.

### M3 remainder — the implicit row cap, enforced by a trigger (2026-09-15)

The last known conformance gap is closed. A single autocommit statement that
crosses the 3000-row cap is now refused with `54000` and commits nothing,
matching Aurora DSQL.

PostgreSQL commits a statement at `CommandComplete`, so the proxy cannot see the
row count in time. A statement-level trigger cannot either: `GET DIAGNOSTICS
ROW_COUNT` is 0 there (verified). The cap is therefore enforced in the backing
database by `docker/init/02-rowcap.sql`: a row trigger counts modifications per
transaction in a transaction-local GUC and raises `54000` on the row that
crosses the cap. A DDL event trigger attaches it to new tables, and the script
backfills existing ones.

Consequences:

- The statement that crosses the cap fails, so nothing commits; PostgreSQL's
  aborted-transaction state then gives `25P02` and the `ROLLBACK` tag on COMMIT
  for free. This made the proxy's `CommandComplete` row counting, its abort
  injection, and the tracker's row-cap rule redundant, and they were removed.
- The cap stays configuration: `limits.dml_rows_per_txn` is passed to the
  backing server through the startup options as `-c dsql.row_cap=N`.
- Overhead is about 1.6 µs per written row (3000 rows: 1.3 ms without the
  trigger, 6.0 ms with it).

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `175 cases match the golden record` with **no known gaps**, and the
integration subtest `row cap fails an autocommit statement` asserts both the
`54000` and that zero rows were committed.

### M7 (part 6) — query conformance probes (2026-09-15)

Added a 61-probe `queries` group and recorded it, bringing the record to 175
cases. It covers the documented `SELECT` clauses, joins and set operations, and
common patterns the documentation does not mention (CTEs, scalar/`IN`/`EXISTS`/
correlated subqueries, `unnest`, `generate_series`, aggregates, `RETURNING`,
upsert, `INSERT ... SELECT`, `EXPLAIN`, `ANALYZE`, `SET CONSTRAINTS`), plus a
handful of patterns and operators drawn from Npgsql's query baseline: full-text
search, geometric functions, `MERGE`, and `TABLESAMPLE`.

The first comparison found nine divergences, all fixed against the record:

- `ANALYZE` was refused because it shares a node with `VACUUM`; a `vacuum_kind`
  predicate now separates them.
- Text-search functions fail as `42704` (missing configuration), not `0A000`.
- `MERGE` and `TABLESAMPLE` are refused with `0A000`; a generic `contains`
  predicate matches nested nodes such as `RangeTableSample`.
- Six probes returned rows in an order DSQL and PostgreSQL disagree on; they now
  carry `ORDER BY` so the comparison is deterministic.
- DSQL reports `server_version` `16.15` and `version()` `PostgreSQL 16`, and
  `SHOW server_version` carries the `SHOW` command tag, so the rewrites and
  default constants were corrected. `SHOW lc_collate` is refused with `42704`.

Also confirmed: `GROUP BY ALL`, which the documentation lists, is actually
rejected by DSQL with `42601` — matching the emulator's parse failure — and the
constructs the docs omit (`GROUPING SETS`, `ROLLUP`, `CUBE`, `LATERAL`,
`DISTINCT ON`, `ROW_NUMBER`, `LAG`, `WITH RECURSIVE`, aggregate `FILTER`) are
supported by both.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `175 cases match the golden record` with `row_cap_implicit` the only
known gap, and `--cleanup-only` confirms no `baseline_` objects remain.

### M4 (part 2) — IAM token acceptance (2026-09-15)

DSQL clients present a short-lived IAM token as the password. The emulator
relays authentication to the backing server, so `docker-compose.yml` now sets
`POSTGRES_HOST_AUTH_METHOD: trust`, and a DSQL-configured application connects
unchanged with any token.

This is a configuration answer rather than token validation: the token is never
checked, and a password-authenticated backing server would reject it. Owning the
client authentication exchange would need a SCRAM handshake on the upstream
connection, which is out of scope here. Recorded in PLAN.md.

Verification: the integration test `TestTokenAuthThroughProxy` starts a
trust-backed PostgreSQL, connects through the emulator with `sslmode=require`
using a password that is not a real credential, and runs a query.

With this, every milestone in PLAN.md is either done or has its remaining work
written down: implicit-transaction wrapping (M3 remainder), the OCC adjudicator
and FK conflict fixtures (M5), and multi-session golden support.

### M5 — optimistic concurrency control (2026-09-15)

- **Mode 1, native delegation.** The upstream already runs at `REPEATABLE READ`,
  so write-write conflicts raise PostgreSQL's serialization failure. The
  emulator now rewrites that error to DSQL's wording and OCC code
  (`change conflicts with another transaction (OC000)`, SQLSTATE `40001`).
- **Mode 2, deterministic injection.** `occ.inject` rules name tables and an
  interval. When a transaction that touched one commits, the emulator rolls the
  upstream back instead of committing and reports the conflict, so an
  application can unit-test its retry loop without a real race.
- The abort machinery was generalized: a `txnFailure` now carries the statement
  to send, the ReadyForQuery status to emit, and how many upstream
  ReadyForQuery messages to swallow. Transaction aborts use it with `E`, an
  injected commit conflict with `I`.
- Classification now reports the relations a DML statement touches, and the
  session remembers a prepared statement's kinds and tables.
- Row-locking clauses: `FOR UPDATE` and `FOR KEY SHARE` are allowed;
  `FOR SHARE` and `FOR NO KEY UPDATE` are refused, with a `locking` predicate
  and an `unsupported_locking` rule. Four `occ` probes were added.

Verification:

```
gofmt -l .                                     # no output
go build ./... && go vet ./... && go vet -tags integration ./...   # ok
go test -race ./...                            # ok
go test -tags integration -count=1 ./test/...  # ok
```

New tests: `TestSessionInjectsOccConflictAtCommit`,
`TestSessionInjectsOccOnlyForMatchingTables`, and
`TestSessionRewritesSerializationFailure` (white-box, deterministic); the
integration subtest `concurrent updates conflict with an occ code`, which runs
two real sessions, blocks one on PostgreSQL's row lock, and asserts the loser
sees `40001` carrying `OC000`.

Conformance still reports `102 cases match the golden record`; the eleven probes
added since the last baseline (environment and locking) are listed as
unrecorded.

Deliberate limitations:

- The upstream blocks before failing where DSQL is lock-free. Mode 3, an
  adjudicator that approximates lock-free conflict, is not built.
- Injection and the row cap apply to explicit transactions only.
- Two-session conflict probes are not in the golden suite, which is
  single-connection; a blocking conflict cannot be replayed safely against the
  emulator. Multi-session support is the next step for pinning DSQL's exact
  conflict output.

### M6 — async indexes and sys.jobs (2026-09-15)

`CREATE INDEX ASYNC` now works end to end, closing the last two conformance
gaps.

- `internal/proxy/rewrite.go` removes the `ASYNC` keyword before the statement
  is parsed, because libpg_query rejects it. The statement is otherwise
  preserved, and the transaction rules treat it as the DDL it is.
- The emulator answers with the `job_id` row DSQL returns: `Describe` is served
  a `job_id` text column instead of `NoData`, and an `Execute` result gains a
  generated id before the command tag.
- `docker/init/01-sys.sql` creates `sys.jobs` and `sys.wait_for_job` in the
  backing database, and `docker-compose.yml` mounts it. `IgnoreRows` now skips
  only the row comparison, so the `job_id` column is still checked.

Verification: `gofmt` clean, `go build`, `go vet` (both tags),
`go test -race ./...`, `go test -tags integration ./test/...`; the conformance
run reports `102 cases match the golden record` with no known gaps left. New
integration subtests `async index reports a job id` and `synchronous index is
still refused`.

Deliberate limitation: the emulator builds the index synchronously and does not
record a row in `sys.jobs`, so `sys.jobs` is present but empty and
`sys.wait_for_job` is a stub. The surface matches; the job lifecycle does not.

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
- `test/integration/proxy_test.go` — testcontainers PostgreSQL 16 + pgx through
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
| 2026-09-17 | `occ_multirow_predicate` is a known gap, not a bug to fix | Failing both sides of a conflict needs DSQL's adjudication logic; the emulator borrows PostgreSQL's locks, where the first writer can always commit. Matching it would mean replacing the premise of the OCC layer for one recorded case. |
| 2026-09-17 | A parent row and a child table per referential action | The actions differ in what they write to the child, so sharing a parent would have let one probe's delete disturb another's. |
| 2026-09-17 | Marked the new pairs `ConflictRace` before knowing whether they conflict | It asserts how many transactions lost rather than which, so it is correct whichever way the recording goes, and wrong only if the count itself differs -- which is the thing worth catching. |
| 2026-09-17 | Fill `{type}` and `{language}` from the refused statement | DSQL names what it refused, and a fixed sentence per rule cannot. The alternative was a rule per type, which would have meant twenty-odd near-identical rules and no way to name an array's element type at all. |
| 2026-09-17 | Render types as PostgreSQL displays them, not as written | The record shows `varbit(8)` refused as `bit varying` and `int[]` as `integer[]`, so DSQL reports the displayed name. Only the parser's internal aliases are mapped; anything else passes through, so a type this has no evidence for is reported as the user wrote it rather than guessed at. |
| 2026-09-17 | Messages stay advisory in the comparison | Matching today does not make wording a contract. Keeping them advisory means a future drift is reported rather than failing a run, which is the same bargain as before -- only now the baseline is zero notes. |
| 2026-09-17 | Kept the multi-statement probes' recorded messages as advisory, like every other message | DSQL words the two refusals differently from the emulator (`multiple ddl statements not supported in a transaction` against `a transaction can include only one DDL statement`). The SQLSTATE is the contract, and matching wording across systems is not a goal. |
| 2026-09-17 | Record one `Result` per statement rather than one `Observation` per statement | Keeps the step-to-observation mapping the comparison and the diff messages rely on, and keeps a multi-statement step legible as one thing in the record. |
| 2026-09-17 | Added the probes before recording them | The harness change is what needed reviewing and testing; the answers cost a metered run. An unrecorded probe is reported, not silently passed, so the gap stays visible until it is filled. |
| 2026-09-17 | Renumber a shadow's parameters instead of declaring their types | The planned `ParameterDescription` capture existed only to keep an unused parameter typeable. Dropping the unused ones removes the need, and with it a `Describe` tracker, an OID cache, and a dependency on the client having described the statement at all. |
| 2026-09-17 | Let the backend infer the shadow's parameter types | A kept parameter sits in the expression it was already used in, so inference sees the same context. Where it cannot, the shadow fails into the existing fallback rather than guessing. |
| 2026-09-17 | Find the ASYNC keyword with `pg_query.Scan`, not a regex | The lexer tokenizes what the grammar rejects, so the keyword's bounds and the statement boundaries both come from the real scanner. This is what a multi-statement query needed, and it removes the last text-matching mechanism from the rewrite path. |
| 2026-09-17 | Disambiguate ASYNC by the token that follows it | `ALTER TABLE async ADD COLUMN b int` is a table named async, not the dialect's form; an action keyword follows a name, an identifier follows the keyword. |
| 2026-09-17 | Count CommandCompletes to place the job id | A multi-statement query answers once per statement, and splicing the job row onto the first one would be a quieter wrong answer than the syntax error being fixed. |
| 2026-09-17 | The README states current behavior only; history lives in this log | A delta is only legible to someone who knows the previous state, which no reader of a README has. Written into `AGENTS.md` so it is a rule rather than a review comment. |
| 2026-09-17 | Change detection waives what the comparison waives | The first real re-recording rewrote two fixtures for nothing but generated job ids. A record that is not enforced on a value should not be rewritten for it either. |
| 2026-09-17 | Restored the two fixtures the fixed rule would not have written | The run found nothing in them; leaving the rewrite in would have put exactly the diff this work exists to remove into the record's history. |
| 2026-09-17 | An unchanged fixture is not rewritten, timestamp included | The record's git history should show the runs that found a difference. A timestamp-only diff on thirteen files hides them. |
| 2026-09-17 | Compare by marshalling both sides with the timestamp cleared | An equality function over the record would have to be updated whenever a field is added, and would fail silently when it was not. |
| 2026-09-17 | Skip the write rather than carry the old timestamp forward | Same bytes either way, but an untouched file keeps its mtime, so "nothing changed" is visible without git. |
| 2026-09-17 | The unchanged-record test reconstructs suite order | `LoadDir` sorts by name and a run appends in suite order, so a load-and-save round trip is not a fixed point; testing it would have asserted the wrong property. |
| 2026-09-17 | Adjudicate with the backend's lock manager, not a write-intent registry | PostgreSQL's row locks already are the write-intent graph, key-column-aware down to `KEY SHARE` versus `NO KEY EXCLUSIVE`. A registry would have had to re-derive what the backend already knows, and parse key predicates to do it. |
| 2026-09-17 | Rejected the `BEFORE ROW` trigger registry | Measured: `GetTupleForTrigger` locks the tuple before the trigger fires, so the trigger never runs on the conflicting path. The design looked cheapest and does not work. |
| 2026-09-17 | `lock_timeout` 50ms, session-wide, from the ruleset | Turns an unbounded wait into evidence of a conflict. Session-wide costs a DDL lock wait being reported as a conflict, which errs toward DSQL's lock-free model rather than PostgreSQL's. |
| 2026-09-17 | One savepoint per transaction, not per statement | A transaction that rolls back to it is doomed, so the work it loses is discarded at `COMMIT` anyway; one subtransaction keeps the backend's stack shallow. The cost is that a doomed transaction does not read its own writes. |
| 2026-09-17 | Answer a refused statement with a shadow rather than a guessed row count | The command tag is what the conformance probes compare, and fabricating it would make the record meaningless. A statement with no exact shadow is not taken over at all. |
| 2026-09-17 | `ConflictRace` rather than permuting session assignments | Permutation was tried first and is wrong: the sessions in a case run different statements, so swapping them compares a `FOR UPDATE` against an `UPDATE`. What varies is which transaction loses, not which statements it ran. |
| 2026-09-17 | `CompareSuites` takes the replay policy from the suite, not the record | Lets a case stop being record-only without re-recording, which costs money and a cluster. The recorded observations stay untouched. |
| 2026-09-15 | Build in Go | `jackc/pgproto3` is purpose-built for transparent PG proxies; `pg_query_go` binds real libpg_query. Installed Go 1.27.1 via Homebrew. |
| 2026-09-15 | Module path `github.com/Dreamescaper/dsql-emulator` | Matches the published GitHub repo. |
| 2026-09-15 | Repo `Dreamescaper/dsql-emulator` is public | Matches the prior-art projects' approach. |
| 2026-09-15 | One upstream connection per client | Correct transaction and `SET` semantics; pooling deferred. |
| 2026-09-15 | Raw byte relay for M0 | Proves end-to-end connectivity with no protocol assumptions before adding interception. |
| 2026-09-15 | Foreign keys are a supported OCC feature | Recent DSQL addition; belongs in the adjudicator, not the reject list. See PLAN.md. |
| 2026-09-15 | Ruleset lives in `rules/` as a package | `go:embed` cannot reach outside its package directory, so the loader and the YAML share `rules/` instead of splitting across `internal/rules` and `rules/`. |
| 2026-09-15 | Parse errors are forwarded, not rejected | DSQL-only syntax such as `CREATE INDEX ASYNC` does not parse with stock libpg_query. Treating parse failure as incompatibility would wrongly reject valid DSQL. |
| 2026-09-15 | Extended-protocol rejection drops messages until `Sync` | Mirrors PostgreSQL: after an error the server ignores messages until the next `Sync`, and this keeps the upstream connection in step with the client. |
| 2026-09-16 | A failed baseline run saves nothing | A partial record would replace a complete one, and rebuilding it costs a metered cluster run. Refusing to save is recoverable; a truncated record is not. |
| 2026-09-16 | `SaveDir` writes before it prunes | A failure part-way through a save leaves the fixtures it has not replaced, instead of an empty directory. |
| 2026-09-16 | An async DDL is admitted at Bind, not Parse | Every other prepared statement is admitted when it runs, because clients cache statements and re-run them without a new Parse. Admitting at both points spent the transaction's single DDL twice. |
| 2026-09-16 | ALTER TABLE type rules duplicated with YAML anchors | A type is unsupported wherever a column declares it, but `matches` keys a rule to one statement node. An anchor keeps the `create_stmt` and `alter_table_stmt` lists identical without a schema change to `Rule.Stmt`. |
| 2026-09-16 | The ALTER TABLE type probes ship unrecorded | `make baseline` costs money and needs a fresh token, so the probes are added and reported as uncovered rather than recorded from a guess. |
| 2026-09-16 | The primary-key guard reads object addresses, not statement text | `sql_drop` reports what a command actually dropped as integer object addresses, so quoting, `IF EXISTS`, `ONLY`, schema qualification and multi-action commands need no parsing. A snapshot at `ddl_command_start` supplies the keys, because they are gone by the time `sql_drop` fires. |
| 2026-09-16 | ASYNC statements are parsed after the keyword is stripped | One narrow regex is unavoidable, because libpg_query rejects the keyword. Everything after it can be decided on a real parse tree, which is what the repository requires elsewhere and what lets the ruleset apply to these statements at all. |
| 2026-09-16 | A schema-qualified index name is forwarded, not rejected in Go | PostgreSQL's grammar refuses it with the same `42601 syntax error at or near "."` the dialect reports, so hard-coding the message duplicated the backend for no gain. |
| 2026-09-16 | The job id travels in a marker comment | It costs no extra round trip, survives into `current_query()` for both the simple and the extended protocol, works for a statement that names no object, and gives each build its own id as a real cluster does. |
| 2026-09-16 | The emulator keeps issuing UUID job ids | The recording suggests DSQL uses 26-character base32, but nothing observed confirms what `wait_for_job` accepts. A UUID stays consistent with the cast that reproduces the verified `22P02`, and the question is recorded in the backlog instead. |
| 2026-09-16 | `ALTER COLUMN ... TYPE` is refused as a statement form | The recorded refusal names no type, where the `ADD COLUMN` one does, and a supported target type returns the identical message. The rule is ordered ahead of the type rules so its wording, not theirs, answers a retype. |
| 2026-09-16 | Setup drops dependents first, as cleanup does | A re-run meets the previous run's objects, including the foreign key a case adds, so the two orderings have to agree or the suite cannot start. |
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

Every milestone is done, the adjudicator included, and PLAN.md's verification
backlog is empty. What remains:

### 1. Smaller items

- IAM tokens are accepted but not validated; validating them means owning the
  client authentication exchange (a SCRAM handshake on the upstream).
- `occ.inject` rules get no validation: an empty or duplicate `id`, or a
  negative `every`, loads silently where an `unsupported` rule would not.
- One conformance run took ~18s instead of ~1s and never reproduced; worth a
  glance if it returns.
- `occ_multirow_predicate` is a known gap on one recording. A second run would
  say whether DSQL always fails both sides of a multi-row conflict or whether
  that run's commits simply landed together.
