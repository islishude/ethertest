package ethertest

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const projectionFormat = 2

var projectionPrefix = []byte("ethertest/beacon-projection/")

type storedProjection struct {
	Format      int         `json:"format"`
	Fork        string      `json:"fork"`
	Slot        uint64      `json:"slot"`
	Root        phase0.Root `json:"root"`
	ParentRoot  phase0.Root `json:"parent_root"`
	EnvelopeSSZ []byte      `json:"envelope_ssz,omitempty"`
	SignedSSZ   []byte      `json:"signed_ssz"`
}

func projectionKey(hash common.Hash) []byte {
	return hashKey(projectionPrefix, hash)
}

func loadProjection(chain *executionChain, hash common.Hash) (*consensusBlock, storedProjection, bool, error) {
	exists, err := chain.db.Has(projectionKey(hash))
	if err != nil {
		return nil, storedProjection{}, false, err
	}
	if !exists {
		return nil, storedProjection{}, false, nil
	}
	encoded, err := chain.db.Get(projectionKey(hash))
	if err != nil {
		return nil, storedProjection{}, false, err
	}
	var record storedProjection
	if err := json.Unmarshal(encoded, &record); err != nil {
		return nil, storedProjection{}, false, fmt.Errorf("decode Beacon projection %s: %w", hash, err)
	}
	if record.Format != projectionFormat {
		return nil, storedProjection{}, false, fmt.Errorf("unsupported Beacon projection format %d", record.Format)
	}
	var block consensusBlock
	switch record.Fork {
	case "gloas":
		block.gloas = new(gloasSignedBeaconBlock)
		block.envelope = new(gloasSignedPayloadEnvelope)
		if err := gloasSSZ.UnmarshalSSZ(block.gloas, record.SignedSSZ); err != nil {
			return nil, storedProjection{}, false, err
		}
		if len(record.EnvelopeSSZ) == 0 {
			return nil, storedProjection{}, false, errors.New("gloas envelope is missing")
		}
		if err := gloasSSZ.UnmarshalSSZ(block.envelope, record.EnvelopeSSZ); err != nil {
			return nil, storedProjection{}, false, err
		}
		if err := block.validateGloasStructure(); err != nil {
			return nil, storedProjection{}, false, err
		}
	case "deneb":
		value := new(deneb.SignedBeaconBlock)
		if err := value.UnmarshalSSZ(record.SignedSSZ); err != nil {
			return nil, storedProjection{}, false, fmt.Errorf("decode Deneb projection %s: %w", hash, err)
		}
		if value.Message == nil || value.Message.Body == nil || value.Message.Body.ExecutionPayload == nil {
			return nil, storedProjection{}, false, fmt.Errorf("deneb projection %s is structurally incomplete", hash)
		}
		block.deneb = value
	case "electra", "fulu":
		value := new(electra.SignedBeaconBlock)
		if err := value.UnmarshalSSZ(record.SignedSSZ); err != nil {
			return nil, storedProjection{}, false, fmt.Errorf("decode %s projection %s: %w", record.Fork, hash, err)
		}
		if value.Message == nil || value.Message.Body == nil || value.Message.Body.ExecutionPayload == nil {
			return nil, storedProjection{}, false, fmt.Errorf("%s projection %s is structurally incomplete", record.Fork, hash)
		}
		block.electra = value
	default:
		return nil, storedProjection{}, false, fmt.Errorf("unsupported Beacon projection fork %q", record.Fork)
	}
	root, err := block.messageRoot()
	if err != nil {
		return nil, storedProjection{}, false, err
	}
	if root != record.Root || block.slot() != record.Slot || block.parentRoot() != record.ParentRoot {
		return nil, storedProjection{}, false, errors.New("stored Beacon projection metadata does not match SSZ object")
	}
	return &block, record, true, nil
}

