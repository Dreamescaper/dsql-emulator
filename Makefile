BINARY := bin/dsql-emu
GO ?= go

.PHONY: build run test test-integration vet tidy up down clean

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
