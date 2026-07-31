# Locked protocol vectors

This directory contains a deliberately small, offline subset used by the v0.1
regression suite. Source revisions and local SHA-256 digests are recorded in
`spec.lock`.

- `c-kzg-4844-v2.1.5` is the upstream `correct_proof_0_0` KZG proof vector,
  retained byte-for-byte as YAML.
- `consensus-spec-tests-v1.6.0-beta.0` carries the official phase0
  `BeaconBlockHeader/ssz_random/case_0` values and Snappy payload in a JSON
  carrier, together with the three upstream Git blob IDs.
- `execution-spec-tests-eip4788` locks the official EIP-4788 wraparound and
  overwrite timestamp parameter sequences exercised against ethertest blocks.

These selected vectors do not represent the complete upstream suites. Full
consensus state-transition vectors and standard client import remain outside
the synthetic v0.1 scope and are not claimed as passing.
