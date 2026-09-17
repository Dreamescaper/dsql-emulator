# Aurora DSQL Emulator
[![Stand With Ukraine](https://raw.githubusercontent.com/vshymanskyy/StandWithUkraine/main/badges/StandWithUkraine.svg)](https://stand-with-ukraine.pp.ua)

A local emulator for [Amazon Aurora DSQL](https://aws.amazon.com/rds/aurora/dsql/):
a PostgreSQL wire-protocol proxy in front of a standard PostgreSQL server that
emulates DSQL's dialect, transaction semantics, and optimistic concurrency
control. Point an application or a test suite at it instead of a cluster.

It is not affiliated with AWS, and it is a development and testing tool, not a
drop-in replacement for the service.

## What it is for

Developing against DSQL means learning rules that PostgreSQL does not enforce:
no user-defined types, one DDL per transaction, a 3000-row limit per
transaction, conflicts that surface as `40001` at commit, and no lock waits.
This emulator rejects what DSQL rejects, enforces the transaction rules, and
reports conflicts the way DSQL reports them, so failures show up locally rather
than in a deployment.

The behavior is not guessed. `test/conformance/golden/` holds a record of what a
real cluster answered for 218 probes, and the emulator is diffed against it. A
probe added since the last recording is reported as unrecorded rather than
silently passing. Every probe in the suite is currently recorded, and the
emulator matches all of them but one accepted divergence.

## Quick start

### Docker

One container serves the whole thing: PostgreSQL with the required init scripts,
and the proxy in front of it.

```sh
docker run --rm -p 5432:5432 ghcr.io/dreamescaper/dsql-emulator:latest
```

Images are published to GHCR when a release is cut, for `linux/amd64` and
`linux/arm64`, tagged with the release version (for example `1.2.3`) and with
`latest` for a non-prerelease. Pin a version tag when you want a reproducible
build.

Then connect as a DSQL client would — TLS is required and the password is an
opaque token:

```sh
PGPASSWORD=any-iam-token psql \
  "host=127.0.0.1 port=5432 user=admin dbname=postgres sslmode=require" \
  -c "select version()"
```

### From source

```sh
make up      # PostgreSQL 16 on host port 5433
make run     # the emulator on 127.0.0.1:5432, talking to 5433
```

### In tests

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
conn, err := pgx.Connect(ctx, dsn)
```

## What it emulates

| Area | Behavior |
|------|----------|
| Connection | PostgreSQL wire protocol, TLS termination so `sslmode=require` works, the `admin` user DSQL uses, `server_version` `16.15` and `version()` `PostgreSQL 16`, `REPEATABLE READ` pinned |
| Dialect | Forty-five rules over a real parse tree: `TRUNCATE`, extensions, triggers, extra databases, temporary and unlogged tables, `serial`, materialized views, `CREATE TABLE AS`, custom types, tablespaces, foreign tables, `VACUUM`, `LISTEN`/`NOTIFY`, `ALTER SYSTEM`, `MERGE`, `TABLESAMPLE`, text search, geometric types, and more |
| Transactions | One DDL per transaction, DDL and DML in separate transactions, a 3000-row cap, a 30-minute age limit, and the aborted-transaction state (`25P02`, then `ROLLBACK` on `COMMIT`) |
| Types | The documented supported set including aliases, identity columns and sequences with the required `CACHE`, domains, enums refused the way DSQL refuses them. The list applies to a column added by `ALTER TABLE ADD COLUMN` as well as one a `CREATE TABLE` declares |
| Indexes | `CREATE INDEX ASYNC` rewritten, answered with a `job_id`, and recorded in `sys.jobs`, in a single statement or in a multi-statement query such as `psql -c 'a; b'` sends; supports **partial indexes** (`WHERE`), expressions, `INCLUDE`, and `NULLS NOT DISTINCT`; synchronous `CREATE INDEX` and a schema-qualified index name are refused |
| `ALTER TABLE` | `DROP COLUMN`, `ADD COLUMN` with `STORAGE`, `SET STORAGE`, `ADD CONSTRAINT ... NOT VALID`, `RENAME`, and `SET SCHEMA`. `ALTER COLUMN ... TYPE` is refused whatever the target type, and dropping a primary-key column is refused. A `CHECK` or `FOREIGN KEY` added by `ALTER TABLE` **must** use `NOT VALID` and is validated through `ALTER TABLE ASYNC ... VALIDATE CONSTRAINT`, which returns a `job_id` recorded in `sys.jobs`; the synchronous form is refused |
| OCC | Conflicts adjudicated at `COMMIT` without waiting for locks, reported as `40001 change conflicts with another transaction (OC000)`, across write-write, `FOR UPDATE`, `FOR KEY SHARE` and foreign-key overlap; plus deterministic injection of conflicts so retry loops can be tested |
| Environment | Single `postgres` database, `UTC`, `admin` user, `sys.jobs` recording each index build |

## What it does not do

- **The loser is the first writer refused a lock, not the second committer.**
  The backend's lock wait is bounded, the refused statement is answered as if it
  had run, and the transaction fails at `COMMIT` the way DSQL fails it. Which
  transaction loses is therefore decided when the rows are asked for rather than
  when they are committed, so a transaction that commits in a different order
  than it wrote can lose where DSQL would not. A doomed transaction does not
  read its own writes. A conflict on a `RETURNING`, an `INSERT ... SELECT`, an
  `ON CONFLICT` or a multi-statement query is reported at the statement rather
  than at `COMMIT`.
- **IAM tokens are accepted, not validated.** The backing database is
  trust-configured, so any password connects. Nothing checks the token.
- **`sys.jobs` records index builds and constraint validation only.** `CREATE INDEX ASYNC` builds the
  index synchronously and records a completed `INDEX_BUILD` job, matching DSQL's
  columns, statuses, and `sys.wait_for_job` being a procedure. DSQL also records
  `ANALYZE` and `DROP` jobs, which the emulator does not, and its ids are 26
  characters (`tpqrncdmjja4tdl3zxo2qqvh4y`) where the emulator's are UUIDs. One
  case returns an id with no row:
  `CREATE INDEX ASYNC IF NOT EXISTS` on an index that already exists builds
  nothing, so there is no job to record.
- **The reported version leaks on unusual paths.** `version()`,
  `SHOW server_version`, `current_setting('server_version')`,
  `current_setting('server_version_num')`, and `SHOW server_version_num` are
  rewritten to DSQL's values. The rewrites match whole statements, so a version
  read another way reports the backing engine.
- **Rejection wording is approximate.** The SQLSTATE is the contract;
  messages mirror DSQL's meaning and drift.
- **No control plane.** Cluster creation, IAM and tagging are out of scope;
  [LocalStack](https://docs.localstack.cloud/aws/services/dsql/) covers those.
- The backing database needs the scripts in `docker/init/` (the `sys` schema and
  `sys.jobs`, the DML row-cap trigger, and the `admin` role clients connect as).
  The image and `docker-compose.yml` provide them; a hand-rolled PostgreSQL
  will not enforce the row cap or accept the `admin` user.

## Configuration

`dsql-emu` flags:

| Flag | Default | Meaning |
|------|---------|---------|
| `--listen` | `127.0.0.1:5432` | address clients connect to |
| `--upstream` | `127.0.0.1:5433` | backing PostgreSQL |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `--tls-cert`, `--tls-key` | | PEM pair for client TLS; a self-signed certificate is generated when unset |
| `--no-tls` | `false` | decline client TLS and stay plaintext |
| `--server-version` | `16.15` | `server_version` reported to clients |

The image adds `DSQL_PORT` (5432) and `DSQL_PG_PORT` (5433) to move the two
listeners, passes `POSTGRES_*` through to the backing database, and reports
readiness by logging `proxy listening`.

## The conformance suite

The ruleset is only as good as its evidence, so the emulator ships the evidence
and a harness to check it.

```sh
make baseline-dry-run   # print the probe suite, no connection
make baseline           # record a golden record from a real cluster (costs money)
                        # only the fixtures whose answers changed are rewritten
make conformance        # diff the emulator against the record, needs Docker only
```

`make baseline` needs `DSQL_HOST` and `DSQL_TOKEN`, and is the only command that
talks to a cluster. Probes cover the dialect, types, transactions, queries,
concurrency, multi-statement queries, and the connection environment. Conflicting concurrency cases are
replayed and enforced like the rest: which transaction loses is a race on both
sides, so what is checked is that one lost, with the same SQLSTATE, at the step
the conflict surfaces at.

## Releases

Cut a release from the Actions tab with the `release` workflow and a
`patch`/`minor`/`major` bump. It verifies the build and tests, computes the next
version from the latest `v*` tag (so the first release is `v0.1.0`), creates the
release, and publishes the image. The tag also becomes the Go module version.

## Development

```sh
make build              # bin/dsql-emu
make test               # unit tests, no Docker
make test-integration   # container-backed suites, requires Docker
make vet
make docker-build       # local image
```

GitHub Actions runs the formatting check, `go vet`, the unit tests, the
container-backed suites, and a Docker build on every push and pull request.

`docs/PLAN.md` is the design and roadmap; `docs/PROGRESS.md` is the status log,
including the decisions behind each choice and the gaps that remain.

## Prior art

- [tomodian/deesql](https://github.com/tomodian/deesql) — proxy that rejects
  unsupported SQL with real SQLSTATEs and handles IAM token auth.
- [renebrandel/dsql-local-simulator](https://github.com/renebrandel/dsql-local-simulator)
  — PostgreSQL extension that blocks unsupported DDL.
- [LocalStack](https://docs.localstack.cloud/aws/services/dsql/) — DSQL control
  plane APIs with an embedded-PostgreSQL data plane.

## License

MIT. See [LICENSE](./LICENSE).
