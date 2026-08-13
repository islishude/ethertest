package ethertest

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	bitfield "github.com/OffchainLabs/go-bitfield"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
	bls "github.com/protolambda/bls12-381-util"
)

const validatorRegistryLimitDepth = 40

// consensusModel builds a deterministic synthetic Beacon projection. None of
// its state roots, proposer choices, finality, or sync aggregates are outputs
// of the BeaconState transition or canonical proposer-selection algorithms.
type consensusModel struct {
	keys                  []*bls.SecretKey
	pubkeys               [][48]byte
	withdrawalCredentials [][32]byte
	genesisValidatorsRoot phase0.Root
	mu                    sync.Mutex
	blocks                map[common.Hash]*consensusBlock
	forks                 ForkConfig
	slotsPerEpoch         uint64
}

type consensusBlock struct {
	deneb   *deneb.SignedBeaconBlock
	electra *electra.SignedBeaconBlock
}

func (b *consensusBlock) messageRoot() (phase0.Root, error) {
	if b.deneb != nil {
		return b.deneb.Message.HashTreeRoot()
	}
	return b.electra.Message.HashTreeRoot()
}

func (b *consensusBlock) marshalSSZ() ([]byte, error) {
	if b.deneb != nil {
		return b.deneb.MarshalSSZ()
	}
	return b.electra.MarshalSSZ()
}

func (b *consensusBlock) value() any {
	if b.deneb != nil {
		return b.deneb
	}
	return b.electra
}

func (b *consensusBlock) commitments() []deneb.KZGCommitment {
	if b.deneb != nil {
		return b.deneb.Message.Body.BlobKZGCommitments
	}
	return b.electra.Message.Body.BlobKZGCommitments
}

func (b *consensusBlock) slot() uint64 {
	if b.deneb != nil {
		return uint64(b.deneb.Message.Slot)
	}
	return uint64(b.electra.Message.Slot)
}

func (b *consensusBlock) parentRoot() phase0.Root {
	if b.deneb != nil {
		return b.deneb.Message.ParentRoot
	}
	return b.electra.Message.ParentRoot
}

func newConsensusModel(cfg Config, executionAddresses []common.Address) (*consensusModel, error) {
	model := &consensusModel{
		keys:                  make([]*bls.SecretKey, cfg.Chain.Validators),
		pubkeys:               make([][48]byte, cfg.Chain.Validators),
		withdrawalCredentials: make([][32]byte, cfg.Chain.Validators),
		blocks:                make(map[common.Hash]*consensusBlock),
		forks:                 cfg.Chain.Forks,
		slotsPerEpoch:         cfg.Chain.SlotsPerEpoch,
	}
	validatorRoots := make([][32]byte, cfg.Chain.Validators)
	for index := range cfg.Chain.Validators {
		seed := sha256.Sum256(fmt.Appendf(nil, "ethertest-validator-%d", index))
		key := new(bls.SecretKey)
		if err := key.Deserialize(&seed); err != nil {
			return nil, err
		}
		pubkey, err := bls.SkToPk(key)
		if err != nil {
			return nil, err
		}
		model.keys[index] = key
		model.pubkeys[index] = pubkey.Serialize()
		credentials := [32]byte{0x01}
		address := executionAddresses[int(index)%len(executionAddresses)]
		copy(credentials[12:], address[:])
		model.withdrawalCredentials[index] = credentials
		validatorRoots[index] = validatorRoot(model.pubkeys[index], credentials, 32_000_000_000)
	}
	model.genesisValidatorsRoot = phase0.Root(merkleizeList(validatorRoots, validatorRegistryLimitDepth))
	return model, nil
}

