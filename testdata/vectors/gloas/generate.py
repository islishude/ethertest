"""Regenerate with Python >=3.12 and pydantic 2.13.5.

pip install /path/to/pinned/ethereum/ssz-specs
python generate.py
Reference codec: ethereum/ssz-specs@608fa1b7d056021c66210e56454475b176c95914.
Containers: consensus-specs@5afdff62889b1a13a5be804c8be8763abf1c8654.
This generator uses no ethertest or dynamic-ssz code. Normal Go tests read the
checked-in vectors without Python, network access, or external dependencies.
"""
import json
from pathlib import Path
from ssz import Container, ProgressiveContainer, ProgressiveList, ProgressiveByteList
from ssz import ByteVector, ByteList, Uint64, Uint256, active_fields, hash_tree_root


def fixed(n):
    return type(f"Bytes{n}", (ByteVector,), {"LENGTH": n})


def container(name, fields, progressive=False):
    namespace = {"__annotations__": fields, "__module__": __name__}
    if progressive:
        namespace["ACTIVE_FIELDS"] = active_fields(len(fields))
    return type(name, (ProgressiveContainer if progressive else Container,), namespace)


def plist(t):
    return type(f"{t.__name__}List", (ProgressiveList[t],), {})


B20, B32, B48, B96, B256 = (fixed(n) for n in (20, 32, 48, 96, 256))
Extra = type("Extra", (ByteList,), {"LIMIT": 32})
Deposit = container("Deposit", dict(pubkey=B48, withdrawal_credentials=B32, amount=Uint64, signature=B96, index=Uint64))
WithdrawalRequest = container("WithdrawalRequest", dict(source_address=B20, validator_pubkey=B48, amount=Uint64))
Consolidation = container("Consolidation", dict(source_address=B20, source_pubkey=B48, target_pubkey=B48))
BuilderDeposit = container("BuilderDeposit", dict(pubkey=B48, withdrawal_credentials=B32, amount=Uint64, signature=B96))
BuilderExit = container("BuilderExit", dict(source_address=B20, pubkey=B48))
Requests = container("Requests", dict(deposits=plist(Deposit), withdrawals=plist(WithdrawalRequest), consolidations=plist(Consolidation), builder_deposits=plist(BuilderDeposit), builder_exits=plist(BuilderExit)), True)
Withdrawal = container("Withdrawal", dict(index=Uint64, validator_index=Uint64, address=B20, amount=Uint64))
Payload = container("Payload", dict(parent_hash=B32, fee_recipient=B20, state_root=B32, receipts_root=B32, logs_bloom=B256, prev_randao=B32, block_number=Uint64, gas_limit=Uint64, gas_used=Uint64, timestamp=Uint64, extra_data=Extra, base_fee_per_gas=Uint256, block_hash=B32, transactions=plist(ProgressiveByteList), withdrawals=plist(Withdrawal), blob_gas_used=Uint64, excess_blob_gas=Uint64, block_access_list=ProgressiveByteList, slot_number=Uint64), True)
Bid = container("Bid", dict(parent_block_hash=B32, parent_block_root=B32, block_hash=B32, prev_randao=B32, fee_recipient=B20, gas_limit=Uint64, builder_index=Uint64, slot=Uint64, value=Uint64, execution_payment=Uint64, blob_kzg_commitments=plist(B48), execution_requests_root=B32), True)
Envelope = container("Envelope", dict(payload=Payload, execution_requests=Requests, builder_index=Uint64, beacon_block_root=B32, parent_beacon_block_root=B32), True)
SignedEnvelope = container("SignedEnvelope", dict(message=Envelope, signature=B96))
Column = container("Column", dict(index=Uint64, column=plist(fixed(2048)), kzg_proofs=plist(B48), slot=Uint64, beacon_block_root=B32))


