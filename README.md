# ethertest

`ethertest` is an embeddable Go local Ethereum test node with both execution
JSON-RPC and a synthetic Beacon REST/SSE surface. It targets DApps, wallets,
explorers, indexers, and other off-chain applications that need realistic
cross-layer behavior without P2P or a validator client.

The current version is `0.1.0-alpha.1`. State format compatibility is not
promised until `v0.1.0`.

## Quick start

```sh
go build -trimpath -o bin/ethertest ./cmd/ethertest
./bin/ethertest
```

Defaults:

- EL HTTP+WS and Beacon REST+SSE: `http://127.0.0.1:8545`
- Beacon API paths start at `http://127.0.0.1:8545/eth/`
- chain/network ID: `31337`
- 10 Anvil-compatible accounts with 10,000 ETH each
- 6-second slots, 8 slots/epoch, 64 deterministic validators
- Osaka/Fulu active at genesis
- synthetic safe/finalized checkpoints lagging one/two epochs

The startup banner prints development private keys. Keys are never included in
JSON logs, errors, metrics, or state archives.

### Docker Compose

The included Compose example builds the CGO-free, nonroot image, publishes the
shared API listener to host loopback only, and persists Pebble plus the
graceful-shutdown state archive:

```sh
docker compose up --build
docker compose down
```

Execution RPC is available at `http://127.0.0.1:8545` and Beacon REST/SSE uses
the same listener, for example
`http://127.0.0.1:8545/eth/v1/beacon/headers/head`.
`docker compose down` sends SIGTERM and allows the node to write
`/state/ethertest-state.tar.zst` before exit. Use
`docker compose down -v` only when intentionally deleting both named volumes.

## Implemented alpha surface

The library exposes `Config`, `Node`, in-process RPC clients, endpoint discovery,
transaction submission, manual mining, missed slots, snapshots, persistent
repeatable checkpoints, persistent explicit branches, canonical switching,
bounded persistent event replay, and atomic state archives.

The network surface currently includes:

- Core `eth`, `net`, `web3`, `txpool`, `miner`, `personal`, and `debug` methods.
- EIP-1186 proofs, state overrides, polling filters, `newHeads`, HTTP/WS batch,
  struct logging, and native Go tracers. JavaScript tracers are rejected.
- Type-3 raw submission with mandatory KZG validation, Deneb JSON/SSZ sidecars,
  Osaka cell proofs, `packed-bytes-v1`, stable blob retrieval, and Fulu data
  columns.
- Beacon genesis/config/health, signed headers and blocks, validators/balances,
  Deneb→Electra/Fulu container transitions, synthetic finality, JSON/SSZ
  negotiation, and live bounded SSE replay.
- Memory and Pebble databases; checksum-verified, zstd-compressed state archives.

Not yet release-complete: unsafe header sessions and taint propagation,
execution request and withdrawal queue controls (containers are present but
queues are empty), finality pause/resume controls, complete RPC compatibility,
generated full upstream API contracts, all official vector suites, encrypted
secret packages, resource pruning modes, representative-client E2E, and release
provenance/signing. These remain gates for `v0.1.0`; the current tree must not
be tagged as that release.

## CLI

```text
ethertest [flags]
ethertest config print|validate
ethertest network --json
ethertest blob encode|decode|send
ethertest state inspect|dump|load|migrate
ethertest accounts export --unsafe-plain
ethertest capabilities
ethertest completion bash|zsh|fish
```

Configuration precedence is defaults, strict TOML, `ETHERTEST_*`, then CLI.
Unknown TOML keys and conflicting settings are rejected. Non-loopback listeners
require explicit `--allow-unsafe-external`. TLS only uses user-provided
certificate/key pairs and never modifies a trust store.
Beacon exposes only an `enabled` setting and inherits the shared HTTP address,
CORS, TLS, request limits, and unsafe-external policy. Disabling HTTP disables
all network APIs; library configurations cannot enable Beacon without HTTP.

## Library

```go
cfg := ethertest.DefaultConfig()
cfg.HTTP.Enabled = false
cfg.Beacon.Enabled = false

node, err := ethertest.New(cfg)
if err != nil { /* handle */ }
if err := node.Start(); err != nil { /* handle */ }
defer node.Close()

client := node.RPCClient()
defer client.Close()
```

All public writes pass through one controller. Queries use geth's immutable
committed headers/state roots and may run concurrently. Persistent namespaces
share one database, but the current alpha does not yet provide a crash-atomic
transaction spanning geth's block writes and every auxiliary namespace.

## Development

```sh
make test
make test-race
make lint
CGO_ENABLED=0 make build
```

Upstream protocol inputs are pinned in `spec.lock`; the consumed minimal/Fulu
constants and API surface contracts are vendored under `specs/upstream`.

## Licensing

Original code is MIT licensed. go-ethereum is LGPL-3.0; see
`THIRD_PARTY_NOTICES.md`. A static binary release remains gated on a formal LGPL
distribution review and reproducible corresponding-source materials.