func validatorRoot(pubkey [48]byte, credentials [32]byte, balance uint64) [32]byte {
	var pubkeyFirst, pubkeySecond [32]byte
	copy(pubkeyFirst[:], pubkey[:32])
	copy(pubkeySecond[:16], pubkey[32:])
	uintChunk := func(value uint64) [32]byte {
		var chunk [32]byte
		binary.LittleEndian.PutUint64(chunk[:8], value)
		return chunk
	}
	farFuture := uintChunk(math.MaxUint64)
	return merkleizeFixed([][32]byte{
		hashPair(pubkeyFirst, pubkeySecond), credentials, uintChunk(balance), {},
		uintChunk(0), uintChunk(0), farFuture, farFuture,
	})
}

func merkleizeFixed(leaves [][32]byte) [32]byte {
	for len(leaves) > 1 {
		next := make([][32]byte, len(leaves)/2)
		for i := range next {
			next[i] = hashPair(leaves[2*i], leaves[2*i+1])
		}
		leaves = next
	}
	return leaves[0]
}

func merkleizeList(leaves [][32]byte, depth int) [32]byte {
	zeros := make([][32]byte, depth+1)
	for i := 1; i <= depth; i++ {
		zeros[i] = hashPair(zeros[i-1], zeros[i-1])
	}
	nodes := append([][32]byte(nil), leaves...)
	level := 0
	for len(nodes) > 1 {
		if len(nodes)%2 != 0 {
			nodes = append(nodes, zeros[level])
		}
		next := make([][32]byte, len(nodes)/2)
		for i := range next {
			next[i] = hashPair(nodes[2*i], nodes[2*i+1])
		}
		nodes = next
		level++
	}
	root := zeros[0]
	if len(nodes) == 1 {
		root = nodes[0]
	}
	for ; level < depth; level++ {
		root = hashPair(root, zeros[level])
	}
	var length [32]byte
	binary.LittleEndian.PutUint64(length[:8], uint64(len(leaves)))
	return hashPair(root, length)
}

func hashPair(left, right [32]byte) [32]byte {
	var input [64]byte
	copy(input[:32], left[:])
	copy(input[32:], right[:])
	return sha256.Sum256(input[:])
}

func zeroHash(depth int) [32]byte {
	var root [32]byte
	for range depth {
		root = hashPair(root, root)
	}
	return root
}

func emptyListRoot(limit uint64) [32]byte {
	depth := 0
	for size := uint64(1); size < limit; size <<= 1 {
		depth++
	}
	return hashPair(zeroHash(depth), [32]byte{})
}

func (m *consensusModel) signedHeader(chain *executionChain, block *types.Block) (*phase0.SignedBeaconBlockHeader, error) {
	signed, err := m.signedBlock(chain, block)
	if err != nil {
		return nil, err
	}
	if signed.deneb != nil {
		bodyRoot, err := signed.deneb.Message.Body.HashTreeRoot()
		if err != nil {
			return nil, err
		}
		return &phase0.SignedBeaconBlockHeader{
			Message: &phase0.BeaconBlockHeader{
				Slot: signed.deneb.Message.Slot, ProposerIndex: signed.deneb.Message.ProposerIndex,
				ParentRoot: signed.deneb.Message.ParentRoot, StateRoot: signed.deneb.Message.StateRoot, BodyRoot: bodyRoot,
			},
			Signature: signed.deneb.Signature,
		}, nil
	}
	bodyRoot, err := signed.electra.Message.Body.HashTreeRoot()
	if err != nil {
		return nil, err
	}
	return &phase0.SignedBeaconBlockHeader{
		Message: &phase0.BeaconBlockHeader{
			Slot: signed.electra.Message.Slot, ProposerIndex: signed.electra.Message.ProposerIndex,
			ParentRoot: signed.electra.Message.ParentRoot, StateRoot: signed.electra.Message.StateRoot, BodyRoot: bodyRoot,
		},
		Signature: signed.electra.Signature,
	}, nil
}

func (m *consensusModel) signedBlock(chain *executionChain, block *types.Block) (*consensusBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.signedBlockLocked(chain, block, nil)
}

