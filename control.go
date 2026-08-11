package ethertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"

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
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	projection, err := n.consensus.ensureProjection(chain, parent)
	if err != nil {
		return common.Hash{}, err
	}
	chain.mu.RLock()
	targetSlot := chain.slot + 1
	parentSafety := chain.blockSafety[parent.Hash()]
	timeline := chain.timeline()
	sessionSafety := chain.sessionSafety()
	chain.mu.RUnlock()
	targetTime := chain.genesisTime + targetSlot*chain.slotDuration
	withdrawals, err := assignedWithdrawals(chain.blockchain, parent, n.pendingWithdrawals)
	if err != nil {
		return common.Hash{}, err
	}
	blocks, receiptSets := core.GenerateChain(chain.config, parent, chain.blockchain.Engine(), chain.db, 1, func(_ int, generator *core.BlockGen) {
		generator.OffsetTime(int64(targetTime) - int64(generator.Timestamp()))
		generator.SetPoS()
		generator.SetCoinbase(chain.feeRecipientAddress())
		generator.SetParentBeaconRoot(common.Hash(projection.Root))
		addWithdrawals(generator, withdrawals)
	})
	generated := replaceGeneratedWithdrawals(blocks[0], receiptSets[0], withdrawals)
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
	if err := state.Database().TrieDB().Commit(root, false); err != nil {
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
	record, err := rlp.EncodeToBytes([][]byte{metadata, parent.Hash().Bytes()})
	if err != nil {
		return common.Hash{}, err
	}
	projectionPut, err := n.consensus.projectionPut(chain, block)
	if err != nil {
		return common.Hash{}, err
	}
	safety := blockSafetyForChild(parentSafety, block.Hash(), taintControlStateOverride)
	timeline.CurrentSlot, timeline.LastProcessedSlot = targetSlot, targetSlot
	sessionSafety.Tainted = true
	if sessionSafety.FirstUnsafeBlock == nil {
		sessionSafety.FirstUnsafeBlock = cloneHashPointer(safety.FirstUnsafeBlock)
	}
	if !slices.Contains(sessionSafety.Reasons, taintControlStateOverride) {
		sessionSafety.Reasons = append(sessionSafety.Reasons, taintControlStateOverride)
		slices.Sort(sessionSafety.Reasons)
	}
	timelineMutation, err := timelinePut(timeline)
	if err != nil {
		return common.Hash{}, err
	}
	safetyMutation, err := blockSafetyPut(block.Hash(), safety)
	if err != nil {
		return common.Hash{}, err
	}
	sessionMutation, err := sessionSafetyPut(sessionSafety)
	if err != nil {
		return common.Hash{}, err
	}
	operation := preparedOperation{
		Kind: "head", OldHead: parent.Hash(), NewHead: block.Hash(),
		TargetNumber: block.NumberU64(), DiscardTargetOnCancel: true,
		Puts: []journalKV{
			{Key: append(append([]byte(nil), controlNamespace...), block.Hash().Bytes()...), Value: record},
			timelineMutation, blockSlotPut(block.Hash(), targetSlot),
			canonicalSlotPut(targetSlot, block.Hash()), safetyMutation, sessionMutation, projectionPut,
		},
	}
	events := []Event{{Type: "control_block", Slot: targetSlot, BlockHash: block.Hash(), BlockNumber: block.NumberU64()}}
	if finalized := n.finalizedEventBetween(targetSlot-1, targetSlot); finalized != nil {
		events = append(events, *finalized)
	}
	if err := n.commitPrepared(chain, operation, events, func() error {
		rawdb.WriteBlock(chain.db, block)
		rawdb.WriteReceipts(chain.db, block.Hash(), block.NumberU64(), types.Receipts{})
		_, setErr := chain.blockchain.SetCanonical(block)
		return setErr
	}, func() {
		chain.mu.Lock()
		chain.slot, chain.lastProcessedSlot = targetSlot, targetSlot
		chain.slotByHash[block.Hash()] = targetSlot
		chain.canonicalBlockBySlot[targetSlot] = block.Hash()
		chain.blockSafety[block.Hash()] = safety
		chain.sessionTainted = true
		chain.firstUnsafeBlock = cloneHashPointer(sessionSafety.FirstUnsafeBlock)
		chain.taintReasons[taintControlStateOverride] = struct{}{}
		chain.mu.Unlock()
	}); err != nil {
		return common.Hash{}, err
	}
	n.pendingWithdrawals = nil
	if err := n.rebuildPendingView(chain); err != nil {
		n.writeErr = err
		return common.Hash{}, fmt.Errorf("control block committed but pending view rebuild failed: %w", err)
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
	changes, _, _, ok := decodeControlRecord(data)
	return changes, ok
}

func decodeControlRecord(data []byte) (ControlChanges, common.Hash, []byte, bool) {
	var record [][]byte
	if rlp.DecodeBytes(data, &record) != nil || len(record) != 2 || len(record[1]) != common.HashLength {
		return nil, common.Hash{}, nil, false
	}
	var changes ControlChanges
	if json.Unmarshal(record[0], &changes) != nil {
		return nil, common.Hash{}, nil, false
	}
	return changes, common.BytesToHash(record[1]), append([]byte(nil), record[0]...), true
}

// VerifyControlRecord verifies ethertest's unsafe fixture record. It does not
// assert that the block is valid under the Ethereum state transition.
func (n *Node) VerifyControlRecord(ctx context.Context, hash common.Hash) (bool, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		block := chain.blockchain.GetBlockByHash(hash)
		if block == nil {
			return false, errors.New("control block not found")
		}
		encoded, err := chain.db.Get(append(append([]byte(nil), controlNamespace...), hash.Bytes()...))
		if err != nil {
			return false, errors.New("control metadata not found")
		}
		changes, recordedParent, metadata, ok := decodeControlRecord(encoded)
		if !ok {
			return false, errors.New("control metadata is invalid")
		}
		if recordedParent != block.ParentHash() {
			return false, errors.New("control metadata parent does not match block")
		}
		digest := sha256.Sum256(metadata)
		wantExtra := append([]byte("ethertest-control-v1:"), digest[:10]...)
		if !bytes.Equal(block.Extra(), wantExtra) {
			return false, errors.New("control metadata digest does not match block")
		}
		parent := chain.blockchain.GetBlockByHash(block.ParentHash())
		if parent == nil {
			return false, errors.New("control parent not found")
		}
		projection, err := n.consensus.ensureProjection(chain, parent)
		if err != nil {
			return false, err
		}
		generated, _ := core.GenerateChain(chain.config, parent, chain.blockchain.Engine(), chain.db, 1, func(_ int, generator *core.BlockGen) {
			generator.OffsetTime(int64(block.Time()) - int64(generator.Timestamp()))
			generator.SetPoS()
			generator.SetCoinbase(block.Coinbase())
			generator.SetParentBeaconRoot(common.Hash(projection.Root))
			addWithdrawals(generator, block.Withdrawals())
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

// VerifyControlBlock is retained as a deprecated compatibility wrapper.
// Deprecated: use VerifyControlRecord.
func (n *Node) VerifyControlBlock(ctx context.Context, hash common.Hash) (bool, error) {
	return n.VerifyControlRecord(ctx, hash)
}
