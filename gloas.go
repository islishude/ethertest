package ethertest

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// blockAccessListBytes obtains the exact committed RLP, including the empty
// genesis BAL. Published Amsterdam blocks must never silently lose their BAL.
func blockAccessListBytes(chain *executionChain, block *types.Block) ([]byte, error) {
	if block.Header().BlockAccessListHash == nil {
		return nil, nil
	}
	list := block.AccessList()
	if list == nil {
		list = rawdb.ReadAccessList(chain.db, block.Hash(), block.NumberU64())
	}
	var encoded []byte
	var err error
	if list == nil {
		if block.NumberU64() != 0 {
			return nil, fmt.Errorf("block %s is missing its block access list", block.Hash())
		}
		encoded = []byte{0xc0}
	} else {
		encoded, err = rlp.EncodeToBytes(list)
		if err != nil {
			return nil, err
		}
	}
	if typesHashBAL(encoded) != *block.Header().BlockAccessListHash {
		return nil, errors.New("block access list hash mismatch")
	}
	return encoded, nil
}

func (m *consensusModel) buildGloas(chain *executionChain, block *types.Block, body *electra.BeaconBlockBody, requests *executionRequests, parentRoot, stateRoot phase0.Root, proposer phase0.ValidatorIndex) (*consensusBlock, error) {
	slot := chain.slotOf(block)
	if block.Header().SlotNumber == nil || *block.Header().SlotNumber != slot {
		return nil, errors.New("amsterdam slotNumber disagrees with the Beacon timeline")
	}
	requests = cloneElectraExecutionRequests(requests)
	parentRequests := cloneElectraExecutionRequests(nil)
	if block.NumberU64() > 0 {
		parent := chain.blockchain.GetBlockByHash(block.ParentHash())
		if parent == nil {
			return nil, errors.New("gloas execution parent is missing")
		}
		// At the Fulu transition the synthetic latest bid has an empty request root.
		if chain.config.IsAmsterdam(parent.Number(), parent.Time()) && parent.NumberU64() > 0 {
			record, exists, err := loadExecutionRequestRecord(chain, parent.Hash())
			if err != nil {
				return nil, err
			}
			if !exists {
				return nil, errors.New("gloas parent requests are missing")
			}
			parentRequests, err = parseExecutionRequests(record.Requests)
			if err != nil {
				return nil, err
			}
		}
	}
	requestRoot, err := gloasSSZ.HashTreeRoot(requests)
	if err != nil {
		return nil, err
	}
	infinity := phase0.BLSSignature{0xc0}
	bid := &gloasSignedPayloadBid{Message: &gloasPayloadBid{
		ParentBlockHash: phase0.Hash32(block.ParentHash()), ParentBlockRoot: parentRoot, BlockHash: phase0.Hash32(block.Hash()), PrevRandao: phase0.Root(block.MixDigest()), FeeRecipient: body.ExecutionPayload.FeeRecipient, GasLimit: block.GasLimit(), BuilderIndex: math.MaxUint64, Slot: phase0.Slot(slot), BlobKZGCommitments: body.BlobKZGCommitments, ExecutionRequestsRoot: requestRoot,
	}, Signature: infinity}
	message := &gloasBeaconBlock{Slot: phase0.Slot(slot), ProposerIndex: proposer, ParentRoot: parentRoot, StateRoot: stateRoot, Body: &gloasBeaconBody{
		RANDAOReveal: body.RANDAOReveal, ETH1Data: body.ETH1Data, Graffiti: body.Graffiti, ProposerSlashings: body.ProposerSlashings, AttesterSlashings: []*gloasAttesterSlashing{}, Attestations: []*gloasAttestation{}, Deposits: body.Deposits, VoluntaryExits: body.VoluntaryExits, SyncAggregate: body.SyncAggregate, BLSToExecutionChanges: body.BLSToExecutionChanges, SignedExecutionPayloadBid: bid, PayloadAttestations: []*gloasPayloadAttestation{}, ParentExecutionRequests: parentRequests,
	}}
	root, err := gloasSSZ.HashTreeRoot(message)
	if err != nil {
		return nil, err
	}
	signature, err := m.sign(root, phase0.DomainType{}, uint64(proposer), slot)
	if err != nil {
		return nil, err
	}
	payload, err := gloasPayload(chain, block, body.ExecutionPayload)
	if err != nil {
		return nil, err
	}
	envelope := &gloasPayloadEnvelope{Payload: payload, ExecutionRequests: requests, BuilderIndex: math.MaxUint64, BeaconBlockRoot: root, ParentBeaconBlockRoot: parentRoot}
	envelopeRoot, err := gloasSSZ.HashTreeRoot(envelope)
	if err != nil {
		return nil, err
	}
	envelopeSignature, err := m.sign(envelopeRoot, phase0.DomainType{0x0b}, uint64(proposer), slot)
	if err != nil {
		return nil, err
	}
	return &consensusBlock{gloas: &gloasSignedBeaconBlock{Message: message, Signature: signature}, envelope: &gloasSignedPayloadEnvelope{Message: envelope, Signature: envelopeSignature}}, nil
}