func (m *consensusModel) signedBlockWithRequests(
	chain *executionChain,
	block *types.Block,
	requests *electra.ExecutionRequests,
) (*consensusBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.signedBlockLocked(chain, block, requests)
}

func (m *consensusModel) signedBlockLocked(
	chain *executionChain,
	block *types.Block,
	requests *electra.ExecutionRequests,
) (*consensusBlock, error) {
	if existing := m.blocks[block.Hash()]; existing != nil {
		return existing, nil
	}
	stored, _, exists, err := loadProjection(chain, block.Hash())
	if err != nil {
		return nil, err
	}
	if exists {
		m.blocks[block.Hash()] = stored
		return stored, nil
	}
	if block.NumberU64() != 0 && chain.blockchain.GetBlockByHash(block.Hash()) != nil {
		return nil, fmt.Errorf("beacon projection for published block %s is missing", block.Hash())
	}
	slot := chain.slotOf(block)
	parentRoot := phase0.Root{}
	if block.NumberU64() != 0 {
		parent := chain.blockchain.GetBlockByHash(block.ParentHash())
		if parent == nil {
			return nil, fmt.Errorf("execution parent %s not found", block.ParentHash())
		}
		parentSigned, err := m.signedBlockLocked(chain, parent, nil)
		if err != nil {
			return nil, err
		}
		root, err := parentSigned.messageRoot()
		if err != nil {
			return nil, err
		}
		parentRoot = root
	}
	body, err := m.body(chain, block, slot, requests)
	if err != nil {
		return nil, err
	}
	var stateInput [40]byte
	copy(stateInput[:32], block.Root().Bytes())
	binary.LittleEndian.PutUint64(stateInput[32:], slot)
	syntheticProposer := phase0.ValidatorIndex(slot % uint64(len(m.keys)))
	syntheticStateRoot := phase0.Root(sha256.Sum256(stateInput[:]))
	var signed *consensusBlock
	if slot/m.slotsPerEpoch < m.forks.PragueEpoch {
		denebBody := denebBodyFromElectra(body)
		message := &deneb.BeaconBlock{
			Slot: phase0.Slot(slot), ProposerIndex: syntheticProposer,
			ParentRoot: parentRoot, StateRoot: syntheticStateRoot, Body: denebBody,
		}
		objectRoot, err := message.HashTreeRoot()
		if err != nil {
			return nil, err
		}
		signature, err := m.sign(objectRoot, phase0.DomainType{}, uint64(syntheticProposer), slot)
		if err != nil {
			return nil, err
		}
		signed = &consensusBlock{deneb: &deneb.SignedBeaconBlock{Message: message, Signature: signature}}
	} else {
		message := &electra.BeaconBlock{
			Slot: phase0.Slot(slot), ProposerIndex: syntheticProposer,
			ParentRoot: parentRoot, StateRoot: syntheticStateRoot, Body: body,
		}
		objectRoot, err := message.HashTreeRoot()
		if err != nil {
			return nil, err
		}
		signature, err := m.sign(objectRoot, phase0.DomainType{}, uint64(syntheticProposer), slot)
		if err != nil {
			return nil, err
		}
		signed = &consensusBlock{electra: &electra.SignedBeaconBlock{Message: message, Signature: signature}}
	}
	m.blocks[block.Hash()] = signed
	return signed, nil
}

func denebBodyFromElectra(body *electra.BeaconBlockBody) *deneb.BeaconBlockBody {
	return &deneb.BeaconBlockBody{
		RANDAOReveal: body.RANDAOReveal, ETH1Data: body.ETH1Data, Graffiti: body.Graffiti,
		ProposerSlashings: []*phase0.ProposerSlashing{},
		AttesterSlashings: []*phase0.AttesterSlashing{},
		Attestations:      []*phase0.Attestation{},
		Deposits:          body.Deposits, VoluntaryExits: body.VoluntaryExits,
		SyncAggregate: body.SyncAggregate, ExecutionPayload: body.ExecutionPayload,
		BLSToExecutionChanges: body.BLSToExecutionChanges,
		BlobKZGCommitments:    body.BlobKZGCommitments,
	}
}

