# ethertest

`ethertest` is an embeddable Go local Ethereum test node with execution
JSON-RPC over HTTP, WebSocket, in-process, or IPC transports and a synthetic
Beacon REST/SSE surface. It targets DApps, wallets,
explorers, indexers, and other off-chain applications that need realistic
cross-layer behavior without P2P or a validator client.

The current version is `0.1.0-alpha.1`. State format compatibility is not
promised until `v0.1.0`. This alpha uses an in-place, breaking state layout:
databases created by the earlier layout are rejected and must be recreated;
there is no migration command.
An unspecified `genesis_time` (`0`) is resolved once for a new generated chain
and then read from the persisted timeline on later Pebble starts. An explicitly
supplied value that differs from the stored value is rejected.

## Quick start

```sh
go build -trimpath -o bin/ethertest ./cmd/ethertest
./bin/ethertest
```

Defaults:

- EL HTTP+WS and Beacon REST+SSE: `http://127.0.0.1:8545`
- IPC: disabled; when enabled its default name is `ethertest.ipc`
- Beacon API paths start at `http://127.0.0.1:8545/eth/`
- chain/network ID: `1337` (matching `geth --dev`; `network_id = 0` inherits the chain ID)
- 10 Anvil-compatible accounts with 10,000 ETH each in the generated default genesis
- 6-second slots, 8 slots/epoch, 64 deterministic validators
- Osaka/Fulu active at genesis
- synthetic safe/finalized checkpoints lagging one/two epochs
- `consensusMode: synthetic`, Beacon API `v4-subset`, `fullConsensus: false`

In human mode, a separate stderr banner prints development private keys. Keys
are never included in structured logs, errors, metrics, or state archives.

### Docker Compose

The included Compose example builds the cgo-enabled, nonroot image, publishes the
shared API listener to host loopback only, and persists Pebble plus the
graceful-shutdown state archive:

```sh
docker compose up --build
docker compose down
```

Execution RPC is available at `http://127.0.0.1:8545` and Beacon REST/SSE uses
the same listener, for example
`http://127.0.0.1:8545/eth/v1/beacon/headers/head`.
Fork-dependent blocks use
`/eth/v2/beacon/blocks/{block_id}` and Fulu data columns use
`/eth/v1/debug/beacon/data_column_sidecars/{block_id}`.
`docker compose down` sends SIGTERM and allows the node to write
`/state/ethertest-state.tar.zst` before exit. Use
`docker compose down -v` only when intentionally deleting both named volumes.

## Implemented alpha surface

The library exposes `Config`, `Node`, in-process RPC clients, endpoint discovery,
transaction submission, manual mining, missed slots, snapshots, persistent
repeatable checkpoints, persistent explicit branches, canonical switching,
bounded persistent event replay, synthetic finality pause/resume controls,
next-block withdrawal injection, native Electra execution-request projection,
persistent execution-request controls, safety queries, and atomic state
archives.
Clean histories can be replayed by geth's execution state processor. State
control methods deliberately create unsafe fixtures; the control block, every
descendant, its branches, and the containing session/archive remain tainted.

The network surface currently includes:

- Core `eth`, `net`, `web3`, `txpool`, `miner`, `personal`, and `debug` methods.
- An in-memory wallet for configured and runtime-imported signers, including
  `eth_signTypedData_v4`, standalone EIP-7702 authorization signing, and
  `ethertest_importAccount`/`ethertest_removeAccount`.
- EIP-1186 proofs, state overrides, polling filters, `newHeads`, HTTP/WS/IPC batch,
  struct logging, `debug_traceBlockByHash`, `debug_traceBlockByNumber`, and
  native Go tracers. JavaScript tracers are rejected.
- One immutable pending candidate view shared by pending block/state/call/proof
  queries, with deterministic executable/queued classification.
- `Node.AddWithdrawal` and `ethertest_addWithdrawal` for adding up to 16
  automatically indexed EIP-4895 withdrawals to the next canonical block.
- Native EIP-6110/7002/7251 execution requests from geth plus persistent
  `Node.Add*Request` and `ethertest_add*Request` control FIFOs. The same typed
  bytes back EL `requestsHash`, Beacon JSON/SSZ, restart, reorg, and archive
  behavior.
