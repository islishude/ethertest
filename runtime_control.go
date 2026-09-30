package ethertest

import (
	"context"

	"math"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// SetFeeRecipient changes the runtime beneficiary and the pending candidate.
func (n *Node) SetFeeRecipient(ctx context.Context, address common.Address) error {
	_, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		previous := chain.feeRecipientAddress()
		chain.setFeeRecipient(address)
		if err := n.rebuildPendingView(ctx, chain); err != nil {
			chain.setFeeRecipient(previous)
			return nil, err
		}
		n.logger.Info("fee recipient changed",
			"event", "fee_recipient_changed",
			"address", address.Hex(),
		)
		return nil, nil
	})
	return err
}

// Automine reports whether accepted transactions trigger immediate mining.
func (n *Node) Automine() bool { return n.currentMiningMode() == "transaction" }

// IntervalMining returns the active runtime interval, or zero when disabled.
func (n *Node) IntervalMining() time.Duration {
	n.miningMu.RLock()
	defer n.miningMu.RUnlock()
	if n.miningMode != "interval" {
		return 0
	}
	return n.miningInterval
}

// SetAutomine changes runtime mining without changing the slot timeline.
// Already executable transactions are mined before enabling automine. If a
// later block fails, earlier committed blocks remain, as with Mine.
func (n *Node) SetAutomine(ctx context.Context, enabled bool) error {
	_, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		if !enabled {
			if n.currentMiningMode() == "transaction" {
				n.setMiningMode(miningModeManual)
			}
			return nil, nil
		}
		for {
			chain.mu.RLock()
			ready := chain.pendingView != nil && len(chain.pendingView.executable) > 0
			chain.mu.RUnlock()
			if !ready {
				break
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			block, _, err := n.mineExecutionBlock(ctx, chain, false)
			if err != nil {
				return nil, err
			}
			n.recordAutomaticBlock(block, "transaction")
		}
		n.setMiningMode("transaction")
		return nil, nil
	})
	return err
}

func (n *Node) setIntervalMiningSeconds(ctx context.Context, seconds uint64) error {
	if seconds > uint64(math.MaxInt64/int64(time.Second)) {
		return &invalidParamsError{message: "mining interval overflows duration"}
	}
	return n.SetIntervalMining(ctx, time.Duration(seconds)*time.Second)
}

// SetIntervalMining selects runtime interval mining, including empty blocks.
// Zero selects manual mode. Block timestamps still advance by slot duration.
func (n *Node) SetIntervalMining(ctx context.Context, interval time.Duration) error {
	if interval < 0 || interval%time.Second != 0 {
		return &invalidParamsError{message: "mining interval must be whole non-negative seconds"}
	}
	_, err := n.executeWrite(ctx, func(_ *executionChain) (any, error) {
		if interval == 0 {
			n.setMiningMode(miningModeManual)
			return nil, nil
		}
		n.miningMu.Lock()
		n.miningInterval = interval
		n.miningEmpty = true
		n.miningMu.Unlock()
		n.setMiningMode("interval")
		return nil, nil
	})
	return err
}

func (n *Node) mineCompatible(ctx context.Context, count uint64, timestamp, interval *uint64) error {
	if count > n.cfg.Limits.MaxControlOperations {
		return newResourceLimitError("block count", count, n.cfg.Limits.MaxControlOperations)
	}
	_, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		if interval != nil && *interval != chain.slotDuration {
			return nil, &invalidParamsError{message: "interval must equal slot duration"}
		}
		slot := chain.currentSlot()
		maxSlot := (math.MaxUint64 - chain.genesisTime) / chain.slotDuration
		if slot > maxSlot || count > maxSlot-slot {
			return nil, &invalidParamsError{message: "mining range overflows timestamp"}
		}
		if count == 0 && timestamp == nil {
			return nil, nil
		}
		if slot == math.MaxUint64 || slot+1 > (math.MaxUint64-chain.genesisTime)/chain.slotDuration {
			return nil, &invalidParamsError{message: "next slot timestamp overflows"}
		}
		next := chain.genesisTime + (slot+1)*chain.slotDuration
		if timestamp != nil && *timestamp != next {
			return nil, &invalidParamsError{message: "timestamp must equal the next slot timestamp"}
		}
		return n.mineBlocks(ctx, chain, count, false)
	})
	return err
}

// DropTransaction removes only a currently pooled transaction.
func (n *Node) DropTransaction(ctx context.Context, hash common.Hash) (bool, error) {
	count, err := n.removePoolTransactions(ctx, &hash, nil)
	return count != 0, err
}

// DropAllTransactions clears the pending pool, retaining canonical blob data.
func (n *Node) DropAllTransactions(ctx context.Context) error {
	_, err := n.removePoolTransactions(ctx, nil, nil)
	return err
}

// RemovePoolTransactions removes all pending transactions of an account.
func (n *Node) RemovePoolTransactions(ctx context.Context, address common.Address) error {
	_, err := n.removePoolTransactions(ctx, nil, &address)
	return err
}

func (n *Node) removePoolTransactions(ctx context.Context, hash *common.Hash, address *common.Address) (int, error) {
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		snapshot := chain.snapshotTransactionPool()
		count := 0
		release := make(map[common.Hash]bool)
		for sender, txs := range snapshot.pending {
			if address != nil && sender != *address {
				continue
			}
			for _, tx := range txs {
				if hash != nil && tx.Hash() != *hash {
					continue
				}
				retained, err := chain.db.Has(append(append([]byte(nil), blobNamespace...), tx.Hash().Bytes()...))
				if err != nil {
					return 0, err
				}
				release[tx.Hash()] = !retained
			}
		}
		chain.mu.Lock()
		for sender, txs := range chain.pending {
			if address != nil && sender != *address {
				continue
			}
			for nonce, tx := range txs {
				if hash != nil && tx.Hash() != *hash {
					continue
				}
				delete(txs, nonce)
				delete(chain.arrival, tx.Hash())
				// chain.blobs also caches retained data: only release an unpersisted bundle.
				if release[tx.Hash()] {
					delete(chain.blobs, tx.Hash())
				}
				count++
			}
			if len(txs) == 0 {
				delete(chain.pending, sender)
			}
		}
		chain.mu.Unlock()
		if count == 0 {
			return 0, nil
		}
		if err := n.rebuildPendingView(ctx, chain); err != nil {
			chain.restoreTransactionPool(snapshot)
			return 0, err
		}
		return count, nil
	})
	if err != nil {
		return 0, err
	}
	return value.(int), nil
}
