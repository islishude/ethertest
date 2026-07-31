# Security

ethertest is a local testing tool, not a production node.

The default mnemonic and private keys are public. CORS is permissive and local
RPC methods are unauthenticated. Never use funded production keys and never
expose the default listener to an untrusted network. Binding a non-loopback
address requires the explicit `--allow-unsafe-external` flag.

Human startup output on stderr includes every unlocked private key. Structured
logs on stdout, metrics, errors, state manifests, and default state archives
must never contain private keys, mnemonics, raw transaction data, or
control-state values. JSON logging suppresses the human startup key banner.
Secret export requires a separate explicit command.

KZG verification is never bypassed. State-control RPC methods create an unsafe
fixture block that is not a standard Ethereum state transition. The block and
all descendants inherit lineage taint, branches and checkpoints retain it, and
the database session/archive remains permanently tainted after returning to a
clean head. Query `ethertest_safetyStatus` or `Node.SafetyStatus` before using an
archive as replay input. `VerifyControlRecord` checks only ethertest's record,
digest, parent, and custom state root; it is not Ethereum transition validation.

Header-invalid mutation controls are not implemented. The Beacon REST surface
is a synthetic v4 subset and must not be exposed or described as a full
consensus client, validator client, finality oracle, or standard CL import path.
