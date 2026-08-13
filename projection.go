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

const projectionFormat = 1

var projectionPrefix = []byte("ethertest/beacon-projection/")

type storedProjection struct {
	Format     int         `json:"format"`
	Fork       string      `json:"fork"`
	Slot       uint64      `json:"slot"`
	Root       phase0.Root `json:"root"`
	ParentRoot phase0.Root `json:"parent_root"`
	SignedSSZ  []byte      `json:"signed_ssz"`
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
	case "deneb":
		value := new(deneb.SignedBeaconBlock)
		if err := value.UnmarshalSSZ(record.SignedSSZ); err != nil {
			return nil, storedProjection{}, false, fmt.Errorf("decode Deneb projection %s: %w", hash, err)
		}
		block.deneb = value
	case "electra", "fulu":
		value := new(electra.SignedBeaconBlock)
		if err := value.UnmarshalSSZ(record.SignedSSZ); err != nil {
			return nil, storedProjection{}, false, fmt.Errorf("decode %s projection %s: %w", record.Fork, hash, err)
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
	requests *electra.ExecutionRequests,
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
	requests *electra.ExecutionRequests,
) (journalKV, error) {
	_, encoded, err := m.projectionRecord(chain, block, requests)
	if err != nil {
		return journalKV{}, err
	}
	return journalKV{Key: projectionKey(block.Hash()), Value: encoded}, nil
}
