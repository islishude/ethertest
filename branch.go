package ethertest

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
)

func (n *Node) CreateBranch(ctx context.Context, name string, blockNumber uint64) error {
	if name == "" {
		return errors.New("branch name is required")
	}
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		if _, exists := n.branches[name]; exists {
			return nil, fmt.Errorf("branch %q already exists", name)
		}
		base := chain.blockchain.GetBlockByNumber(blockNumber)
		if base == nil {
			return nil, errors.New("branch base not found")
		}
		head := chain.blockchain.CurrentBlock()
		finality := n.resolveSyntheticFinality(chain.currentSlot())
		if base.NumberU64() < finality.Finalized.NumberU64() && base.Hash() != head.Hash() {
			return nil, errors.New("branch base is finalized; use reset or restore")
		}
		chain.mu.RLock()
		baseSafety := chain.blockSafety[base.Hash()]
		chain.mu.RUnlock()
		item := &branch{name: name, base: base.Hash(), head: base.Hash(), tainted: baseSafety.Tainted}
		if err := persistBranch(chain.db, item); err != nil {
			return nil, err
		}
		n.branches[name] = item
		n.logger.Info("branch created",
			"event", "branch_created",
			"name", name,
			"base_number", base.NumberU64(),
			"base_hash", base.Hash().Hex(),
		)
		return nil, nil
	})
	return err
}

func (n *Node) MineBranch(ctx context.Context, name string, count uint64) ([]common.Hash, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		item := n.branches[name]
		if item == nil {
			return nil, fmt.Errorf("branch %q not found", name)
		}
		hashes := make([]common.Hash, 0, count)
		for range count {
			parent := chain.blockchain.GetBlockByHash(item.head)
			if parent == nil {
				return nil, errors.New("branch head not found")
			}
			projection, err := n.consensus.ensureProjection(chain, parent)
			if err != nil {
				return nil, err
			}
			targetTime := parent.Time() + uint64(n.cfg.Chain.SlotDuration.Seconds())
			blocks, _ := core.GenerateChain(chain.config, parent, chain.blockchain.Engine(), chain.db, 1, func(_ int, gen *core.BlockGen) {
				gen.OffsetTime(int64(targetTime) - int64(gen.Timestamp()))
				gen.SetPoS()
				gen.SetCoinbase(chain.feeRecipientAddress())
				gen.SetParentBeaconRoot(common.Hash(projection.Root))
				gen.SetExtra([]byte("ethertest-branch:" + name))
			})
			block := blocks[0]
			slot := uint64(0)
			if block.Time() > chain.genesisTime {
				slot = (block.Time() - chain.genesisTime) / chain.slotDuration
			}
			projectionPut, err := n.consensus.projectionPut(chain, block)
			if err != nil {
				return nil, err
			}
			chain.mu.RLock()
			parentSafety := chain.blockSafety[parent.Hash()]
			chain.mu.RUnlock()
			safety := blockSafetyForChild(parentSafety, block.Hash(), "")
			updated := &branch{
				name: item.name, base: item.base, head: block.Hash(),
				blocks:  append(append([]common.Hash(nil), item.blocks...), block.Hash()),
				tainted: safety.Tainted,
			}
			branchMutation, err := branchPut(updated)
			if err != nil {
				return nil, err
			}
			safetyMutation, err := blockSafetyPut(block.Hash(), safety)
			if err != nil {
				return nil, err
			}
			operation := preparedOperation{
				Kind: "block", TargetBlock: block.Hash(),
				Puts: []journalKV{blockSlotPut(block.Hash(), slot), safetyMutation, projectionPut, branchMutation},
			}
			if err := n.commitPrepared(chain, operation, nil, func() error {
				_, insertErr := chain.blockchain.InsertBlockWithoutSetHead(ctx, block, false)
				return insertErr
			}, func() {
				chain.mu.Lock()
				chain.slotByHash[block.Hash()] = slot
				chain.blockSafety[block.Hash()] = safety
				chain.mu.Unlock()
				item.head = updated.head
				item.blocks = updated.blocks
				item.tainted = updated.tainted
			}); err != nil {
				return nil, err
			}
			hashes = append(hashes, block.Hash())
		}
		if len(hashes) != 0 {
			n.logger.Info("branch blocks mined",
				"event", "branch_blocks_mined",
				"name", name,
				"blocks", len(hashes),
				"head_hash", hashes[len(hashes)-1].Hex(),
			)
		}
		return hashes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]common.Hash), nil
}

func (n *Node) SwitchBranch(ctx context.Context, name string) error {
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		item := n.branches[name]
		if item == nil {
			return nil, fmt.Errorf("branch %q not found", name)
		}
		target := chain.blockchain.GetBlockByHash(item.head)
		if target == nil {
			return nil, errors.New("branch head not found")
		}
		oldHead := chain.blockchain.GetBlockByHash(chain.blockchain.CurrentBlock().Hash())
		oldPath, _ := divergentPaths(chain, oldHead, target)
		commonAncestor := oldHead
		for range oldPath {
			commonAncestor = chain.blockchain.GetBlockByHash(commonAncestor.ParentHash())
		}
		finalized := n.resolveSyntheticFinality(chain.currentSlot()).Finalized
		if commonAncestor.NumberU64() < finalized.NumberU64() {
			return nil, errors.New("branch switch would replace finalized history; use reset or restore")
		}
		if err := n.switchCanonical(chain, target, chain.slotOf(target)); err != nil {
			return nil, err
		}
		n.logger.Info("branch switched",
			"event", "branch_switched",
			"name", name,
			"block_number", target.NumberU64(),
			"block_hash", target.Hash().Hex(),
		)
		return nil, nil
	})
	return err
}

