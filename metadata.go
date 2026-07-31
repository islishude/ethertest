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
	Hash   common.Hash `json:"hash"`
	Number uint64      `json:"number"`
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
		checkpoints[name] = &chainPoint{hash: stored.Hash, number: stored.Number}
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

func persistCheckpoint(db ethdb.Database, name string, point *chainPoint) error {
	encoded, err := json.Marshal(storedChainPoint{Hash: point.hash, Number: point.number})
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
