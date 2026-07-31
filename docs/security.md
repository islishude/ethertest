# Security

ethertest is a local testing tool, not a production node.

The default mnemonic and private keys are public. CORS is permissive and local
RPC methods are unauthenticated. Never use funded production keys and never
expose the default listener to an untrusted network. Binding a non-loopback
address requires the explicit `--allow-unsafe-external` flag.

Human startup output includes every unlocked private key. Structured logs,
metrics, errors, state manifests, and default state archives must never contain
private keys. Secret export requires a separate explicit command.

KZG verification is never bypassed. Header-invalid development blocks are not
implemented in the current alpha; the planned unsafe-session API must
permanently taint any affected branch before that capability can be enabled.