func (m *consensusModel) body(
	chain *executionChain,
	block *types.Block,
	slot uint64,
	executionRequests *electra.ExecutionRequests,
) (*electra.BeaconBlockBody, error) {
	transactions := make([]bellatrix.Transaction, len(block.Transactions()))
	commitments := make([]deneb.KZGCommitment, 0)
	for index, transaction := range block.Transactions() {
		raw, err := transaction.WithoutBlobTxSidecar().MarshalBinary()
		if err != nil {
			return nil, err
		}
		transactions[index] = raw
		if sidecar := chain.blobSidecar(transaction.Hash()); sidecar != nil {
			for _, commitment := range sidecar.Commitments {
				commitments = append(commitments, deneb.KZGCommitment(commitment))
			}
		}
	}
	withdrawals := make([]*capella.Withdrawal, len(block.Withdrawals()))
	for index, withdrawal := range block.Withdrawals() {
		withdrawals[index] = &capella.Withdrawal{
			Index:          capella.WithdrawalIndex(withdrawal.Index),
			ValidatorIndex: phase0.ValidatorIndex(withdrawal.Validator),
			Address:        bellatrix.ExecutionAddress(withdrawal.Address), Amount: phase0.Gwei(withdrawal.Amount),
		}
	}
	payload := &deneb.ExecutionPayload{
		ParentHash: phase0.Hash32(block.ParentHash()), FeeRecipient: bellatrix.ExecutionAddress(block.Coinbase()),
		StateRoot: phase0.Root(block.Root()), ReceiptsRoot: phase0.Root(block.ReceiptHash()),
		LogsBloom: block.Bloom(), PrevRandao: block.MixDigest(), BlockNumber: block.NumberU64(),
		GasLimit: block.GasLimit(), GasUsed: block.GasUsed(), Timestamp: block.Time(),
		ExtraData: block.Extra(), BaseFeePerGas: uint256.MustFromBig(block.BaseFee()),
		BlockHash: phase0.Hash32(block.Hash()), Transactions: transactions, Withdrawals: withdrawals,
	}
	if block.BlobGasUsed() != nil {
		payload.BlobGasUsed = *block.BlobGasUsed()
	}
	if block.ExcessBlobGas() != nil {
		payload.ExcessBlobGas = *block.ExcessBlobGas()
	}
	var epochRoot [32]byte
	binary.LittleEndian.PutUint64(epochRoot[:8], slot/m.slotsPerEpoch)
	randao, err := m.sign(epochRoot, phase0.DomainType{0x02, 0, 0, 0}, slot%uint64(len(m.keys)), slot)
	if err != nil {
		return nil, err
	}
	return &electra.BeaconBlockBody{
		RANDAOReveal: randao, ETH1Data: &phase0.ETH1Data{BlockHash: block.Hash().Bytes()},
		ProposerSlashings: []*phase0.ProposerSlashing{}, AttesterSlashings: []*electra.AttesterSlashing{},
		Attestations: []*electra.Attestation{}, Deposits: []*phase0.Deposit{},
		VoluntaryExits:   []*phase0.SignedVoluntaryExit{},
		SyncAggregate:    &altair.SyncAggregate{SyncCommitteeBits: make(bitfield.Bitvector512, 64)},
		ExecutionPayload: payload, BLSToExecutionChanges: []*capella.SignedBLSToExecutionChange{},
		BlobKZGCommitments: commitments,
		ExecutionRequests:  cloneElectraExecutionRequests(executionRequests),
	}, nil
}

