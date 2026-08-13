.PHONY: all build test test-race test-rpc-e2e lint generate-check clean

GO ?= go
GOLANGCI_LINT ?= golangci-lint
NPM ?= npm
RPC_E2E_DIR := e2e/rpc

all: test build

build:
	$(GO) build -trimpath -o bin/ethertest ./cmd/ethertest

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

test-rpc-e2e: build
	$(NPM) --prefix $(RPC_E2E_DIR) ci --ignore-scripts
	RPC_E2E_BINARY=$(CURDIR)/bin/ethertest $(NPM) --prefix $(RPC_E2E_DIR) test

lint:
	$(GOLANGCI_LINT) run

generate-check:
	$(GO) run ./internal/beaconcontractgen -check

clean:
	$(GO) clean -testcache
	rm -rf bin coverage dist $(RPC_E2E_DIR)/node_modules
