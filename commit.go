package ethertest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type commitStage string

const (
	commitStagePrepared  commitStage = "prepared"
	commitStageExecution commitStage = "execution"
	commitStageAuxiliary commitStage = "auxiliary"
)

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
		Blocks: append([]common.Hash(nil), item.blocks...), Tainted: item.tainted,
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
		n.writeErr = err
		return err
	}
	if n.commitHook != nil {
		if err := n.commitHook(commitStagePrepared); err != nil {
			n.writeErr = err
			return fmt.Errorf("failure after recovery journal preparation: %w", err)
		}
	}
	if err := mutate(); err != nil {
		n.writeErr = err
		return fmt.Errorf("execution mutation failed with recovery journal retained: %w", err)
	}
	if n.commitHook != nil {
		if err := n.commitHook(commitStageExecution); err != nil {
			n.writeErr = err
			return fmt.Errorf("failure after execution mutation: %w", err)
		}
	}
	if err := finalizePreparedOperation(chain.db, operation); err != nil {
		n.writeErr = err
		return err
	}
	if n.commitHook != nil {
		if err := n.commitHook(commitStageAuxiliary); err != nil {
			n.writeErr = err
			return fmt.Errorf("failure after auxiliary commit: %w", err)
		}
	}
	apply()
	n.events.apply(plan)
	return nil
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
			n.writeErr = err
			return err
		}
	}
	for _, key := range append(deletes, plan.deletes...) {
		if err := batch.Delete(key); err != nil {
			n.writeErr = err
			return err
		}
	}
	if err := batch.Write(); err != nil {
		n.writeErr = err
		return err
	}
	apply()
	n.events.apply(plan)
	return nil
}
