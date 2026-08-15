# Architecture

ethertest is a single-process Ethereum execution and synthetic consensus
development node. A `Node` owns one isolated chain. The CLI runs one node per
process; Go callers may create multiple nodes.

All public state changes pass through one ordered controller. Execution queries
are anchored to immutable committed headers; block, retained blob sidecar,
Beacon projection, and revision event publication are produced by that
controller. The public Go API intentionally does not expose mutable geth state.

A Node-owned, concurrency-safe in-memory wallet is the sole owner of signing
keys. For the generated genesis, the execution chain receives configured
addresses for allocation; an imported execution genesis instead preserves its
exact alloc and does not fund or otherwise merge wallet addresses. In both
modes the addresses remain available for fee-recipient defaults and synthetic
validator setup, and the chain never retains their private keys. Wallet mutation
passes through the same ordered controller while address enumeration and signing
use the wallet's read lock.

Memory and Pebble use one `ethdb.Database` with separate prefixes for blob,
control, execution-request queue/records, checkpoint, branch, projection,
safety, slot, and event data; geth owns its execution keys. A prepared-operation
journal brackets execution block/head mutation and one auxiliary batch. Startup
cancels an intent when geth still has the old head, completes it when geth
reached the target, and fails closed when neither state matches. Events, request
queue changes and consumed IDs, Beacon projections, and slot/safety metadata
share the auxiliary batch.

The alpha metadata layout is updated in place. There is no v2 schema or data
migration path: a nonempty database without the current metadata marker is
rejected with an instruction to create a fresh chain.
For a new generated chain, `genesis_time = 0` is resolved once; subsequent
Pebble starts load that value from the timeline before constructing geth's
genesis block. An explicit conflicting value fails closed.
External execution genesis files retain their timestamp, including zero. New
imports persist an external-genesis marker and hash in the same timeline. On a
later start without the source file, geth's stored genesis header, alloc, and
ChainConfig are reconstructed before the synthetic consensus model is created;
a re-supplied file must match both the stored hash and ChainConfig. Archives
carry this metadata because they contain the complete shared database.

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

Synthetic finality can be paused independently of block and slot progression.
The timeline persists the frozen observer slot, and every EL tag, Beacon
checkpoint, finalized response flag, finalized-checkpoint event, and branch
finality guard resolves through that same slot. Resuming catches up directly to
the current slot and publishes at most one latest finalized-checkpoint event.
Canonical rewinds preserve the paused mode and clamp a future frozen slot to the
new head so finality never resolves beyond it. Because this state is stored in
the shared database, Pebble restarts and state archives retain it.

Every execution child uses the persisted root of its parent synthetic Beacon
block as `parent_beacon_block_root`. Missed slots retain the most recent actual
Beacon block root. JSON and SSZ are loaded from the same persisted projection.

The Beacon projection changes from the Deneb block container to the
Electra/Fulu container at the configured Prague epoch. Fulu data-column
sidecars use the narrow local SSZ implementation; the block container remains
the Electra shape as specified for this subset. Validator balances remain
static, but `execution_requests` are populated from the same typed bytes used by
the EL `requestsHash`.

After all transactions have been added, block generation asks geth for its
native EIP-6110/7002/7251 output. Unknown types, noncanonical type ordering,
bad fixed lengths, protocol-limit overflow, or a hash mismatch reject the
candidate. Native request bytes, the complete request bytes, and any consumed
synthetic control IDs are persisted per execution block. Missing legacy records
are derived from the stored Beacon projection only when block re-execution
confirms the bytes are native; an unrecoverable nonempty history fails closed.

Three Node-owned persistent FIFOs provide synthetic deposit, withdrawal, and
consolidation controls. Each entry has a database-monotonic ID. Native entries
occupy each type's capacity first and controls append in FIFO order. Branch
mining records only its geth-native output and never consumes the control queue.
Canonical switching restores controls from the removed path, removes IDs
consumed by the added path, and sorts the result by ID. Geth's native
withdrawal/consolidation contract queues instead follow the selected StateDB;
deposit requests remain attached to the block containing their log.

A block containing a synthetic request control is resealed with the combined
`requestsHash` and canonicalized through the recovery journal rather than geth's
normal request validation, because no corresponding execution transition
exists. It receives permanent `execution-request-control` taint. Native-only
blocks remain replayable and untainted.

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