func gloasPayload(chain *executionChain, block *types.Block, p *deneb.ExecutionPayload) (*gloasExecutionPayload, error) {
	bal, err := blockAccessListBytes(chain, block)
	if err != nil {
		return nil, err
	}
	transactions := make([]hexutil.Bytes, len(p.Transactions))
	for i, tx := range p.Transactions {
		transactions[i] = bytes.Clone(tx)
	}
	return &gloasExecutionPayload{ParentHash: p.ParentHash, FeeRecipient: p.FeeRecipient, StateRoot: p.StateRoot, ReceiptsRoot: p.ReceiptsRoot, LogsBloom: gloasLogsBloom(p.LogsBloom), PrevRandao: p.PrevRandao, BlockNumber: p.BlockNumber, GasLimit: p.GasLimit, GasUsed: p.GasUsed, Timestamp: p.Timestamp, ExtraData: p.ExtraData, BaseFeePerGas: p.BaseFeePerGas, BlockHash: p.BlockHash, Transactions: transactions, Withdrawals: p.Withdrawals, BlobGasUsed: p.BlobGasUsed, ExcessBlobGas: p.ExcessBlobGas, BlockAccessList: bal, SlotNumber: *block.Header().SlotNumber}, nil
}

func (m *consensusModel) validateGloas(chain *executionChain, block *types.Block, projection *consensusBlock, body *electra.BeaconBlockBody, requests *executionRequests, stateRoot phase0.Root, proposer phase0.ValidatorIndex) error {
	if err := projection.validateGloasStructure(); err != nil {
		return err
	}
	expected, err := m.buildGloas(chain, block, body, requests, projection.parentRoot(), stateRoot, proposer)
	if err != nil {
		return err
	}
	a, err := gloasSSZ.MarshalSSZ(expected.gloas)
	if err != nil {
		return err
	}
	b, err := gloasSSZ.MarshalSSZ(projection.gloas)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errors.New("gloas block does not match its execution block")
	}
	a, err = gloasSSZ.MarshalSSZ(expected.envelope)
	if err != nil {
		return err
	}
	b, err = gloasSSZ.MarshalSSZ(projection.envelope)
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errors.New("gloas envelope does not match its execution block")
	}
	return nil
}
func (b *consensusBlock) validateGloasStructure() error {
	if b.gloas == nil || b.gloas.Message == nil || b.gloas.Message.Body == nil || b.gloas.Message.Body.SignedExecutionPayloadBid == nil || b.gloas.Message.Body.SignedExecutionPayloadBid.Message == nil || b.gloas.Message.Body.ParentExecutionRequests == nil || b.envelope == nil || b.envelope.Message == nil || b.envelope.Message.Payload == nil || b.envelope.Message.ExecutionRequests == nil {
		return errors.New("gloas projection is structurally incomplete")
	}
	return nil
}
func (n *Node) beaconPayloadEnvelope(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	if !n.chain.config.IsAmsterdam(block.Number(), block.Time()) {
		writeBeaconError(w, http.StatusNotFound, errors.New("execution payload envelopes require Gloas"))
		return
	}
	ssz, err := beaconWantsSSZ(r)
	if err != nil {
		writeBeaconError(w, http.StatusNotAcceptable, err)
		return
	}
	projection, err := n.consensus.signedBlock(n.chain, block)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Eth-Consensus-Version", "gloas")
	if ssz {
		encoded, err := gloasSSZ.MarshalSSZ(projection.envelope)
		if err != nil {
			writeBeaconError(w, http.StatusInternalServerError, err)
			return
		}
		setSyntheticConsensusHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(encoded)
		return
	}
	writeBeaconJSON(w, http.StatusOK, n.beaconVersionedResponse(block, projection.envelope))
}

func typesHashBAL(encoded []byte) common.Hash { return crypto.Keccak256Hash(encoded) }

func (n *Node) gloasForkEpoch() string {
	if n.cfg.Chain.Forks.AmsterdamEpoch < 0 {
		return strconv.FormatUint(math.MaxUint64, 10)
	}
	return strconv.FormatInt(n.cfg.Chain.Forks.AmsterdamEpoch, 10)
}