- Type-3 raw submission with mandatory KZG validation, Deneb JSON/SSZ sidecars,
  Osaka cell proofs, `packed-bytes-v1`, stable blob retrieval, and Fulu data
  columns.
- Beacon API v4.0.0 subset for genesis/config/health, signed headers and blocks,
  validators/balances,
  Deneb→Electra/Fulu container transitions, synthetic finality, JSON/SSZ
  negotiation, required-topic standard SSE replay, and structured errors.
- `Node.PauseFinality`, `Node.ResumeFinality`, `Node.FinalityStatus`, and matching
  `ethertest_*` RPC controls for persistent synthetic finality fixtures.
- Memory and Pebble databases; recovery-journaled execution/auxiliary commits;
  checksum-verified, zstd-compressed state archives.
- `Node.SafetyStatus`, `Node.BlockSafety`, `ethertest_safetyStatus`, and
  `ethertest_blockSafety` for permanent fixture taint discovery.
- Offline locked EIP-4788 wraparound, KZG proof, and SSZ container regression
  vectors; their source revisions and digests are recorded in `spec.lock`.

### Execution APIs v1.0.0-beta.7

The standard RPC registration baseline is
[`ethereum/execution-apis@v1.0.0-beta.7`](https://github.com/ethereum/execution-apis/tree/v1.0.0-beta.7),
commit `5aebdfdd45cadeb723be4bd45b4611b71c8b1c85`. The locked offline
classification in `specs/upstream/execution-rpc-subset.json` contains all 78
methods: 49 are implemented and 29 are deliberately unregistered. Existing
`web3`, `personal`, `miner`, subscription, and `ethertest`/`anvil`/`evm`
extensions remain available but are not counted in that baseline.

| Surface                                                                                                            | Status                                                                                           |
| ------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------ |
| `eth` block, transaction, receipt, state, fee, filter, signing, config, capabilities, and `eth_simulateV1` methods | 41 implemented                                                                                   |
| `debug_getRawHeader`, `debug_getRawBlock`, `debug_getRawReceipts`, `debug_getRawTransaction`                       | 4 implemented                                                                                    |
| `net_version`, `txpool_status`, `txpool_content`, `txpool_contentFrom`                                             | 4 implemented                                                                                    |
| All 25 `engine_*` methods                                                                                          | Excluded until ethertest has a real authenticated EL/CL Engine API boundary                      |
| `debug_getBadBlocks`, `testing_buildBlockV1`                                                                       | Excluded because v0.1 has no truthful sync bad-block pipeline or public upstream testing service |
| `eth_getBlockAccessList`, `debug_getRawBlockAccessList`                                                            | Excluded until Amsterdam/EIP-7928 is supported                                                   |

`safe` and `finalized` continue to be synthetic slot-derived tags. Configured
development accounts and runtime-imported accounts are accepted by `eth_sign`,
`eth_signTransaction`, `eth_sendTransaction`, and `eth_signTypedData_v4`;
signatures and errors never expose their keys or mnemonic. These wallet methods
are extensions and do not change the locked beta.7 method counts. Pending blocks
and transactions encode unconfirmed inclusion fields as `null`. Polling filters
and pending-transaction history are bounded in-memory state and are not restored
after restart. `eth_capabilities` reports the actual archive/state window while
block, transaction, log, and receipt history starts at genesis.

`eth_simulateV1` is read-only and uses sequential state across simulated
blocks. It supports block/state overrides, moved precompiles, gap-filled empty
blocks, optional validation, full transaction results, and synthetic ETH
transfer logs; omitted timestamps advance by the configured network slot
duration. Its fixed v0.1 limits are 256 output blocks, 5,000 calls per
block, 10,000 calls per request, 50,000,000 cumulative gas, and a five-second
EVM timeout. Limit exhaustion returns `-38026`; timeout returns `-32016`.

### Next-block withdrawals

`ethertest_addWithdrawal` accepts one object with `validatorIndex`, `address`,
and nonzero `amount` fields. Integer fields are JSON-RPC quantities and `amount`
is denominated in Gwei. The node assigns the globally monotonic withdrawal
`index`, refreshes the pending block and state immediately, and returns `true`
without triggering mining. The accepted withdrawals are consumed by the next
canonical block, including an explicitly empty or control block. The in-memory
queue is limited to 16 entries and is not restored by snapshots, checkpoints,
archives, or process restart.

### Electra execution requests

Every Prague-or-later block captures geth's native EIP-6110 deposit logs and
EIP-7002/EIP-7251 system-contract queue output after its final transaction.
Native typed bytes are validated strictly against the block `requestsHash`,
then persisted with the Electra/Fulu Beacon projection. Native-only blocks keep
normal geth insertion and do not taint their history. The deposit log parser
uses geth's configured `DepositContractAddress`; contract code at any other
address is not treated as EIP-6110 output. With an imported execution genesis,
capture uses that file's exact state and ChainConfig after the cross-layer
compatibility checks described below.

The Go controls `Node.AddDepositRequest`, `Node.AddWithdrawalRequest`, and
`Node.AddConsolidationRequest` have matching RPCs:

- `ethertest_addDepositRequest({pubkey, withdrawalCredentials, amount, signature, index})`
- `ethertest_addWithdrawalRequest({sourceAddress, validatorPubkey, amount})`
- `ethertest_addConsolidationRequest({sourceAddress, sourcePubkey, targetPubkey})`

RPC integer fields are hex quantities. Fixed byte lengths are checked, while
validator membership and BLS/BeaconState semantics are intentionally not
claimed. The persistent FIFO limits are 8192 deposits, 16 withdrawals, and 2
consolidations. Enqueueing refreshes `pending` without mining; requests may wait
before Prague, and interval mining advances until the first eligible block.

For each type, native entries precede control entries. Native output consumes
the protocol capacity first, so controls remain queued when a block is full.
Only canonical mining consumes controls; canonical rewinds restore consumed
IDs in FIFO order and retain later pending additions. Native queue state follows
geth's branch StateDB instead. Queue changes, native and complete typed block
records, Beacon projection, slots, safety, and events share the prepared-operation
recovery journal and survive Pebble restart and archive round trips.

Including any control entry changes `requestsHash` without applying the
corresponding execution contract/log transition. Such a block and all of its
descendants are therefore permanently unsafe with reason
`execution-request-control`; Beacon optimistic flags, safety RPCs, and archive
manifests expose that taint. No `anvil_*` or `evm_*` aliases are registered, and
the existing EIP-4895 `ethertest_addWithdrawal` queue is independent.

This is a synthetic Beacon projection, not a consensus client: it does not
implement BeaconState transitions, Casper FFG, fork choice, P2P, the Engine API,
or standard CL block import.

### Synthetic finality controls

`Node.PauseFinality` and `ethertest_pauseFinality` freeze the slot used to
derive synthetic `safe` and `finalized` checkpoints. Blocks and missed slots
continue advancing while paused. The frozen projection is shared by EL block
tags, Beacon finality checkpoints and `finalized` response flags, branch
protection, and finalized-checkpoint events.

`Node.ResumeFinality` and `ethertest_resumeFinality` immediately catch the
projection up to the current slot. If the finalized checkpoint changed, one
event for the latest checkpoint is published. Both operations are idempotent;
repeated calls do not create events or revisions. `Node.FinalityStatus` and
`ethertest_finalityStatus` report the current and projection slots plus the
resolved safe/finalized slots, hashes, and block numbers.

The paused state survives Pebble restart and state archive dump/load. A
snapshot revert, checkpoint restore, or branch switch does not resume finality;
rewinding before the frozen slot lowers that slot to the new head. These methods
are registered only in the `ethertest` namespace, not as `anvil_*` or `evm_*`
aliases.

### Runtime wallet accounts

`Node.ImportAccount` and `ethertest_importAccount` add an unlocked signer to the
node's in-memory wallet. The RPC accepts a `0x`-prefixed 32-byte secp256k1
private key and an optional absolute uint256 balance. Without a balance, import
does not change the chain. With a balance, import creates the same permanently
tainted unsafe control block as `ethertest_setBalance` and returns its hash as
`controlBlockHash`.

Configured mnemonic accounts cannot be removed. Runtime accounts can be
removed with `Node.RemoveAccount` or `ethertest_removeAccount`; removal only
withdraws signing authority and leaves balance, nonce, code, and history
untouched. Runtime membership is not restored by snapshots, checkpoints,
branches, state archives, or process restart. A persisted balance control block
survives normally even though its runtime signer does not.

`eth_signTypedData_v4` accepts either an EIP-712 object or its JSON string. If
`domain.chainId` is present it must match the node; an omitted chain ID is
allowed. The RPC returns `r || s || v` with the legacy `27/28` recovery byte.
There are no v3, legacy typed-data, password, lock/unlock, or encrypted-keystore
variants in this wallet surface.

`Node.SignAuthorization` and `ethertest_signAuthorization` sign an exact
EIP-7702 authorization tuple with a configured or runtime-imported account. The
RPC takes the authority address followed by `{chainId,address,nonce}`, where
`address` is the delegation target, and returns the standard `chainId`,
`address`, `nonce`, `yParity`, `r`, and `s` object. The target may be zero to
clear a delegation. The chain ID must match the node or be explicitly zero;
zero is the replayable cross-chain value defined by EIP-7702.

The signer never derives a nonce or submits a transaction. With viem, use
`prepareAuthorization` to resolve the chain ID, pending nonce, and
`executor: "self"` adjustment, then pass that complete tuple to the custom RPC.
Viem's standard `signAuthorization` action still rejects JSON-RPC accounts, so
this method is an explicit ethertest extension rather than transparent wallet
compatibility. Signing does not change execution state, the canonical head,
events, revisions, snapshots, or archives. No `eth_*`, `anvil_*`, or `evm_*`
alias is registered. `ethertest_capabilities` advertises
`authorizationSigning: true`.

Not yet release-complete: unsafe header mutation sessions, complete RPC
compatibility, generated full upstream API contracts, all official
vector suites, encrypted secret packages, resource pruning modes, and release
provenance/signing. These remain gates for `v0.1.0`; the current tree must not
be tagged as that release.

## CLI

```text
ethertest [flags]
ethertest config print|validate
ethertest network --json
ethertest blob encode|decode|send
ethertest state inspect|dump|load
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
HTTP, WebSocket, Beacon REST, and Beacon SSE; IPC remains independently
available. Library configurations cannot enable Beacon without HTTP.

### Execution genesis import

An optional geth-compatible execution `genesis.json` can be selected with
`Config.Chain.GenesisFile`, `[chain] genesis`, `ETHERTEST_GENESIS`, or
`--genesis`. Relative paths are resolved from the process working directory.
The selected file is authoritative for its execution ChainConfig, header fields,
gas limit, timestamp, and complete `alloc`; ethertest does not merge the default
account balances into it. Configured mnemonic accounts remain unlocked signers,
but an address omitted from `alloc` starts unfunded.

Imported chains must be proof-of-stake, start at London/Shanghai/Cancun, schedule
Prague and Osaka on exact ethertest epoch boundaries, retain the repository's
pinned Cancun/Prague blob parameters, and leave post-Osaka forks disabled. The
fixed EIP-4788, EIP-2935, EIP-7002, and EIP-7251 system contract accounts must
match geth's canonical initial code and state. Invalid files fail before the
node starts and are never rewritten.

`network_id = 0` inherits the effective execution chain ID; a nonzero
`network_id`, `ETHERTEST_NETWORK_ID`, or `--network-id` explicitly overrides
`net_version`. `ethertest config validate` parses and validates the genesis,
while `config print` and `network --json` report the imported effective chain
fields, gas limit, and fork epochs.

For Pebble, the imported marker and genesis hash are stored alongside the
timeline. Later starts may omit the source file and recover the genesis header,
alloc, and ChainConfig from the database. If a file is supplied again, both its
genesis hash and decoded ChainConfig must exactly match the stored values. A
configured but unreadable path is an error. State archives retain the same
metadata, so archive loads also restart without the source file.

IPC is opt-in. Enable it with `[ipc] enabled = true`,
`ETHERTEST_IPC_ENABLED=true`, or `--ipc PATH`; use `--no-ipc` to override an
earlier configuration layer. `ipc.path` defaults to `ethertest.ipc`. On Unix, a
simple name is placed under `storage.path` for Pebble nodes or the system
temporary directory for in-memory nodes, while a path containing a directory
is used exactly as supplied. Windows maps the configured name to a named pipe.
Unix sockets are created with mode `0600`. `ethertest network --json` reports
the resolved endpoint in its `ipc` field.

An IPC-only CLI node can be started with:

```sh
ethertest --no-http --ipc /tmp/ethertest.ipc
```

For containers, set `--ipc` to a path in a bind-mounted writable directory.
Because the image runs as nonroot, create that host directory for UID/GID
`65532` and mount the directory rather than an individual socket file, for
example `--ipc /run/ethertest/ethertest.ipc` with a bind mount at
`/run/ethertest`.

### Logging

The CLI writes structured runtime logs to stdout at `info` level. Human-only
development account output is kept on stderr so it cannot corrupt the log
stream. Lifecycle and control operations are logged immediately, while
transaction and interval automining activity is combined into one
`chain_progress` event every 10 seconds. Empty intervals and per-request access
logs are suppressed. `debug` replaces the aggregate with per-transaction and
per-block events.

Configure logging with `[log]`, `--log-level`, `--log-json`, and
`--log-progress-interval`, or with `ETHERTEST_LOG_LEVEL`,
`ETHERTEST_LOG_JSON`, and `ETHERTEST_LOG_PROGRESS_INTERVAL`. Supported levels
are `debug`, `info`, `warn`, `error`, and `off`; the minimum progress interval
is one second. JSON mode emits one object per line with stable `time`, `level`,
`msg`, and `event` fields and suppresses the human private-key banner.

Embedded nodes do not write logs unless the caller supplies
`ethertest.WithLogger(logger)`. Runtime logs never contain private keys,
mnemonics, raw transaction data, or control-state values.

## Library

```go
cfg := ethertest.DefaultConfig()
cfg.HTTP.Enabled = false
cfg.Beacon.Enabled = false
cfg.IPC.Enabled = true
cfg.IPC.Path = "/tmp/ethertest.ipc"

node, err := ethertest.New(cfg)
if err != nil { /* handle */ }
if err := node.Start(); err != nil { /* handle */ }
defer node.Close()

client := node.RPCClient()
defer client.Close()
```

`RPCClient` remains an in-process client. External clients can connect to
`node.Endpoints().IPC` with geth's `rpc.DialIPC` after `Start` returns.

Applications that want node logs can pass a host-owned `*slog.Logger` with
`ethertest.New(cfg, ethertest.WithLogger(logger))`.

Runtime signers can be managed through the library without exposing keys over
JSON-RPC:

```go
result, err := node.ImportAccount(ctx, privateKey, optionalBalance)
removed, err := node.RemoveAccount(ctx, result.Address)
```

`result.ControlBlockHash` is nil when no balance was requested.

All public writes pass through one controller. Queries use geth's immutable
committed headers/state roots and may run concurrently. Persistent namespaces
share one database, but the current alpha does not yet provide a crash-atomic
transaction spanning geth's block writes and every auxiliary namespace.

## Development

Use the Go version declared in `go.mod`.

Install the latest golangci-lint before running the lint target:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
```

```sh
make test
make test-race
make test-rpc-e2e
make lint
make build
```

`make lint` checks production and test code with the standard golangci-lint set,
the `modernize` analyzer, and `gofmt`. It uses read-only module mode
so linting cannot rewrite `go.mod` or `go.sum`; CI runs the same pinned tool and
configuration in a dedicated job.

`make test-rpc-e2e` builds and starts the real CLI on random loopback ports,
then runs the representative-client JSON-RPC suite against it. The suite
requires Node.js 24 or newer and Foundry/cast v1.7.1; it installs the lockfile's
exact viem v2.55.13 dependency with lifecycle scripts disabled. viem typed
actions and cast typed/raw calls collectively exercise every method marked
implemented in the locked execution API subset, plus HTTP batching, WebSocket
`newHeads`, EIP-712, EIP-7702, common local-node controls, and canonical RPC
errors. CI runs this as a separate blocking Ubuntu job.

Upstream protocol inputs are pinned in `spec.lock`; the consumed minimal/Fulu
constants and API surface contracts are vendored under `specs/upstream`.

## Licensing

Original code is MIT licensed. go-ethereum is LGPL-3.0; see
`THIRD_PARTY_NOTICES.md`. A static binary release remains gated on a formal LGPL
distribution review and reproducible corresponding-source materials.
