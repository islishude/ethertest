# Vendored specifications

Normal builds are offline. The reviewed OpenRPC, OpenAPI, and minimal/Fulu
constant subsets consumed by the current alpha are stored below this directory;
dependency implementations remain pinned by `go.mod` and `go.sum`.

The implemented Beacon v4 subset has an offline generator for its request,
response, and error types; `go generate ./...` validates the required locked
paths and schemas before rewriting the Go types. Selected EIP-4788, KZG, and SSZ
container vectors are vendored under `testdata/vectors` with their source commit
and digest in `spec.lock`. Automated upstream refresh, full wire contracts,
license mirroring, and the complete official vector suites remain release gates.
Changes to a reviewed subset, generated output, and `spec.lock` must be made
together. A release must not contain a `PIN_REQUIRED` entry.
