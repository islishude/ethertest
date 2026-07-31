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
		finalizedSlot := uint64(0)
		if currentSlot := chain.currentSlot(); currentSlot > 2*n.cfg.Chain.SlotsPerEpoch {
			finalizedSlot = currentSlot - 2*n.cfg.Chain.SlotsPerEpoch
		}
		if chain.slotOf(base) <= finalizedSlot && base.Hash() != head.Hash() {
			return nil, errors.New("branch base is finalized; use reset or restore")
		}
		item := &branch{name: name, base: base.Hash(), head: base.Hash()}
		if err := persistBranch(chain.db, item); err != nil {
			return nil, err
		}
		n.branches[name] = item
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
		chain.mu.Lock()
		defer chain.mu.Unlock()
		hashes := make([]common.Hash, 0, count)
		for range count {
			parent := chain.blockchain.GetBlockByHash(item.head)
			if parent == nil {
				return nil, errors.New("branch head not found")
			}
			targetTime := parent.Time() + uint64(n.cfg.Chain.SlotDuration.Seconds())
			blocks, _ := core.GenerateChain(chain.config, parent, chain.blockchain.Engine(), chain.db, 1, func(_ int, gen *core.BlockGen) {
				gen.OffsetTime(int64(targetTime) - int64(gen.Timestamp()))
				gen.SetPoS()
				gen.SetCoinbase(chain.feeRecipient)
				gen.SetParentBeaconRoot(parent.Hash())
				gen.SetExtra([]byte("ethertest-branch:" + name))
			})
			block := blocks[0]
			if _, err := chain.blockchain.InsertBlockWithoutSetHead(ctx, block, false); err != nil {
				return nil, err
			}
			item.head = block.Hash()
			item.blocks = append(item.blocks, block.Hash())
			slot := uint64(0)
			if block.Time() > chain.genesisTime {
				slot = (block.Time() - chain.genesisTime) / chain.slotDuration
			}
			chain.slotByHash[block.Hash()] = slot
			chain.blockBySlot[slot] = block.Hash()
			hashes = append(hashes, block.Hash())
		}
		if err := persistBranch(chain.db, item); err != nil {
			return nil, err
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
		return nil, n.switchCanonical(chain, target)
	})
	return err
}

func (n *Node) switchCanonical(chain *executionChain, target *types.Block) error {
	oldHead := chain.blockchain.GetBlockByHash(chain.blockchain.CurrentBlock().Hash())
	if oldHead.Hash() == target.Hash() {
		return nil
	}
	oldPath, newPath := divergentPaths(chain, oldHead, target)
	if _, err := chain.blockchain.SetCanonical(target); err != nil {
		return err
	}
	targetSlot := chain.slotOf(target)
	chain.mu.Lock()
	chain.slot = targetSlot
	for slot := range chain.blockBySlot {
		if slot > targetSlot {
			delete(chain.blockBySlot, slot)
		}
	}
	for _, block := range slices.Backward(newPath) {

		slot := chain.slotOfUnlocked(block)
		chain.slotByHash[block.Hash()] = slot
		chain.blockBySlot[slot] = block.Hash()
	}
	chain.mu.Unlock()
	for _, block := range oldPath {
		if _, err := n.events.append(Event{
			Type: "block", BlockHash: block.Hash(), BlockNumber: block.NumberU64(), Removed: true,
		}); err != nil {
			return err
		}
	}
	for _, block := range slices.Backward(newPath) {

		if _, err := n.events.append(Event{Type: "block", BlockHash: block.Hash(), BlockNumber: block.NumberU64()}); err != nil {
			return err
		}
	}
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
