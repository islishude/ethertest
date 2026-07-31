# Repository instructions

## Scope

This repository implements `github.com/islishude/ethertest`, an embeddable Go
local Ethereum test node. Keep changes within the agreed v0.1 scope in
`README.md`: no remote fork, EL/CL P2P, Engine API, GraphQL, IPC, JavaScript
tracers, validator-client integration, or production-scale claims.

The current version is an alpha. Do not tag or describe it as release-complete
until every gate listed under "Not yet release-complete" in `README.md` is
closed with test evidence.

## Architecture invariants

- All public writes must pass through the `Node` single-writer controller.
- Do not expose geth `StateDB`, mutable chain internals, or raw stores through
  the public Go API.
- Keep execution blocks, Beacon projections, retained blob data, control
  metadata, and revision events consistent. If a change cannot be crash-atomic,
  document the recovery boundary and fail visibly.
- KZG verification is mandatory and must never gain a bypass.
- Finality is synthetic and must be identified as such in APIs and docs.
- EL `safe`/`finalized` tags and Beacon checkpoints must resolve through the
  same slot-based result, including missed slots.
- Reorg publication order is removed events first, then new canonical events.
- `evm_snapshot`/revert remains one-shot; named checkpoints remain repeatable.
- Unsafe header mutation must not be added without permanent branch/archive
  taint propagation.

## Protocol and API changes

- Keep fork activation paired across EL and CL. Test the exact boundary slot
  when changing Cancun/Deneb, Prague/Electra, or Osaka/Fulu behavior.
- New non-standard RPC methods use the `ethertest_*` namespace. Preserve
  intentional `anvil_*` and `evm_*` aliases.
- Do not register methods whose P2P, sync, freezer, Engine API, or validator
  semantics cannot be represented truthfully.
- Beacon JSON and SSZ responses must describe the same object and include the
  correct `Eth-Consensus-Version`.
- Changes to vendored protocol subsets require a matching `spec.lock` update.
  Normal builds and tests must remain offline.

## Security and secrets

- Default accounts are public development keys. Never place private keys or the
  mnemonic in JSON logs, metrics, errors, manifests, or ordinary state archives.
- Non-loopback listeners require explicit unsafe opt-in.
- TLS accepts user-provided cert/key pairs only and must fail before binding
  when the material is invalid.
- Preserve the nonroot, CGO-free container runtime.

## Development workflow

Run formatting and the checks relevant to the change:

```sh
gofmt -w *.go cmd/ethertest/*.go
go test ./...
go vet ./...
CGO_ENABLED=0 go test ./...
go test -race ./...
```

Use `go test ./...` for normal changes; run the CGO-free and race gates for
concurrency, persistence, RPC, consensus, KZG, or release-facing changes. Add a
regression test for every corrected invariant.

Do not edit unrelated user changes, stage files, create commits, publish images,
or create releases unless explicitly requested.

## Documentation and delivery

- Keep `README.md`, `example.ethertest.toml`, `compose.yaml`, and CLI help aligned
  with actual behavior.
- Distinguish implemented behavior, locally passing checks, skipped external
  suites, and remaining release gates.
- Static distribution remains gated on the LGPL review and corresponding-source
  materials described in `THIRD_PARTY_NOTICES.md`.
