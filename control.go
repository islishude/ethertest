package ethertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
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
	if uint64(len(changes)) > n.cfg.Limits.MaxControlOperations {
		return common.Hash{}, newResourceLimitError(
			"control account count", uint64(len(changes)), n.cfg.Limits.MaxControlOperations,
		)
	}
	changes = cloneControlChanges(changes)
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		return n.applyControl(ctx, chain, changes)
	})
	if err != nil {
		return common.Hash{}, err
	}
	return value.(common.Hash), nil
}

func cloneControlChanges(changes ControlChanges) ControlChanges {
	cloned := make(ControlChanges, len(changes))
	for address, change := range changes {
		copy := AccountChanges{}
		if change.Balance != nil {
			copy.Balance = new(big.Int).Set(change.Balance)
		}
		if change.Nonce != nil {
			value := *change.Nonce
			copy.Nonce = &value
		}
		if change.Code != nil {
			value := append([]byte(nil), (*change.Code)...)
			copy.Code = &value
		}
		if change.Storage != nil {
			value := make(map[common.Hash]common.Hash, len(*change.Storage))
			maps.Copy(value, *change.Storage)
			copy.Storage = &value
		}
		if change.StorageDiff != nil {
			value := make(map[common.Hash]common.Hash, len(*change.StorageDiff))
			maps.Copy(value, *change.StorageDiff)
			copy.StorageDiff = &value
		}
		cloned[address] = copy
	}
	return cloned
}