func (m *consensusModel) kzgCommitmentsInclusionProof(chain *executionChain, block *types.Block) ([4][32]byte, error) {
	signed, err := m.signedBlock(chain, block)
	if err != nil {
		return [4][32]byte{}, err
	}
	if signed.deneb != nil {
		return denebKZGCommitmentsInclusionProof(signed.deneb.Message.Body)
	}
	body := signed.electra.Message.Body
	var randaoChunks [][32]byte
	for index := range 3 {
		var chunk [32]byte
		copy(chunk[:], body.RANDAOReveal[index*32:(index+1)*32])
		randaoChunks = append(randaoChunks, chunk)
	}
	randaoChunks = append(randaoChunks, [32]byte{})
	randaoRoot := merkleizeFixed(randaoChunks)
	eth1Root, err := body.ETH1Data.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	syncRoot, err := body.SyncAggregate.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	payloadRoot, err := body.ExecutionPayload.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	requestsRoot, err := body.ExecutionRequests.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	commitmentRoots := make([][32]byte, len(body.BlobKZGCommitments))
	for index, commitment := range body.BlobKZGCommitments {
		var first, second [32]byte
		copy(first[:], commitment[:32])
		copy(second[:16], commitment[32:])
		commitmentRoots[index] = hashPair(first, second)
	}
	commitmentsRoot := merkleizeList(commitmentRoots, 12)
	leaves := make([][32]byte, 16)
	copy(leaves, [][32]byte{
		randaoRoot, eth1Root, body.Graffiti,
		emptyListRoot(16), emptyListRoot(1), emptyListRoot(8),
		emptyListRoot(16), emptyListRoot(16), syncRoot, payloadRoot,
		emptyListRoot(16), commitmentsRoot, requestsRoot,
	})
	bodyRoot := merkleizeFixed(append([][32]byte(nil), leaves...))
	want, err := body.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	if bodyRoot != want {
		return [4][32]byte{}, errors.New("internal body proof tree does not match SSZ root")
	}
	var proof [4][32]byte
	index := 11
	nodes := leaves
	for level := range 4 {
		proof[level] = nodes[index^1]
		next := make([][32]byte, len(nodes)/2)
		for i := range next {
			next[i] = hashPair(nodes[2*i], nodes[2*i+1])
		}
		index /= 2
		nodes = next
	}
	return proof, nil
}

func denebKZGCommitmentsInclusionProof(body *deneb.BeaconBlockBody) ([4][32]byte, error) {
	var randaoChunks [][32]byte
	for index := range 3 {
		var chunk [32]byte
		copy(chunk[:], body.RANDAOReveal[index*32:(index+1)*32])
		randaoChunks = append(randaoChunks, chunk)
	}
	randaoChunks = append(randaoChunks, [32]byte{})
	randaoRoot := merkleizeFixed(randaoChunks)
	eth1Root, err := body.ETH1Data.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	syncRoot, err := body.SyncAggregate.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	payloadRoot, err := body.ExecutionPayload.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	commitmentRoots := make([][32]byte, len(body.BlobKZGCommitments))
	for index, commitment := range body.BlobKZGCommitments {
		var first, second [32]byte
		copy(first[:], commitment[:32])
		copy(second[:16], commitment[32:])
		commitmentRoots[index] = hashPair(first, second)
	}
	commitmentsRoot := merkleizeList(commitmentRoots, 12)
	leaves := make([][32]byte, 16)
	copy(leaves, [][32]byte{
		randaoRoot, eth1Root, body.Graffiti,
		emptyListRoot(16), emptyListRoot(2), emptyListRoot(128),
		emptyListRoot(16), emptyListRoot(16), syncRoot, payloadRoot,
		emptyListRoot(16), commitmentsRoot,
	})
	bodyRoot := merkleizeFixed(append([][32]byte(nil), leaves...))
	want, err := body.HashTreeRoot()
	if err != nil {
		return [4][32]byte{}, err
	}
	if bodyRoot != want {
		return [4][32]byte{}, errors.New("internal Deneb body proof tree does not match SSZ root")
	}
	var proof [4][32]byte
	index := 11
	nodes := leaves
	for level := range 4 {
		proof[level] = nodes[index^1]
		next := make([][32]byte, len(nodes)/2)
		for i := range next {
			next[i] = hashPair(nodes[2*i], nodes[2*i+1])
		}
		index /= 2
		nodes = next
	}
	return proof, nil
}

