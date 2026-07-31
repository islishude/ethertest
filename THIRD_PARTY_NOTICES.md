# Third-party notices

`ethertest` is licensed under the MIT License. It links to third-party
libraries under their own licenses.

In particular, the go-ethereum library is licensed under the GNU Lesser
General Public License v3.0. `rpc_transactions.go`, `rpc_simulate.go`, and
`rpc_fee_config.go` contain adapted go-ethereum logic and retain file-level
copyright and LGPL notices. Binary and container releases must not be
published until the release workflow includes the corresponding source,
license notices, reproducible build material, and the project's LGPL
distribution review has passed.

The complete dependency inventory is generated as an SBOM by the release
workflow. Exact dependency versions are recorded in `go.mod`, `go.sum`, and
`spec.lock`.