# Full block/body schema. The block vector intentionally uses empty consensus
# operation lists, as ethertest's deterministic projection does.
Header = container("Header", dict(slot=Uint64, proposer_index=Uint64, parent_root=B32, state_root=B32, body_root=B32))
SignedHeader = container("SignedHeader", dict(message=Header, signature=B96))
ProposerSlashing = container("ProposerSlashing", dict(signed_header_1=SignedHeader, signed_header_2=SignedHeader))
Checkpoint = container("Checkpoint", dict(epoch=Uint64, root=B32))
AttestationData = container("AttestationData", dict(slot=Uint64, index=Uint64, beacon_block_root=B32, source=Checkpoint, target=Checkpoint))
IndexedAttestation = container("IndexedAttestation", dict(attesting_indices=plist(Uint64), data=AttestationData, signature=B96), True)
AttesterSlashing = container("AttesterSlashing", dict(attestation_1=IndexedAttestation, attestation_2=IndexedAttestation))
from ssz import ProgressiveBitList, Vector
Attestation = container("Attestation", dict(aggregation_bits=ProgressiveBitList, data=AttestationData, signature=B96, committee_bits=fixed(8)), True)
DepositData = container("DepositData", dict(pubkey=B48, withdrawal_credentials=B32, amount=Uint64, signature=B96))
Proof = type("Proof", (Vector[B32],), {"LENGTH": 33})
DepositOperation = container("DepositOperation", dict(proof=Proof, data=DepositData))
Exit = container("Exit", dict(epoch=Uint64, validator_index=Uint64))
SignedExit = container("SignedExit", dict(message=Exit, signature=B96))
BLSChange = container("BLSChange", dict(validator_index=Uint64, from_bls_pubkey=B48, to_execution_address=B20))
SignedBLSChange = container("SignedBLSChange", dict(message=BLSChange, signature=B96))
Eth1Data = container("Eth1Data", dict(deposit_root=B32, deposit_count=Uint64, block_hash=B32))
SyncAggregate = container("SyncAggregate", dict(sync_committee_bits=fixed(64), sync_committee_signature=B96))
SignedBid = container("SignedBid", dict(message=Bid, signature=B96))
PayloadAttestationData = container("PayloadAttestationData", dict(beacon_block_root=B32, slot=Uint64, payload_present=__import__('ssz').Boolean, blob_data_available=__import__('ssz').Boolean))
PayloadAttestation = container("PayloadAttestation", dict(aggregation_bits=fixed(2), data=PayloadAttestationData, signature=B96), True)
Body = container("Body", dict(randao_reveal=B96, eth1_data=Eth1Data, graffiti=B32, proposer_slashings=plist(ProposerSlashing), attester_slashings=plist(AttesterSlashing), attestations=plist(Attestation), deposits=plist(DepositOperation), voluntary_exits=plist(SignedExit), sync_aggregate=SyncAggregate, bls_to_execution_changes=plist(SignedBLSChange), signed_execution_payload_bid=SignedBid, payload_attestations=plist(PayloadAttestation), parent_execution_requests=Requests), True)
Block = container("Block", dict(slot=Uint64, proposer_index=Uint64, parent_root=B32, state_root=B32, body=Body))
SignedBlock = container("SignedBlock", dict(message=Block, signature=B96))


def hx(n, byte=0):
    return "0x" + bytes([byte] * n).hex()


requests = dict(deposits=[dict(pubkey=hx(48, 1), withdrawal_credentials=hx(32, 2), amount="3", signature=hx(96, 4), index="5")], withdrawals=[dict(source_address=hx(20, 6), validator_pubkey=hx(48, 7), amount="8")], consolidations=[dict(source_address=hx(20, 9), source_pubkey=hx(48, 10), target_pubkey=hx(48, 11))], builder_deposits=[dict(pubkey=hx(48, 12), withdrawal_credentials=hx(32, 13), amount="14", signature=hx(96, 15))], builder_exits=[dict(source_address=hx(20, 16), pubkey=hx(48, 17))])
payload = dict(parent_hash=hx(32, 1), fee_recipient=hx(20, 2), state_root=hx(32, 3), receipts_root=hx(32, 4), logs_bloom=hx(256, 5), prev_randao=hx(32, 6), block_number="7", gas_limit="30000000", gas_used="15000", timestamp="1800000060", extra_data="0x010203", base_fee_per_gas="1000000000", block_hash=hx(32, 8), transactions=["0x010203", "0x0405060708"], withdrawals=[dict(index="9", validator_index="10", address=hx(20, 11), amount="12")], blob_gas_used="131072", excess_blob_gas="0", block_access_list="0xc0", slot_number="10")
bid = dict(parent_block_hash=hx(32, 1), parent_block_root=hx(32, 2), block_hash=hx(32, 3), prev_randao=hx(32, 4), fee_recipient=hx(20, 5), gas_limit="30000000", builder_index=str(2**64 - 1), slot="10", value="0", execution_payment="0", blob_kzg_commitments=[hx(48, 6), hx(48, 7)], execution_requests_root="0x" + hash_tree_root(Requests.model_validate(requests)).hex())
envelope = dict(message=dict(payload=payload, execution_requests=requests, builder_index=str(2**64 - 1), beacon_block_root=hx(32, 9), parent_beacon_block_root=hx(32, 10)), signature=hx(96, 11))
column = dict(index="3", column=[hx(2048, 1)], kzg_proofs=[hx(48, 2)], slot="10", beacon_block_root=hx(32, 3))
body = dict(randao_reveal=hx(96, 12), eth1_data=dict(deposit_root=hx(32), deposit_count="0", block_hash=hx(32)), graffiti=hx(32, 13), proposer_slashings=[], attester_slashings=[], attestations=[], deposits=[], voluntary_exits=[], sync_aggregate=dict(sync_committee_bits=hx(64), sync_committee_signature=hx(96)), bls_to_execution_changes=[], signed_execution_payload_bid=dict(message=bid, signature="0xc0" + "00" * 95), payload_attestations=[], parent_execution_requests=requests)
block = dict(message=dict(slot="10", proposer_index="2", parent_root=hx(32, 14), state_root=hx(32, 15), body=body), signature=hx(96, 16))
vectors = []
for name, typ, data in [("requests", Requests, requests), ("payload", Payload, payload), ("bid", Bid, bid), ("envelope", SignedEnvelope, envelope), ("column", Column, column), ("block", SignedBlock, block)]:
    value = typ.model_validate(data)
    vectors.append(dict(name=name, data=data, ssz="0x" + value.encode_bytes().hex(), root="0x" + hash_tree_root(value).hex()))
Path(__file__).with_name("vectors.json").write_text(json.dumps(vectors, indent=2) + "\n")
