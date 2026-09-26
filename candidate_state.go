package ethertest

import (
	"bytes"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

// readThroughStore keeps all writes in memory and falls back to the immutable
// canonical database for parent trie/code reads.
type readThroughStore struct {
	*memorydb.Database
	base ethdb.KeyValueStore
}

func newReadThroughStore(base ethdb.KeyValueStore) *readThroughStore {
	return &readThroughStore{Database: memorydb.New(), base: base}
}

func (db *readThroughStore) Has(key []byte) (bool, error) {
	if exists, err := db.Database.Has(key); err != nil || exists {
		return exists, err
	}
	return db.base.Has(key)
}

func (db *readThroughStore) Get(key []byte) ([]byte, error) {
	if exists, err := db.Database.Has(key); err != nil {
		return nil, err
	} else if exists {
		return db.Database.Get(key)
	}
	return db.base.Get(key)
}

// NewIterator materializes the requested merged range. Trie construction uses
// point reads, but a correct merged iterator keeps the adapter safe for code DB
// and future geth changes.
func (db *readThroughStore) NewIterator(prefix []byte, start []byte) ethdb.Iterator {
	merged := memorydb.New()
	copyRange := func(iterator ethdb.Iterator) bool {
		defer iterator.Release()
		for iterator.Next() {
			if err := merged.Put(bytes.Clone(iterator.Key()), bytes.Clone(iterator.Value())); err != nil {
				return false
			}
		}
		return iterator.Error() == nil
	}
	if !copyRange(db.base.NewIterator(prefix, start)) || !copyRange(db.Database.NewIterator(prefix, start)) {
		return memorydb.New().NewIterator(nil, nil)
	}
	return merged.NewIterator(prefix, start)
}

type candidateState struct {
	store   *readThroughStore
	db      ethdb.Database
	trie    *triedb.Database
	stateDB state.Database
}

func newCandidateState(base ethdb.Database, root common.Hash) (*candidateState, *state.StateDB, error) {
	store := newReadThroughStore(base)
	db := rawdb.NewDatabase(store)
	trieConfig := *triedb.HashDefaults
	trieConfig.Preimages = true
	trieDB := triedb.NewDatabase(db, &trieConfig)
	stateDB := state.NewDatabase(trieDB, state.NewCodeDB(db))
	statedb, err := state.New(root, stateDB)
	if err != nil {
		return nil, nil, fmt.Errorf("open candidate state %s: %w", root, err)
	}
	return &candidateState{store: store, db: db, trie: trieDB, stateDB: stateDB}, statedb, nil
}

func (candidate *candidateState) commit(statedb *state.StateDB, number uint64, rules params.Rules) (*state.StateDB, common.Hash, error) {
	root, err := statedb.Commit(rules, number)
	if err != nil {
		return nil, common.Hash{}, err
	}
	if err := candidate.trie.Commit(root, false); err != nil {
		return nil, common.Hash{}, err
	}
	stable, err := state.New(root, candidate.stateDB)
	if err != nil {
		return nil, common.Hash{}, err
	}
	return stable, root, nil
}

func (candidate *candidateState) puts() ([]journalKV, error) {
	iterator := candidate.store.Database.NewIterator(nil, nil)
	defer iterator.Release()
	var puts []journalKV
	for iterator.Next() {
		puts = append(puts, journalKV{Key: bytes.Clone(iterator.Key()), Value: bytes.Clone(iterator.Value())})
	}
	if err := iterator.Error(); err != nil {
		return nil, err
	}
	return puts, nil
}