func (n *Node) applyControl(ctx context.Context, chain *executionChain, changes ControlChanges) (common.Hash, error) {
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	projection, err := n.consensus.ensureProjection(chain, parent)
	if err != nil {
		return common.Hash{}, err
	}
	chain.mu.RLock()
	if chain.slot == math.MaxUint64 {
		chain.mu.RUnlock()
		return common.Hash{}, errors.New("control block slot overflows uint64")
	}
	targetSlot := chain.slot + 1
	parentSafety := chain.blockSafety[parent.Hash()]
	timeline := chain.timeline()
	sessionSafety := chain.sessionSafety()
	chain.mu.RUnlock()
	if targetSlot > (math.MaxUint64-chain.genesisTime)/chain.slotDuration {
		return common.Hash{}, errors.New("control block timestamp overflows uint64")
	}
	targetTime := chain.genesisTime + targetSlot*chain.slotDuration
	withdrawals, err := assignedWithdrawals(chain.blockchain, parent, n.pendingWithdrawals)
	if err != nil {
		return common.Hash{}, err
	}
	candidate, initialState, err := newCandidateState(chain.db, parent.Root())
	if err != nil {
		return common.Hash{}, err
	}
	generated, receipts, state, nativeRequests, err := chain.generateBlock(
		ctx, parent, targetTime, common.Hash(projection.Root), nil, withdrawals, false, nil, initialState, nil,
	)
	if err != nil {
		return common.Hash{}, err
	}
	rules := chain.config.Rules(generated.Number(), true, generated.Time())
	executionChanges := changes
	if rules.IsAmsterdam {
		executionChanges, err = expandStorageReplacements(ctx, chain, parent, generated.AccessList(), changes)
		if err != nil {
			return common.Hash{}, err
		}
	}
	if rules.IsAmsterdam {
		state.Prepare(rules, common.Address{}, common.Address{}, nil, nil, nil)
		state.SetTxContext(common.Hash{}, 0, 1)
	}
	if err := applyAccountChanges(state, executionChanges); err != nil {
		return common.Hash{}, err
	}
	var accessList *bal.BlockAccessList
	if rules.IsAmsterdam {
		merged := constructionAccessList(generated.AccessList())
		merged.Merge(state.Finalise(rules))
		accessList = merged.ToEncodingObj()
		if err := accessList.Validate(generated.GasLimit(), 0); err != nil {
			return common.Hash{}, err
		}
	}
	root := state.IntermediateRoot(rules)
	metadata, err := json.Marshal(changes)
	if err != nil {
		return common.Hash{}, err
	}
	digest := sha256.Sum256(metadata)
	header := generated.Header()
	header.Root = root
	header.Extra = append([]byte("ethertest-control-v1:"), digest[:10]...)
	if accessList != nil {
		hash := accessList.Hash()
		header.BlockAccessListHash = &hash
	}
	block := generated.WithSeal(header)
	if accessList != nil {
		block = block.WithAccessListUnsafe(accessList)
	}
	prepared, err := prepareExecutionRequestBlock(block, nativeRequests, n.pendingExecutionRequests)
	if err != nil {
		return common.Hash{}, err
	}
	block = prepared.Block
	if err := chain.deriveReceiptFields(block, receipts); err != nil {
		return common.Hash{}, err
	}
	controlRecord, err := rlp.EncodeToBytes([][]byte{metadata, parent.Hash().Bytes()})
	if err != nil {
		return common.Hash{}, err
	}
	projectionPut, err := n.consensus.projectionPut(chain, block, prepared.Requests)
	if err != nil {
		return common.Hash{}, err
	}
	requestRecordPut, err := executionRequestRecordPut(block.Hash(), prepared.Record)
	if err != nil {
		return common.Hash{}, err
	}
	unsafeReasons := []string{taintControlStateOverride}
	if prepared.Controlled {
		unsafeReasons = append(unsafeReasons, taintExecutionRequestControl)
	}
	safety := blockSafetyForChild(parentSafety, block.Hash(), unsafeReasons...)
	timeline.CurrentSlot, timeline.LastProcessedSlot = targetSlot, targetSlot
	sessionSafety = taintStoredSession(sessionSafety, safety, unsafeReasons...)
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
			{Key: append(append([]byte(nil), controlNamespace...), block.Hash().Bytes()...), Value: controlRecord},
			timelineMutation, blockSlotPut(block.Hash(), targetSlot),
			canonicalSlotPut(targetSlot, block.Hash()), safetyMutation, sessionMutation, projectionPut, requestRecordPut,
		},
	}
	_, committedRoot, err := candidate.commit(state, block.NumberU64(), chain.config.Rules(block.Number(), true, block.Time()))
	if err != nil {
		return common.Hash{}, err
	}
	if committedRoot != block.Root() {
		return common.Hash{}, fmt.Errorf("control state root %s does not match block %s", committedRoot, block.Root())
	}
	statePuts, err := candidate.puts()
	if err != nil {
		return common.Hash{}, err
	}
	operation.ExecutionPuts, operation.RollbackDeletes, err = n.planExecutionPuts(chain, statePuts)
	if err != nil {
		return common.Hash{}, err
	}
	if prepared.Controlled {
		queueMutation, err := executionRequestQueuePut(prepared.Remaining)
		if err != nil {
			return common.Hash{}, err
		}
		operation.Puts = append(operation.Puts, queueMutation)
	}
	events := []Event{{Type: "control_block", Slot: targetSlot, BlockHash: block.Hash(), BlockNumber: block.NumberU64()}}
	if finalized := n.finalizedEventBetween(targetSlot-1, targetSlot); finalized != nil {
		events = append(events, *finalized)
	}
	if err := n.commitPrepared(chain, operation, events, func() error {
		if err := writeJournalPuts(chain, operation.ExecutionPuts); err != nil {
			return err
		}
		if err := persistBuiltBlock(chain, block, receipts); err != nil {
			return err
		}
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
		for _, reason := range sessionSafety.Reasons {
			chain.taintReasons[reason] = struct{}{}
		}
		chain.mu.Unlock()
		applyProjectionIndex(chain, block.Hash(), projectionPut)
		n.pendingExecutionRequests = prepared.Remaining
	}); err != nil {
		return common.Hash{}, err
	}
	n.pendingWithdrawals = nil
	if err := n.rebuildPendingView(ctx, chain); err != nil {
		n.disableWrites(err)
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
			if change.Balance.Sign() < 0 {
				return errors.New("control balance cannot be negative")
			}
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
		coinbase := block.Coinbase()
		_, initialState, err := newCandidateState(chain.db, parent.Root())
		if err != nil {
			return false, err
		}
		_, _, state, _, err := chain.generateBlock(
			ctx, parent, block.Time(), common.Hash(projection.Root), nil, block.Withdrawals(), false, nil, initialState, &coinbase,
		)
		if err != nil {
			return false, err
		}
		if err := applyAccountChanges(state, changes); err != nil {
			return false, err
		}
		return state.IntermediateRoot(chain.config.Rules(block.Number(), true, block.Time())) == block.Root(), nil
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

// constructionAccessList retains native system accesses before control overrides
// are merged at the post-execution index. Overrides remain permanently tainted.
func constructionAccessList(list *bal.BlockAccessList) *bal.ConstructionBlockAccessList {
	result := bal.NewConstructionBlockAccessList()
	if list == nil {
		return result
	}
	for _, account := range *list {
		address := account.Address
		result.AccountRead(address)
		for _, key := range account.StorageReads {
			result.StorageRead(address, common.Hash(key.Bytes32()))
		}
		for _, slot := range account.StorageChanges {
			for _, v := range slot.SlotChanges {
				result.StorageWrite(v.BlockAccessIndex, address, common.Hash(slot.Slot.Bytes32()), common.Hash(v.PostValue.Bytes32()))
			}
		}
		for _, v := range account.BalanceChanges {
			result.BalanceChange(v.BlockAccessIndex, address, v.PostBalance)
		}
		for _, v := range account.NonceChanges {
			result.NonceChange(address, v.BlockAccessIndex, v.PostNonce)
		}
		for _, v := range account.CodeChanges {
			result.CodeChange(address, v.BlockAccessIndex, v.NewCode)
		}
	}
	return result
}

// A BAL describes individual slots. Expand full storage overrides against the
// parent's trie plus native system writes so clearing old slots is explicit.
func expandStorageReplacements(ctx context.Context, chain *executionChain, parent *types.Block, native *bal.BlockAccessList, changes ControlChanges) (ControlChanges, error) {
	result := cloneControlChanges(changes)
	parentState, err := chain.blockchain.StateAt(parent.Header())
	if err != nil {
		return nil, err
	}
	for address, change := range result {
		if change.Storage == nil {
			continue
		}
		diff := make(map[common.Hash]common.Hash)
		root := parentState.GetStorageRoot(address)
		if root != (common.Hash{}) && root != types.EmptyRootHash {
			storage, err := parentState.Database().OpenStorageTrie(parent.Root(), address, root, nil)
			if err != nil {
				return nil, err
			}
			nodes, err := storage.NodeIterator(nil)
			if err != nil {
				return nil, err
			}
			iter := trie.NewIterator(nodes)
			for iter.Next() {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				key := parentState.Database().TrieDB().Preimage(common.BytesToHash(iter.Key))
				if len(key) != common.HashLength {
					return nil, fmt.Errorf("storage preimage unavailable for control account %s", address)
				}
				diff[common.BytesToHash(key)] = common.Hash{}
			}
			if iter.Err != nil {
				return nil, iter.Err
			}
		}
		if native != nil {
			for _, account := range *native {
				if account.Address == address {
					for _, slot := range account.StorageChanges {
						diff[common.Hash(slot.Slot.Bytes32())] = common.Hash{}
					}
				}
			}
		}
		maps.Copy(diff, *change.Storage)
		if change.StorageDiff != nil {
			maps.Copy(diff, *change.StorageDiff)
		}
		change.Storage = nil
		change.StorageDiff = &diff
		result[address] = change
	}
	return result, nil
}
