package ethertest

import (
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
)

var (
	checkpointNamespace = []byte("ethertest/checkpoint/")
	branchNamespace     = []byte("ethertest/branch/")
)

type storedChainPoint struct {
	Hash    common.Hash `json:"hash"`
	Number  uint64      `json:"number"`
	Slot    uint64      `json:"slot"`
	Tainted bool        `json:"tainted"`
}

type storedBranch struct {
	Name    string        `json:"name"`
	Base    common.Hash   `json:"base"`
	Head    common.Hash   `json:"head"`
	Blocks  []common.Hash `json:"blocks"`
	Tainted bool          `json:"tainted"`
}

func loadControlMetadata(db ethdb.Database) (map[string]*chainPoint, map[string]*branch, error) {
	checkpoints := make(map[string]*chainPoint)
	iterator := db.NewIterator(checkpointNamespace, nil)
	for iterator.Next() {
		var stored storedChainPoint
		if err := json.Unmarshal(iterator.Value(), &stored); err != nil {
			iterator.Release()
			return nil, nil, fmt.Errorf("decode checkpoint metadata: %w", err)
		}
		name := string(iterator.Key()[len(checkpointNamespace):])
		if name == "" {
			iterator.Release()
			return nil, nil, fmt.Errorf("decode checkpoint metadata: empty name")
		}
		checkpoints[name] = &chainPoint{
			hash: stored.Hash, number: stored.Number, slot: stored.Slot, tainted: stored.Tainted,
		}
	}
	if err := iterator.Error(); err != nil {
		iterator.Release()
		return nil, nil, err
	}
	iterator.Release()

	branches := make(map[string]*branch)
	iterator = db.NewIterator(branchNamespace, nil)
	for iterator.Next() {
		var stored storedBranch
		if err := json.Unmarshal(iterator.Value(), &stored); err != nil {
			iterator.Release()
			return nil, nil, fmt.Errorf("decode branch metadata: %w", err)
		}
		name := string(iterator.Key()[len(branchNamespace):])
		if name == "" || stored.Name != name {
			iterator.Release()
			return nil, nil, fmt.Errorf("decode branch metadata: inconsistent name %q", name)
		}
		branches[name] = &branch{
			name: stored.Name, base: stored.Base, head: stored.Head,
			blocks: append([]common.Hash(nil), stored.Blocks...), tainted: stored.Tainted,
		}
	}
	if err := iterator.Error(); err != nil {
		iterator.Release()
		return nil, nil, err
	}
	iterator.Release()
	return checkpoints, branches, nil
}

func validateControlMetadata(chain *executionChain, checkpoints map[string]*chainPoint, branches map[string]*branch) error {
	chain.mu.RLock()
	defer chain.mu.RUnlock()
	for name, point := range checkpoints {
		if point == nil {
			return fmt.Errorf("checkpoint %q has no metadata", name)
		}
		block := chain.blockchain.GetBlock(point.hash, point.number)
		if block == nil {
			return fmt.Errorf("checkpoint %q references missing execution block %s", name, point.hash)
		}
		slot, exists := chain.slotByHash[point.hash]
		if !exists || point.slot < slot {
			return fmt.Errorf("checkpoint %q has inconsistent slot metadata", name)
		}
		safety, exists := chain.blockSafety[point.hash]
		if !exists || safety.Tainted != point.tainted {
			return fmt.Errorf("checkpoint %q has inconsistent safety metadata", name)
		}
	}
	for name, item := range branches {
		if item == nil {
			return fmt.Errorf("branch %q has no metadata", name)
		}
		base := chain.blockchain.GetBlockByHash(item.base)
		if base == nil {
			return fmt.Errorf("branch %q references missing base block %s", name, item.base)
		}
		head := chain.blockchain.GetBlockByHash(item.head)
		if head == nil {
			return fmt.Errorf("branch %q references missing head block %s", name, item.head)
		}
		if len(item.blocks) == 0 {
			if item.head != item.base {
				return fmt.Errorf("branch %q has no blocks but its head differs from its base", name)
			}
		} else if item.blocks[len(item.blocks)-1] != item.head {
			return fmt.Errorf("branch %q head does not match its final block", name)
		}
		parent := base
		parentSlot := chain.slotByHash[parent.Hash()]
		seen := make(map[common.Hash]struct{}, len(item.blocks))
		for index, hash := range item.blocks {
			if _, duplicate := seen[hash]; duplicate {
				return fmt.Errorf("branch %q repeats block %s", name, hash)
			}
			seen[hash] = struct{}{}
			block := chain.blockchain.GetBlockByHash(hash)
			if block == nil {
				return fmt.Errorf("branch %q references missing block %s", name, hash)
			}
			if block.ParentHash() != parent.Hash() || block.NumberU64() != parent.NumberU64()+1 {
				return fmt.Errorf("branch %q block %d does not extend its recorded parent", name, index)
			}
			slot, exists := chain.slotByHash[hash]
			if !exists || slot <= parentSlot {
				return fmt.Errorf("branch %q block %s has inconsistent slot metadata", name, hash)
			}
			parent, parentSlot = block, slot
		}
		safety, exists := chain.blockSafety[item.head]
		if !exists || safety.Tainted != item.tainted {
			return fmt.Errorf("branch %q has inconsistent safety metadata", name)
		}
	}
	return nil
}

func persistCheckpoint(db ethdb.Database, name string, point *chainPoint) error {
	encoded, err := json.Marshal(storedChainPoint{
		Hash: point.hash, Number: point.number, Slot: point.slot, Tainted: point.tainted,
	})
	if err != nil {
		return err
	}
	return db.Put(appendKey(checkpointNamespace, name), encoded)
}

func persistBranch(db ethdb.Database, item *branch) error {
	encoded, err := json.Marshal(storedBranch{
		Name: item.name, Base: item.base, Head: item.head,
		Blocks: item.blocks, Tainted: item.tainted,
	})
	if err != nil {
		return err
	}
	return db.Put(appendKey(branchNamespace, item.name), encoded)
}

func appendKey(prefix []byte, suffix string) []byte {
	key := make([]byte, len(prefix)+len(suffix))
	copy(key, prefix)
	copy(key[len(prefix):], suffix)
	return key
}
