package ethertest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
)

var controlNamespace = []byte("ethertest/control/")

type AccountChanges struct {
	Balance     *big.Int                     `json:"balance,omitempty"`
	Nonce       *uint64                      `json:"nonce,omitempty"`
	Code        *[]byte                      `json:"code,omitempty"`
	Storage     *map[common.Hash]common.Hash `json:"storage,omitempty"`
	StorageDiff *map[common.Hash]common.Hash `json:"storage_diff,omitempty"`
}

type ControlChanges map[common.Address]AccountChanges

func (n *Node) ApplyControl(ctx context.Context, changes ControlChanges) (common.Hash, error) {
	if len(changes) == 0 {
		return common.Hash{}, errors.New("control changes are empty")
	}
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		return n.applyControl(chain, changes)
	})
	if err != nil {
		return common.Hash{}, err
	}
	return value.(common.Hash), nil
}

func (n *Node) applyControl(chain *executionChain, changes ControlChanges) (common.Hash, error) {
	chain.mu.Lock()
	defer chain.mu.Unlock()
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	targetSlot := chain.slot + 1
	targetTime := chain.genesisTime + targetSlot*chain.slotDuration
	blocks, _ := core.GenerateChain(chain.config, parent, chain.blockchain.Engine(), chain.db, 1, func(_ int, generator *core.BlockGen) {
		generator.OffsetTime(int64(targetTime) - int64(generator.Timestamp()))
		generator.SetPoS()
		generator.SetCoinbase(chain.feeRecipient)
		generator.SetParentBeaconRoot(parent.Hash())
	})
	generated := blocks[0]
	state, err := chain.blockchain.StateAt(generated.Header())
	if err != nil {
		return common.Hash{}, err
	}
	if err := applyAccountChanges(state, changes); err != nil {
		return common.Hash{}, err
	}
	root, err := state.Commit(generated.NumberU64(), true, true)
	if err != nil {
		return common.Hash{}, err
	}
	metadata, err := json.Marshal(changes)
	if err != nil {
		return common.Hash{}, err
	}
	digest := sha256.Sum256(metadata)
	header := generated.Header()
	header.Root = root
	header.Extra = append([]byte("ethertest-control-v1:"), digest[:10]...)
	block := generated.WithSeal(header)
	rawdb.WriteBlock(chain.db, block)
	rawdb.WriteReceipts(chain.db, block.Hash(), block.NumberU64(), types.Receipts{})
	if _, err := chain.blockchain.SetCanonical(block); err != nil {
		return common.Hash{}, err
	}
	record, err := rlp.EncodeToBytes([][]byte{metadata, parent.Hash().Bytes()})
	if err != nil {
		return common.Hash{}, err
	}
	if err := chain.db.Put(append(append([]byte(nil), controlNamespace...), block.Hash().Bytes()...), record); err != nil {
		return common.Hash{}, err
	}
	chain.slot = targetSlot
	chain.slotByHash[block.Hash()] = targetSlot
	chain.blockBySlot[targetSlot] = block.Hash()
	if _, err := n.events.append(Event{Type: "control_block", BlockHash: block.Hash(), BlockNumber: block.NumberU64()}); err != nil {
		return common.Hash{}, err
	}
	n.logger.Info("control block applied",
		"event", "control_block_applied",
		"block_number", block.NumberU64(),
		"block_hash", block.Hash().Hex(),
		"slot", targetSlot,
		"accounts_changed", len(changes),
	)
	return block.Hash(), nil
}

func applyAccountChanges(state *state.StateDB, changes ControlChanges) error {
	for address, change := range changes {
		if change.Balance != nil {
			balance, overflow := uint256.FromBig(change.Balance)
			if overflow {
				return errors.New("control balance exceeds uint256")
			}
			state.SetBalance(address, balance, tracing.BalanceChangeUnspecified)
		}
		if change.Nonce != nil {
			state.SetNonce(address, *change.Nonce, tracing.NonceChangeUnspecified)
		}
		if change.Code != nil {
			state.SetCode(address, *change.Code, tracing.CodeChangeUnspecified)
		}
		if change.Storage != nil {
			state.SetStorage(address, *change.Storage)
		}
		if change.StorageDiff != nil {
			for key, value := range *change.StorageDiff {
				state.SetState(address, key, value)
			}
		}
	}
	return nil
}

func (n *Node) ControlChanges(hash common.Hash) (ControlChanges, bool) {
	data, err := n.chain.db.Get(append(append([]byte(nil), controlNamespace...), hash.Bytes()...))
	if err != nil {
		return nil, false
	}
	var record [][]byte
	if rlp.DecodeBytes(data, &record) != nil || len(record) == 0 {
		return nil, false
	}
	var changes ControlChanges
	if json.Unmarshal(record[0], &changes) != nil {
		return nil, false
	}
	return changes, true
}

func (n *Node) VerifyControlBlock(ctx context.Context, hash common.Hash) (bool, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		chain.mu.Lock()
		defer chain.mu.Unlock()
		block := chain.blockchain.GetBlockByHash(hash)
		if block == nil {
			return false, errors.New("control block not found")
		}
		changes, ok := n.ControlChanges(hash)
		if !ok {
			return false, errors.New("control metadata not found")
		}
		parent := chain.blockchain.GetBlockByHash(block.ParentHash())
		if parent == nil {
			return false, errors.New("control parent not found")
		}
		generated, _ := core.GenerateChain(chain.config, parent, chain.blockchain.Engine(), chain.db, 1, func(_ int, generator *core.BlockGen) {
			generator.OffsetTime(int64(block.Time()) - int64(generator.Timestamp()))
			generator.SetPoS()
			generator.SetCoinbase(block.Coinbase())
			generator.SetParentBeaconRoot(parent.Hash())
		})
		state, err := chain.blockchain.StateAt(generated[0].Header())
		if err != nil {
			return false, err
		}
		if err := applyAccountChanges(state, changes); err != nil {
			return false, err
		}
		return state.IntermediateRoot(true) == block.Root(), nil
	})
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}