func (m *consensusModel) blobCommitmentInclusionProof(
	chain *executionChain,
	block *types.Block,
	commitmentIndex uint64,
) ([][32]byte, error) {
	signed, err := m.signedBlock(chain, block)
	if err != nil {
		return nil, err
	}
	commitments := signed.commitments()
	if commitmentIndex >= uint64(len(commitments)) {
		return nil, fmt.Errorf("blob commitment index %d is out of range", commitmentIndex)
	}
	const listDepth = 12
	nodes := make([][32]byte, 1<<listDepth)
	for index, commitment := range commitments {
		var first, second [32]byte
		copy(first[:], commitment[:32])
		copy(second[:16], commitment[32:])
		nodes[index] = hashPair(first, second)
	}
	proof := make([][32]byte, 0, 17)
	index := int(commitmentIndex)
	for range listDepth {
		proof = append(proof, nodes[index^1])
		next := make([][32]byte, len(nodes)/2)
		for node := range next {
			next[node] = hashPair(nodes[2*node], nodes[2*node+1])
		}
		nodes = next
		index /= 2
	}
	var length [32]byte
	binary.LittleEndian.PutUint64(length[:8], uint64(len(commitments)))
	proof = append(proof, length)
	bodyProof, err := m.kzgCommitmentsInclusionProof(chain, block)
	if err != nil {
		return nil, err
	}
	proof = append(proof, bodyProof[:]...)
	return proof, nil
}

func (m *consensusModel) proposerDomain() (phase0.Domain, error) {
	return m.domain(phase0.DomainType{}, m.forkVersion(0))
}

func (m *consensusModel) domain(domainType phase0.DomainType, version phase0.Version) (phase0.Domain, error) {
	forkData := &phase0.ForkData{
		CurrentVersion:        version,
		GenesisValidatorsRoot: m.genesisValidatorsRoot,
	}
	root, err := forkData.HashTreeRoot()
	if err != nil {
		return phase0.Domain{}, err
	}
	var domain phase0.Domain
	copy(domain[:4], domainType[:])
	copy(domain[4:], root[:28])
	return domain, nil
}

func (m *consensusModel) sign(objectRoot [32]byte, domainType phase0.DomainType, validator, slot uint64) (phase0.BLSSignature, error) {
	domain, err := m.domain(domainType, m.forkVersion(slot))
	if err != nil {
		return phase0.BLSSignature{}, err
	}
	signingRoot, err := (&phase0.SigningData{ObjectRoot: objectRoot, Domain: domain}).HashTreeRoot()
	if err != nil {
		return phase0.BLSSignature{}, err
	}
	signature := bls.Sign(m.keys[validator], signingRoot[:]).Serialize()
	return phase0.BLSSignature(signature), nil
}

func (m *consensusModel) forkVersion(slot uint64) phase0.Version {
	epoch := slot / m.slotsPerEpoch
	switch {
	case epoch >= m.forks.OsakaEpoch:
		return phase0.Version{0x06, 0, 0, 0}
	case epoch >= m.forks.PragueEpoch:
		return phase0.Version{0x05, 0, 0, 0}
	default:
		return phase0.Version{0x04, 0, 0, 0}
	}
}

func (m *consensusModel) forkName(slot uint64) string {
	version := m.forkVersion(slot)
	switch version[0] {
	case 0x04:
		return "deneb"
	case 0x05:
		return "electra"
	default:
		return "fulu"
	}
}
