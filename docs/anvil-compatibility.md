# Anvil development workflow compatibility

The reference is Anvil **v1.8.3**, commit
`cae51ad458f6abb64852b7709eb784352429825d`. This is a selected local workflow
subset, not a drop-in Anvil replacement or a release-completeness claim.
The offline [matrix](../specs/upstream/anvil-compatibility.json) inventories all
177 primary `EthRequest` methods and their upstream aliases, actual local
entrypoints, the selected contracts, subscriptions, CLI flags, and evidence.
It is digest-locked in `spec.lock`; a same-name entry alone is not certification.

## Contracts and migration

| Call | Result | Behavior and limits |
| --- | --- | --- |
| `anvil_setBalance`, `anvil_setCode`, `anvil_setNonce` | `null` | Still mines one permanently tainted control block |
| `anvil_setStorageAt` | `true` | Accepts short quantity slots; value must be 32 bytes; still mines a control block |
| `anvil_mine(count?, interval?)` | `null` | Count defaults to 1; omitted interval uses slot duration, explicit interval must equal it |
| `evm_mine(timestampOrOptions?)` | `"0x0"` | Optional `{timestamp, blocks}` with required timestamp; timestamp must equal the next slot timestamp |
| `evm_snapshot`, `evm_revert` | hex ID / boolean | One-shot chain snapshots; finalized-history protection remains |
| `anvil_getAutomine`, `anvil_setAutomine` | boolean / `null` | Transaction mining; enabling also drains already executable pool entries |
| `anvil_getIntervalMining`, `anvil_setIntervalMining` | seconds or `null` / `null` | Positive seconds select interval mode including empty blocks; zero selects manual |
| `anvil_dropTransaction` | hash or `null` | Removes only a pooled transaction |
| `anvil_dropAllTransactions`, `anvil_removePoolTransactions` | `null` | Clear all or one sender's pool entries, including queued entries |
| `anvil_setCoinbase` | `null` | Updates runtime beneficiary and the pending view without mining |
| `anvil_getGenesisTime` | integer seconds | Resolved, including persisted or imported, genesis time |
| `txpool_inspect` | pending/queued maps | Uses the same candidate classification as other txpool methods; sender keys remain lowercase, while Anvil v1.8.3 uses checksummed keys |

`evm_setAutomine` and `evm_setIntervalMining` are also registered. Disabling
automine while interval mining is active leaves interval mining active.
Enabling automine selects transaction mode; interval mining is then disabled.
Intervals control wall-clock scheduling only: **execution timestamps always
advance by the configured slot duration**. Runtime mining settings and fee
recipient changes are not restored by snapshots or archives; restart uses the
configuration. No storage schema migration is required.

Unsigned integer parsing is method-specific. Setters, mining counts, and
`evm_mine` timestamps accept JSON uint64 integers or uint256 numeric strings
without floating-point conversion (timestamps/counts/nonces must fit uint64).
Like the pinned reference, strings also accept decimal, hexadecimal, octal,
binary, digit separators, and empty zero encodings. `setStorageAt` values and block-override
`prevRandao` are fixed 32-byte data. Negative/overflowing values, unsupported
explicit mining timestamps/intervals, and excessive counts are rejected before
writing. Multi-block mining and enabling automine can commit earlier blocks
before a later execution/cancellation failure, just as `Node.Mine` does; these
operations are not all-or-nothing transactions.

Scripts that relied on the old aliases should migrate as follows:

| Previous expectation | Replacement |
| --- | --- |
| `anvil_setBalance/setCode/setNonce/setStorageAt` returns a block hash | Use the corresponding `ethertest_*` control |
| `evm_mine("0x2")` mines two blocks and returns hashes | Use `ethertest_mine("0x2")` |
| Anvil-style mining success result | Use `anvil_mine` or `evm_mine` with the contracts above |
| Branch, missed-slot, safety, or other ethertest-specific controls under `anvil_*` / `evm_*` | Existing aliases remain deprecated; use `ethertest_*` |

