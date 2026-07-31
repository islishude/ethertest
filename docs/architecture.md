# Architecture

ethertest is a single-process Ethereum execution and synthetic consensus
development node. A `Node` owns one isolated chain. The CLI runs one node per
process; Go callers may create multiple nodes.

All public state changes pass through one ordered controller. Execution queries
are anchored to immutable committed headers; block, retained blob sidecar,
Beacon projection, and revision event publication are produced by that
controller. The public Go API intentionally does not expose mutable geth state.

Memory and Pebble use one `ethdb.Database` with separate prefixes for blob,
control, checkpoint, branch, and event data; geth owns its execution keys.
Controller ordering gives a consistent live revision. A future release gate is
to add a recovery journal so a process crash cannot expose a geth block without
the corresponding auxiliary revision.

Consensus finality is synthetic. Blocks, SSZ roots, proposer signatures, KZG
proofs, and cross-layer references are intended to be cryptographically real,
but ethertest is not a validator client and does not claim that its Beacon
state can be replayed through the complete Ethereum consensus transition.

The Beacon projection changes from the Deneb block container to the
Electra/Fulu container at the configured Prague epoch. Fulu data-column
sidecars use the narrow local SSZ implementation; the block container remains
the Electra shape as specified for this subset. Validator balances are currently
static and execution request/withdrawal queues are empty pending their v0.1
state-machine gate.

## Explicit non-goals for v0.1

- remote state forking;
- execution or consensus P2P;
- Engine API;
- GraphQL and IPC;
- JavaScript tracers;
- external validator clients;
- production deployment or large-scale indexing.
