# AGENTS.md

Instructions for agents working in this repository.

## What this is

A local emulator for Amazon Aurora DSQL. It is a PostgreSQL wire-protocol proxy
in front of a standard PostgreSQL container that emulates DSQL's dialect,
transaction semantics, and OCC conflict behavior. See `docs/PLAN.md` for the
design and roadmap, and `docs/PROGRESS.md` for status.

## Toolchain

- Go 1.27+, installed via Homebrew. If `go` is not on `PATH`:
  `export PATH="/opt/homebrew/bin:$PATH"`.
- Docker is required for integration tests.
- The backing database needs the init scripts in `docker/init/` (the `sys`
  schema and the row-cap trigger). `docker-compose.yml` mounts them, and the
  container-backed tests pass them to testcontainers. A hand-rolled PostgreSQL
  without them will not enforce the DML row cap.

## Commands

```sh
make build              # build bin/dsql-emu
make test               # unit tests, no Docker
make test-integration   # container-backed tests, requires Docker
make vet                # go vet ./...
make up                 # start PostgreSQL 17 on host port 5433
make run                # run the emulator (listen 5432 -> upstream 5433)
make down               # stop Postgres and remove volumes
make baseline-dry-run   # print the conformance suite without connecting
make baseline           # record a golden record from a real DSQL cluster
make conformance        # diff the emulator against the golden record
```

Always run `make build`, `make vet`, and `make test` before reporting work done.
Run `make test-integration` when the change touches the proxy, relay, wire
protocol, or conformance harness. `make baseline` talks to a real cluster and
costs money: never run it without an explicit request and a fresh token.

## Documentation maintenance — required

Two documents must be kept current. Treat updating them as part of the task, not
an afterthought. Update them in the same turn as the code change.

### `docs/PLAN.md` — the design (stable)

Update **in place** when any of these change:

- architecture or component boundaries
- the milestone table (status, or a new milestone)
- OCC design, session FSM rules, or the ruleset schema
- goals, non-goals, or the fidelity target
- the verification backlog (when an open item is answered, remove it and record
  the answer in the relevant section)

### `docs/PROGRESS.md` — the log (append-only)

On every work session:

1. Update **Current status** and the **Last updated** date.
2. Add an entry at the **top** of the `## Completed` section, newest first.
   Include:
   - what was delivered, with the files touched
   - verification actually performed, with the exact commands and passing output
   - any deliberate limitations or known gaps
3. Add rows to the **Decisions log** for any non-obvious choice, with the
   rationale.
4. Update **Next up** to reflect what is now unblocked.

Rules:

- Record only work that was actually verified. Never mark something done based on
  intent.
- If verification failed or is blocked, say so explicitly and keep the milestone
  status as in-progress in `docs/PLAN.md`.
- Keep `docs/PLAN.md` milestone status in sync with `docs/PROGRESS.md`.

## Code conventions

- Do not add code comments unless a comment is genuinely warranted.
- Do not commit, amend, push, or open PRs unless explicitly asked.
- Prefer the standard library; add a dependency only when it clearly earns its
  place (as `pgproto3`, `pgx`, and `testcontainers-go` do).
- Bind tests to `127.0.0.1:0` and read the bound address from the API rather than
  hardcoding ports.
- Protocol classification must use a real parser (libpg_query), never regex.