Only explicitly listed legacy aliases are preserved. New `ethertest_*` methods
are not automatically exported under `anvil`/`evm`. No `hardhat_*` or
`tenderly_*` namespace is introduced. `ethertest_capabilities` advertises the
reference version and the control-block, slot-time, and synthetic-finality
limitations.

## Read-only calls and subscriptions

`eth_call` and `eth_estimateGas` accept a fourth, optional block-overrides
object: `number`, `time`, `gasLimit`, `feeRecipient`, `prevRandao`,
`baseFeePerGas`, and `blobBaseFee`. Unknown fields are rejected. Every estimate
trial uses the same overrides; neither method changes canonical state, pending
state, Beacon projections, revisions, or taint. Existing gas, timeout, and
response limits apply. Like pinned Anvil v1.8.3, `blobBaseFee` overrides the
value returned by `BLOBBASEFEE`.

HTTP remains request/response; WS, IPC, and in-process subscriptions support
`newHeads`, `logs`, and `newPendingTransactions`. The latter accepts an optional
boolean selecting complete transactions instead of hashes. Pending inclusion
fields remain `null`, even if the accepted transaction is mined or dropped
before notification delivery.

Canonical rewinds publish removed logs before replacement logs, including
snapshot revert. **Anvil v1.8.3 `evm_revert` does not emit removed logs**; this
is an intentional difference required by ethertest's canonical event contract.
Live notification queues and replay history are bounded by `events.capacity`;
`limits.max_subscriptions` and response-size limits also apply. Gaps, overflow,
or oversized notifications close the affected connection and log the reason.
Other connections continue operating. Consumers must reconnect, resubscribe,
and recover missed history using queries. Live subscriptions and pending event
history are not restored on restart.

## CLI subset

The added flags map onto existing configuration, with precedence
**defaults → TOML → `ETHERTEST_*` → CLI**:

- `--host`, `--port`: shared HTTP/WS/Beacon listener; conflict with explicit
  `--http`. Non-loopback still requires `--allow-unsafe-external`.
- `--accounts`, `--balance`, `--mnemonic`: development accounts; balance is
  whole ETH. Imported genesis remains authoritative for allocations.
- `--no-mining`, `--block-time`: manual or periodic mining, mutually exclusive.
  `--block-time` accepts positive seconds (including fractions), mines empty
  blocks, and never changes slot duration.
- `--order`, `--gas-limit`, `--coinbase`: existing transaction ordering,
  generated-genesis gas limit, and runtime fee recipient configuration.
- `--quiet`: suppresses runtime logs and the development-account banner.

Defaults remain chain ID `1337`, six-second slots, and Amsterdam/Gloas at
genesis. State archives remain `ethertest-state-v3`, and IPC retains its existing
explicit path syntax. Anvil archive import/export, `--hardfork`, mixed mining,
free timestamps, no-block setters, impersonation, `trace_*`/`ots_*`, reset/reorg
RPCs, remote forks, and other network modes are not added in this round.

## Validation

```sh
go test ./...
go test -race ./...
go test -run '^TestSecp256k1UsesCGOBackend$' .
golangci-lint run
go run ./internal/beaconcontractgen -check
make test-rpc-e2e
make test-anvil-compat
```

Normal Go tests use offline fixtures and require no Anvil executable. The
separate differential target requires Anvil v1.8.3 at the pinned commit and
cast/viem, and fails rather than skips if the reference is absent. Override its
paths with `ANVIL`, `CAST`, and `RPC_E2E_BINARY` as needed. It starts disposable
loopback nodes with matched accounts, chain ID, genesis time, and Osaka rules;
it asserts the named differences instead of ignoring arbitrary mismatches.
Amsterdam/Gloas control-block boundaries are covered by local Go regressions.
Existing release gates and external official-suite requirements remain open.
