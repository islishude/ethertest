# Independent Gloas SSZ vectors

`vectors.json` was generated using Ethereum's Python `ssz-specs` reference
implementation at `608fa1b7d056021c66210e56454475b176c95914`, with Python 3.14
and pydantic 2.13.5. Container definitions follow consensus-specs
v1.7.0-beta.2 (`5afdff62889b1a13a5be804c8be8763abf1c8654`). The six vectors
cover execution requests (all five types), payload, bid, signed envelope,
data column, and signed Beacon block including progressive body.

To regenerate, check out that exact ssz-specs revision, create a Python >=3.12
virtual environment, install `pydantic==2.13.5` and the checkout with pip, then
run `python testdata/vectors/gloas/generate.py`. Update the corresponding digests
in `spec.lock` only after checking the resulting diff. Normal Go builds/tests
read the checked-in file offline; they never invoke this generator.

These vectors compare the complete encoding and tree root against an independent
codec. They are a protocol subset, not the upstream state-transition suite.
Like the existing ethertest block containers, the block fixture retains a
512-bit sync aggregate; PTC size is the ethertest minimal value of 16. Signatures
in these serialization vectors are fixed bytes; separate node tests validate
actual deterministic BLS signatures and cross-object references.
