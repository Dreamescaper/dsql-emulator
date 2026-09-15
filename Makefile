BINARY := bin/dsql-emu
BASELINE := bin/dsql-baseline
GO ?= go
GOLDEN_DIR ?= test/conformance/golden
IMAGE ?= ghcr.io/dreamescaper/dsql-emulator
TAG ?= latest

.PHONY: build run test test-integration vet tidy up down clean baseline baseline-dry-run conformance docker-build docker-push

build:
	$(GO) build -o $(BINARY) ./cmd/dsql-emu

run: build
	$(BINARY) --listen 127.0.0.1:5432 --upstream 127.0.0.1:5433 --log-level debug

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags integration -count=1 ./test/...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

up:
	docker compose up -d --wait

down:
	docker compose down -v

clean:
	rm -rf bin

# Record how a real Aurora DSQL cluster answers the probe suite. Requires an
# IAM auth token, and drops every object it creates.
baseline:
	$(GO) run ./cmd/dsql-baseline \
		--host "$${DSQL_HOST:?set DSQL_HOST to the cluster endpoint}" \
		--token "$${DSQL_TOKEN:?set DSQL_TOKEN to a fresh auth token}" \
		--out-dir $(GOLDEN_DIR)

baseline-dry-run:
	$(GO) run ./cmd/dsql-baseline --dry-run

# Check the emulator against the recorded baseline (does not touch a cluster).
conformance:
	$(GO) test -tags integration -count=1 -run TestConformanceAgainstEmulator ./test/conformance/

# One container serves the whole emulator: PostgreSQL with the init scripts it
# needs, plus the proxy on port 5432.
docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push: docker-build
	docker push $(IMAGE):$(TAG)
