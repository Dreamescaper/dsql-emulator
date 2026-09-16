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

Every milestone is done. What remains is depth on the two areas that are still
thin, in the order I would take them.

### 1. Multi-session conformance, then OCC and FK conflicts

The suite runs one connection, so DSQL's conflict output is unverified: the
`40001`/`OC000` wording, whether the loser fails at the statement or at commit,
and how `FOR KEY SHARE` and foreign keys adjudicate. Integration tests cover the
emulator's behavior, but nothing pins it to the real cluster.

- Extend the case model with concurrent sessions, per-step timeouts, and a
  record-only mode, because a blocking PostgreSQL conflict cannot be replayed
  safely against the emulator.
- Probe a write-write conflict, `SELECT ... FOR UPDATE` versus a write, and the
  foreign-key delete-referenced-row / insert-referencing-row pair.
- Reconcile the emulator, and settle whether `SET DEFAULT` and `CASCADE`
  conflict like `SET NULL`.

### 2. `sys.jobs` lifecycle

The surface exists but is empty: `CREATE INDEX ASYNC` returns a `job_id` and
`sys.jobs` is a table nothing writes to, with `sys.wait_for_job` a stub. Record
a job row per async index build and make `wait_for_job` meaningful.

### 3. OCC adjudicator (mode 3)

Conflicts are detected by PostgreSQL, which blocks before failing; DSQL is
lock-free. A write-intent registry would approximate commit-time conflict
without the block. Large, and worth doing only once the behavior above is
pinned by a recording.

### Smaller items

- IAM tokens are accepted but not validated; validating them means owning the
  client authentication exchange (a SCRAM handshake on the upstream).
- One conformance run took ~18s instead of ~1s and never reproduced; worth a
  glance if it returns.
