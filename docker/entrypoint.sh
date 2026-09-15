#!/bin/sh
# Starts PostgreSQL on an internal port, waits for it, then runs the emulator in
# front of it on the port clients connect to. PostgreSQL's own entrypoint still
# runs first so the data directory is initialised and the scripts in
# /docker-entrypoint-initdb.d are applied.
set -eu

# DSQL clients authenticate with a short-lived token, so the backing database
# accepts any password by default.
export POSTGRES_HOST_AUTH_METHOD="${POSTGRES_HOST_AUTH_METHOD:-trust}"

PG_PORT="${DSQL_PG_PORT:-5433}"
EMULATOR_PORT="${DSQL_PORT:-5432}"

/usr/local/bin/docker-entrypoint.sh "$@" &
postgres_pid=$!

until pg_isready -q -h 127.0.0.1 -p "$PG_PORT" -U "${POSTGRES_USER:-postgres}"; do
    if ! kill -0 "$postgres_pid" 2>/dev/null; then
        echo "postgres exited during startup" >&2
        exit 1
    fi
    sleep 0.2
done

/usr/local/bin/dsql-emu \
    --listen "0.0.0.0:${EMULATOR_PORT}" \
    --upstream "127.0.0.1:${PG_PORT}" &
emulator_pid=$!

shutdown() {
    trap - TERM INT
    kill -TERM "$emulator_pid" "$postgres_pid" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap shutdown TERM INT

# Exit when either process does.
while kill -0 "$emulator_pid" 2>/dev/null && kill -0 "$postgres_pid" 2>/dev/null; do
    sleep 0.5
done
shutdown
