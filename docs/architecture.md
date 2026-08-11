# Architecture

ethertest is a single-process Ethereum execution and synthetic consensus
development node. A `Node` owns one isolated chain. The CLI runs one node per
process; Go callers may create multiple nodes.

All public state changes pass through one ordered controller. Execution queries
are anchored to immutable committed headers; block, retained blob sidecar,
Beacon projection, and revision event publication are produced by that
controller. The public Go API intentionally does not expose mutable geth state.

A Node-owned, concurrency-safe in-memory wallet is the sole owner of signing
keys. The execution chain receives configured addresses for genesis allocation,
fee-recipient defaults, and synthetic validator setup, but never retains their
private keys. Wallet mutation passes through the same ordered controller while
address enumeration and signing use the wallet's read lock.

Memory and Pebble use one `ethdb.Database` with separate prefixes for blob,
control, checkpoint, branch, projection, safety, slot, and event data; geth owns
its execution keys. A prepared-operation journal brackets execution block/head
mutation and one auxiliary batch. Startup cancels an intent when geth still has
the old head, completes it when geth reached the target, and fails closed when
neither state matches. Events and their slot/index metadata share the auxiliary
batch.

The alpha metadata layout is updated in place. There is no v2 schema or data
migration path: a nonempty database without the current metadata marker is
rejected with an instruction to create a fresh chain.
For a new chain, `genesis_time = 0` is resolved once; subsequent Pebble starts
load that value from the timeline before constructing geth's genesis block.
An explicit conflicting value fails closed.

Canonical slot lookup and per-execution-hash branch slots are separate. Missed
slots, checkpoint slots, synthetic Beacon projections, and lineage safety are
persisted. The session taint bit is monotonic even when a reorg returns the head
to a clean branch.

The transaction pool is local and deterministic rather than geth's network
pool. A single-writer rebuilds an immutable candidate block, post-execution
state, receipts, and executable/queued classification after head or pool
changes. All pending state queries resolve through that candidate.

Runtime wallet membership is ephemeral control-plane state. It is deliberately
independent of snapshots, checkpoints, branches, databases, and archives.
Optional import funding is execution state: the control block is committed
before the signer is published and follows normal persistence and taint rules.
Consequently, a crash after that commit can leave a funded address without its
runtime signer, which is also the defined state after every restart.

Consensus finality is synthetic. Blocks, SSZ roots, proposer signatures, KZG
proofs, and cross-layer references are intended to be cryptographically real,
but ethertest is not a validator client and does not claim that its Beacon
state can be replayed through the complete Ethereum consensus transition.
The state root, proposer, sync aggregate, and finality resolver are deterministic
synthetic helpers rather than canonical BeaconState results.

Every execution child uses the persisted root of its parent synthetic Beacon
block as `parent_beacon_block_root`. Missed slots retain the most recent actual
Beacon block root. JSON and SSZ are loaded from the same persisted projection.

The Beacon projection changes from the Deneb block container to the
Electra/Fulu container at the configured Prague epoch. Fulu data-column
sidecars use the narrow local SSZ implementation; the block container remains
the Electra shape as specified for this subset. Validator balances are currently
static and execution request/withdrawal queues are empty pending their v0.1
state-machine gate.

The network advertises `consensusMode: synthetic`, `beaconApi: v4-subset`,
`fullConsensus: false`, and `releaseComplete: false`.

## Explicit non-goals for v0.1

- remote state forking;
- execution or consensus P2P;
- Engine API;
- GraphQL and IPC;
- JavaScript tracers;
- external validator clients;
- production deployment or large-scale indexing.
