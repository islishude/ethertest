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

`execution-rpc-subset.json` is the complete 78-method registration audit for
Execution APIs `v1.0.0-beta.7` at commit
`5aebdfdd45cadeb723be4bd45b4611b71c8b1c85`. Every method records either
`implemented` or `excluded`; excluded entries also record the current reason and
the condition required to add them. The file is a classification contract, not
a vendored copy of the full upstream OpenRPC schema.
