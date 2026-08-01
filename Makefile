.PHONY: all build test test-race lint generate-check clean

GO ?= go
GOLANGCI_LINT ?= golangci-lint

all: test build

build:
	$(GO) build -trimpath -o bin/ethertest ./cmd/ethertest

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

lint:
	$(GOLANGCI_LINT) run

generate-check:
	$(GO) run ./internal/beaconcontractgen -check

clean:
	$(GO) clean -testcache
	rm -rf bin coverage dist
