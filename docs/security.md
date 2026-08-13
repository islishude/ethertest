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

`ethertest_importAccount` accepts a raw private key and therefore is only a
development convenience. On plaintext HTTP the key is protected only by the
loopback listener boundary; use the Go API or user-provided TLS when transport
exposure matters. The request body, typed-data payloads, signatures, balances,
and imported keys are never logged or persisted. Imported keys remain unlocked
in process memory until removed or shutdown and are not encrypted.

`ethertest_signAuthorization` can create reusable EIP-7702 delegation
signatures for any configured or runtime-imported account. Treat access to it as
access to the unlocked signer. The method accepts only the node chain ID or an
explicit chain ID of zero; zero authorizations are replayable across chains and
should be used only when that behavior is intentional. Authorization requests
and signatures are not logged, persisted, or included in revision events.

Configured accounts are protected from runtime removal. Removing an imported
account deletes only the in-memory signing capability; it does not erase the
corresponding execution state or history. Importing with a balance creates an
unsafe control block and permanently taints the session/archive exactly like a
direct balance override.
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