func (n *Node) switchCanonical(chain *executionChain, target *types.Block, targetSlot uint64) error {
	oldHead := chain.blockchain.GetBlockByHash(chain.blockchain.CurrentBlock().Hash())
	if oldHead.Hash() == target.Hash() && chain.currentSlot() == targetSlot {
		return nil
	}
	oldPath, newPath := divergentPaths(chain, oldHead, target)
	commonAncestor := oldHead
	for range oldPath {
		commonAncestor = chain.blockchain.GetBlockByHash(commonAncestor.ParentHash())
	}
	chain.mu.RLock()
	timeline := chain.timeline()
	canonicalSlots := make(map[uint64]common.Hash, len(chain.canonicalBlockBySlot))
	for slot, hash := range chain.canonicalBlockBySlot {
		canonicalSlots[slot] = hash
	}
	chain.mu.RUnlock()
	timeline.CurrentSlot, timeline.LastProcessedSlot = targetSlot, targetSlot
	timelineMutation, err := timelinePut(timeline)
	if err != nil {
		return err
	}
	operation := preparedOperation{
		Kind: "head", OldHead: oldHead.Hash(), NewHead: target.Hash(),
		Puts: []journalKV{timelineMutation},
	}
	for slot, hash := range canonicalSlots {
		if slot > targetSlot {
			operation.Deletes = append(operation.Deletes, slotKey(canonicalSlotPrefix, slot))
			delete(canonicalSlots, slot)
			continue
		}
		for _, block := range oldPath {
			if hash == block.Hash() {
				operation.Deletes = append(operation.Deletes, slotKey(canonicalSlotPrefix, slot))
				delete(canonicalSlots, slot)
				break
			}
		}
	}
	for _, block := range slices.Backward(newPath) {
		slot := chain.slotOfUnlocked(block)
		canonicalSlots[slot] = block.Hash()
		operation.Puts = append(operation.Puts, canonicalSlotPut(slot, block.Hash()))
	}
	events := make([]Event, 0, len(oldPath)+len(newPath))
	for _, block := range oldPath {
		events = append(events, Event{
			Type: "block", Slot: chain.slotOf(block), BlockHash: block.Hash(),
			BlockNumber: block.NumberU64(), Removed: true,
		})
	}
	for _, block := range slices.Backward(newPath) {
		events = append(events, Event{
			Type: "block", Slot: chain.slotOf(block), BlockHash: block.Hash(), BlockNumber: block.NumberU64(),
		})
	}
	if oldHead.Hash() != target.Hash() {
		events = append(events, Event{
			Type: "chain_reorg", Slot: targetSlot, OldHead: oldHead.Hash(), NewHead: target.Hash(),
			Depth: uint64(len(oldPath)),
		})
	}
	if err := n.commitPrepared(chain, operation, events, func() error {
		_, setErr := chain.blockchain.SetCanonical(target)
		return setErr
	}, func() {
		chain.mu.Lock()
		chain.slot, chain.lastProcessedSlot = targetSlot, targetSlot
		chain.canonicalBlockBySlot = canonicalSlots
		chain.mu.Unlock()
	}); err != nil {
		return err
	}
	if err := n.rebuildPendingView(chain); err != nil {
		n.writeErr = err
		return fmt.Errorf("canonical switch committed but pending view rebuild failed: %w", err)
	}
	n.logger.Info("canonical chain reorganized",
		"event", "chain_reorganized",
		"old_head_number", oldHead.NumberU64(),
		"old_head_hash", oldHead.Hash().Hex(),
		"new_head_number", target.NumberU64(),
		"new_head_hash", target.Hash().Hex(),
		"common_ancestor_number", commonAncestor.NumberU64(),
		"common_ancestor_hash", commonAncestor.Hash().Hex(),
		"removed_blocks", len(oldPath),
		"added_blocks", len(newPath),
	)
	return nil
}

func divergentPaths(chain *executionChain, oldHead, newHead *types.Block) (oldPath, newPath []*types.Block) {
	oldBlock, newBlock := oldHead, newHead
	for oldBlock.NumberU64() > newBlock.NumberU64() {
		oldPath = append(oldPath, oldBlock)
		oldBlock = chain.blockchain.GetBlockByHash(oldBlock.ParentHash())
	}
	for newBlock.NumberU64() > oldBlock.NumberU64() {
		newPath = append(newPath, newBlock)
		newBlock = chain.blockchain.GetBlockByHash(newBlock.ParentHash())
	}
	for oldBlock.Hash() != newBlock.Hash() {
		oldPath = append(oldPath, oldBlock)
		newPath = append(newPath, newBlock)
		oldBlock = chain.blockchain.GetBlockByHash(oldBlock.ParentHash())
		newBlock = chain.blockchain.GetBlockByHash(newBlock.ParentHash())
	}
	return oldPath, newPath
}
