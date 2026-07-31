.PHONY: all build test test-race lint generate-check clean

GO ?= go

all: test build

build:
	$(GO) build -trimpath -o bin/ethertest ./cmd/ethertest

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

lint:
	$(GO) vet ./...

generate-check:
	$(GO) generate ./...
	git diff --exit-code

clean:
	$(GO) clean -testcache
	rm -rf bin coverage dist
