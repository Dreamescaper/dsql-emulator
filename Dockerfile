# syntax=docker/dockerfile:1

# The emulator embeds libpg_query through cgo, so the build needs a C toolchain.
FROM golang:1.27-alpine AS build
RUN apk add --no-cache build-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -o /out/dsql-emu ./cmd/dsql-emu

# One container serves the whole emulator: PostgreSQL with the init scripts the
# emulator needs, plus the proxy in front of it.
FROM postgres:17-alpine

COPY --from=build /out/dsql-emu /usr/local/bin/dsql-emu
COPY docker/init/ /docker-entrypoint-initdb.d/
COPY docker/entrypoint.sh /usr/local/bin/dsql-entrypoint.sh
RUN chmod +x /usr/local/bin/dsql-entrypoint.sh

# DSQL clients authenticate with a short-lived token; a trust-backed database
# accepts any password, so a token works without being validated.
ENV POSTGRES_USER=postgres \
    POSTGRES_DB=postgres \
    PGPORT=5433 \
    DSQL_PORT=5432

EXPOSE 5432
HEALTHCHECK --interval=5s --timeout=3s --start-period=60s \
    CMD pg_isready -h 127.0.0.1 -p 5432 -U postgres || exit 1

ENTRYPOINT ["/usr/local/bin/dsql-entrypoint.sh"]
CMD ["postgres"]