func (m *consensusModel) projectionRecord(
	chain *executionChain,
	block *types.Block,
	requests *executionRequests,
) (storedProjection, []byte, error) {
	signed, err := m.signedBlockWithRequests(chain, block, requests)
	if err != nil {
		return storedProjection{}, nil, err
	}
	root, err := signed.messageRoot()
	if err != nil {
		return storedProjection{}, nil, err
	}
	ssz, err := signed.marshalSSZ()
	if err != nil {
		return storedProjection{}, nil, err
	}
	record := storedProjection{
		Format: projectionFormat, Fork: m.forkName(chain.slotOf(block)), Slot: chain.slotOf(block),
		Root: root, ParentRoot: signed.parentRoot(), SignedSSZ: ssz,
	}
	if signed.envelope != nil {
		record.EnvelopeSSZ, err = gloasSSZ.MarshalSSZ(signed.envelope)
		if err != nil {
			return storedProjection{}, nil, err
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return storedProjection{}, nil, err
	}
	return record, encoded, nil
}

func (m *consensusModel) ensureProjection(chain *executionChain, block *types.Block) (storedProjection, error) {
	_, record, exists, err := loadProjection(chain, block.Hash())
	if err != nil {
		return storedProjection{}, err
	}
	if exists {
		return record, nil
	}
	if block.NumberU64() != 0 {
		return storedProjection{}, fmt.Errorf("beacon projection for published block %s is missing", block.Hash())
	}
	record, encoded, err := m.projectionRecord(chain, block, nil)
	if err != nil {
		return storedProjection{}, err
	}
	if err := chain.db.Put(projectionKey(block.Hash()), encoded); err != nil {
		return storedProjection{}, err
	}
	return record, nil
}

func (m *consensusModel) projectionPut(
	chain *executionChain,
	block *types.Block,
	requests *executionRequests,
) (journalKV, error) {
	record, encoded, err := m.projectionRecord(chain, block, requests)
	if err != nil {
		return journalKV{}, err
	}
	chain.mu.RLock()
	previous, collision := chain.beaconBlockByRoot[common.Hash(record.Root)]
	chain.mu.RUnlock()
	if collision && previous != block.Hash() {
		return journalKV{}, fmt.Errorf("beacon root %s already belongs to execution block %s", record.Root, previous)
	}
	return journalKV{Key: projectionKey(block.Hash()), Value: encoded}, nil
}

func initializeBeaconRootIndex(model *consensusModel, chain *executionChain) error {
	chain.mu.RLock()
	hashes := make([]common.Hash, 0, len(chain.slotByHash))
	for hash := range chain.slotByHash {
		hashes = append(hashes, hash)
	}
	chain.mu.RUnlock()
	index := make(map[common.Hash]common.Hash, len(hashes))
	records := make(map[common.Hash]storedProjection, len(hashes))
	for _, hash := range hashes {
		consensusBlock, projection, exists, err := loadProjection(chain, hash)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("execution block %s has no Beacon projection", hash)
		}
		executionBlock := chain.blockchain.GetBlockByHash(hash)
		if executionBlock == nil {
			return fmt.Errorf("beacon projection %s has no execution block", hash)
		}
		if projection.Slot != chain.slotOf(executionBlock) {
			return fmt.Errorf("beacon projection %s has slot %d, want %d", hash, projection.Slot, chain.slotOf(executionBlock))
		}
		expectedFork := "deneb"
		if chain.config.IsPrague(executionBlock.Number(), executionBlock.Time()) {
			expectedFork = "electra"
		}
		if chain.config.IsOsaka(executionBlock.Number(), executionBlock.Time()) {
			expectedFork = "fulu"
		}
		if chain.config.IsAmsterdam(executionBlock.Number(), executionBlock.Time()) {
			expectedFork = "gloas"
		}
		if projection.Fork != expectedFork {
			return fmt.Errorf("beacon projection %s uses fork %q, want %q", hash, projection.Fork, expectedFork)
		}
		executionHash, err := consensusBlock.executionHash()
		if err != nil || executionHash != hash {
			return fmt.Errorf("beacon projection %s execution payload is inconsistent", hash)
		}
		if err := model.validateProjectionObject(chain, executionBlock, consensusBlock); err != nil {
			return fmt.Errorf("validate Beacon projection %s: %w", hash, err)
		}
		root := common.Hash(projection.Root)
		if previous, duplicate := index[root]; duplicate && previous != hash {
			return fmt.Errorf("beacon root %s maps to both %s and %s", root, previous, hash)
		}
		index[root] = hash
		records[hash] = projection
	}
	for hash, projection := range records {
		block := chain.blockchain.GetBlockByHash(hash)
		if block.NumberU64() == 0 {
			if projection.ParentRoot != (phase0.Root{}) {
				return errors.New("genesis Beacon projection has a nonzero parent root")
			}
			continue
		}
		parent, exists := records[block.ParentHash()]
		if !exists || projection.ParentRoot != parent.Root {
			return fmt.Errorf("beacon projection %s does not reference its execution parent's Beacon root", hash)
		}
	}
	chain.mu.Lock()
	chain.beaconBlockByRoot = index
	chain.mu.Unlock()
	return nil
}

func applyProjectionIndex(chain *executionChain, executionHash common.Hash, mutation journalKV) {
	var projection storedProjection
	if json.Unmarshal(mutation.Value, &projection) != nil {
		return
	}
	chain.mu.Lock()
	chain.beaconBlockByRoot[common.Hash(projection.Root)] = executionHash
	chain.mu.Unlock()
}
