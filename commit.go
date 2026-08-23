package ethertest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
)

type commitStage string

const (
	commitStagePrepared  commitStage = "prepared"
	commitStageExecution commitStage = "execution"
	commitStageAuxiliary commitStage = "auxiliary"
)

func (n *Node) disableWrites(err error) {
	if err == nil {
		return
	}
	if n.writeErr == nil {
		n.writeErr = err
	}
	n.setMiningMode(miningModeManual)
}

func timelinePut(value storedTimeline) (journalKV, error) {
	encoded, err := json.Marshal(value)
	return journalKV{Key: append([]byte(nil), timelineKey...), Value: encoded}, err
}

func sessionSafetyPut(value storedSessionSafety) (journalKV, error) {
	encoded, err := json.Marshal(value)
	return journalKV{Key: append([]byte(nil), sessionSafetyKey...), Value: encoded}, err
}

func blockSafetyPut(hash common.Hash, value BlockSafety) (journalKV, error) {
	encoded, err := json.Marshal(value)
	return journalKV{Key: hashKey(blockSafetyPrefix, hash), Value: encoded}, err
}

func blockSlotPut(hash common.Hash, slot uint64) journalKV {
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], slot)
	return journalKV{Key: hashKey(blockSlotPrefix, hash), Value: value[:]}
}

func canonicalSlotPut(slot uint64, hash common.Hash) journalKV {
	return journalKV{Key: slotKey(canonicalSlotPrefix, slot), Value: hash.Bytes()}
}

func branchPut(item *branch) (journalKV, error) {
	encoded, err := json.Marshal(storedBranch{
		Name: item.name, Base: item.base, Head: item.head,
		Tainted: item.tainted,
	})
	return journalKV{Key: appendKey(branchNamespace, item.name), Value: encoded}, err
}

func (n *Node) commitPrepared(
	chain *executionChain,
	operation preparedOperation,
	events []Event,
	mutate func() error,
	apply func(),
) error {
	if n.writeErr != nil {
		return fmt.Errorf("node writes are disabled after a persistence failure: %w", n.writeErr)
	}
	exists, err := chain.db.Has(journalKey)
	if err != nil {
		n.disableWrites(err)
		return err
	}
	if exists {
		return errors.New("a prepared operation is already pending")
	}
	plan, err := n.events.plan(events)
	if err != nil {
		return err
	}
	operation.Puts = append(operation.Puts, plan.puts...)
	operation.Deletes = append(operation.Deletes, plan.deletes...)
	if err := writePreparedOperation(chain.db, operation); err != nil {
		n.disableWrites(err)
		return err
	}
	if n.commitHook != nil {
		if err := n.commitHook(commitStagePrepared); err != nil {
			n.disableWrites(err)
			return fmt.Errorf("failure after recovery journal preparation: %w", err)
		}
	}
	if err := mutate(); err != nil {
		n.disableWrites(err)
		return fmt.Errorf("execution mutation failed with recovery journal retained: %w", err)
	}
	if n.commitHook != nil {
		if err := n.commitHook(commitStageExecution); err != nil {
			n.disableWrites(err)
			return fmt.Errorf("failure after execution mutation: %w", err)
		}
	}
	if err := finalizePreparedOperation(chain.db, operation); err != nil {
		n.disableWrites(err)
		return err
	}
	if n.commitHook != nil {
		if err := n.commitHook(commitStageAuxiliary); err != nil {
			n.disableWrites(err)
			return fmt.Errorf("failure after auxiliary commit: %w", err)
		}
	}
	apply()
	n.events.apply(plan)
	return nil
}

func executionPutsForJournal(db interface {
	Has([]byte) (bool, error)
}, puts []journalKV) ([]journalKV, [][]byte, error) {
	rollback := make([][]byte, 0, len(puts))
	for _, item := range puts {
		exists, err := db.Has(item.Key)
		if err != nil {
			return nil, nil, err
		}
		if !exists {
			rollback = append(rollback, append([]byte(nil), item.Key...))
		}
	}
	return puts, rollback, nil
}

func (n *Node) planExecutionPuts(chain *executionChain, puts []journalKV) ([]journalKV, [][]byte, error) {
	executionPuts, rollbackDeletes, err := executionPutsForJournal(chain.db, puts)
	if err != nil {
		n.disableWrites(err)
	}
	return executionPuts, rollbackDeletes, err
}

func writeJournalPuts(chain *executionChain, puts []journalKV) error {
	batch := chain.db.NewBatch()
	for _, item := range puts {
		if err := batch.Put(item.Key, item.Value); err != nil {
			return err
		}
	}
	return batch.Write()
}

// persistBuiltBlock stores the already-executed block and receipts atomically.
// The direct builder has produced and validated the state transition, so this
// path must not invoke geth's processor and execute the transactions again.
func persistBuiltBlock(chain *executionChain, block *types.Block, receipts types.Receipts) error {
	batch := chain.db.NewBatch()
	defer batch.Close()
	rawdb.WriteBlock(batch, block)
	rawdb.WriteReceipts(batch, block.Hash(), block.NumberU64(), receipts)
	return batch.Write()
}

func (n *Node) commitAuxiliary(
	chain *executionChain,
	puts []journalKV,
	deletes [][]byte,
	events []Event,
	apply func(),
) error {
	if n.writeErr != nil {
		return fmt.Errorf("node writes are disabled after a persistence failure: %w", n.writeErr)
	}
	plan, err := n.events.plan(events)
	if err != nil {
		return err
	}
	batch := chain.db.NewBatch()
	for _, item := range append(puts, plan.puts...) {
		if err := batch.Put(item.Key, item.Value); err != nil {
			n.disableWrites(err)
			return err
		}
	}
	for _, key := range append(deletes, plan.deletes...) {
		if err := batch.Delete(key); err != nil {
			n.disableWrites(err)
			return err
		}
	}
	if err := batch.Write(); err != nil {
		n.disableWrites(err)
		return err
	}
	apply()
	n.events.apply(plan)
	return nil
}
