package ethertest

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// syntheticFinalityResolution is ethertest's deterministic slot projection.
// It is not Casper FFG finality.
type syntheticFinalityResolution struct {
	HeadSlot      uint64
	SafeSlot      uint64
	FinalizedSlot uint64
	Safe          *types.Block
	Finalized     *types.Block
}

// FinalityStatus describes ethertest's current synthetic finality projection.
// FinalitySlot is the observer slot from which the one/two-epoch lags are
// calculated. It equals CurrentSlot unless finality is paused.
type FinalityStatus struct {
	Paused               bool        `json:"paused"`
	CurrentSlot          uint64      `json:"current_slot"`
	FinalitySlot         uint64      `json:"finality_slot"`
	SafeSlot             uint64      `json:"safe_slot"`
	SafeBlockHash        common.Hash `json:"safe_block_hash"`
	SafeBlockNumber      uint64      `json:"safe_block_number"`
	FinalizedSlot        uint64      `json:"finalized_slot"`
	FinalizedBlockHash   common.Hash `json:"finalized_block_hash"`
	FinalizedBlockNumber uint64      `json:"finalized_block_number"`
	ConsensusMode        string      `json:"consensus_mode"`
}

func (n *Node) resolveSyntheticFinality(headSlot uint64) syntheticFinalityResolution {
	return n.resolveSyntheticFinalityAt(n.syntheticFinalitySlot(headSlot))
}

func (n *Node) resolveSyntheticFinalityAt(headSlot uint64) syntheticFinalityResolution {
	safeSlot := subtractSlots(headSlot, n.cfg.Chain.SlotsPerEpoch)
	finalizedSlot := subtractSlots(headSlot, 2*n.cfg.Chain.SlotsPerEpoch)
	return syntheticFinalityResolution{
		HeadSlot: headSlot, SafeSlot: safeSlot, FinalizedSlot: finalizedSlot,
		Safe: n.chain.blockAtOrBeforeSlot(safeSlot), Finalized: n.chain.blockAtOrBeforeSlot(finalizedSlot),
	}
}

func (n *Node) syntheticFinalitySlot(headSlot uint64) uint64 {
	n.chain.mu.RLock()
	paused, finalitySlot := n.chain.finalityPaused, n.chain.finalitySlot
	n.chain.mu.RUnlock()
	if paused && finalitySlot < headSlot {
		return finalitySlot
	}
	return headSlot
}

// FinalityStatus returns the active synthetic safe/finalized projection.
func (n *Node) FinalityStatus() FinalityStatus {
	n.chain.mu.RLock()
	paused := n.chain.finalityPaused
	currentSlot := n.chain.slot
	finalitySlot := n.chain.finalitySlot
	n.chain.mu.RUnlock()
	if !paused || currentSlot < finalitySlot {
		finalitySlot = currentSlot
	}
	resolution := n.resolveSyntheticFinalityAt(finalitySlot)
	return FinalityStatus{
		Paused: paused, CurrentSlot: currentSlot, FinalitySlot: resolution.HeadSlot,
		SafeSlot: resolution.SafeSlot, SafeBlockHash: resolution.Safe.Hash(),
		SafeBlockNumber: resolution.Safe.NumberU64(), FinalizedSlot: resolution.FinalizedSlot,
		FinalizedBlockHash: resolution.Finalized.Hash(), FinalizedBlockNumber: resolution.Finalized.NumberU64(),
		ConsensusMode: "synthetic",
	}
}

// PauseFinality freezes synthetic safe/finalized resolution at the current
// slot while block production and missed-slot processing continue.
func (n *Node) PauseFinality(ctx context.Context) error {
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		chain.mu.RLock()
		if chain.finalityPaused {
			chain.mu.RUnlock()
			return nil, nil
		}
		timeline := chain.timeline()
		currentSlot := chain.slot
		chain.mu.RUnlock()
		timeline.FinalityPaused = true
		timeline.FinalitySlot = currentSlot
		mutation, err := timelinePut(timeline)
		if err != nil {
			return nil, err
		}
		resolution := n.resolveSyntheticFinalityAt(currentSlot)
		if err := n.commitAuxiliary(chain, []journalKV{mutation}, nil, nil, func() {
			chain.mu.Lock()
			chain.finalityPaused = true
			chain.finalitySlot = currentSlot
			chain.mu.Unlock()
		}); err != nil {
			return nil, err
		}
		n.logger.Info("synthetic finality paused",
			"event", "finality_paused",
			"slot", currentSlot,
			"safe_slot", resolution.SafeSlot,
			"safe_block_hash", resolution.Safe.Hash().Hex(),
			"finalized_slot", resolution.FinalizedSlot,
			"finalized_block_hash", resolution.Finalized.Hash().Hex(),
		)
		return nil, nil
	})
	return err
}

// ResumeFinality resumes slot-derived finality and immediately catches the
// projection up to the current slot.
func (n *Node) ResumeFinality(ctx context.Context) error {
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		chain.mu.RLock()
		if !chain.finalityPaused {
			chain.mu.RUnlock()
			return nil, nil
		}
		timeline := chain.timeline()
		currentSlot, finalitySlot := chain.slot, chain.finalitySlot
		chain.mu.RUnlock()
		if currentSlot < finalitySlot {
			finalitySlot = currentSlot
		}
		previous := n.resolveSyntheticFinalityAt(finalitySlot)
		next := n.resolveSyntheticFinalityAt(currentSlot)
		timeline.FinalityPaused = false
		timeline.FinalitySlot = 0
		mutation, err := timelinePut(timeline)
		if err != nil {
			return nil, err
		}
		var events []Event
		if finalized := n.finalizedEventForTransition(previous, next); finalized != nil {
			events = append(events, *finalized)
		}
		if err := n.commitAuxiliary(chain, []journalKV{mutation}, nil, events, func() {
			chain.mu.Lock()
			chain.finalityPaused = false
			chain.finalitySlot = 0
			chain.mu.Unlock()
		}); err != nil {
			return nil, err
		}
		n.logger.Info("synthetic finality resumed",
			"event", "finality_resumed",
			"from_slot", finalitySlot,
			"slot", currentSlot,
			"safe_slot", next.SafeSlot,
			"safe_block_hash", next.Safe.Hash().Hex(),
			"finalized_slot", next.FinalizedSlot,
			"finalized_block_hash", next.Finalized.Hash().Hex(),
		)
		return nil, nil
	})
	return err
}

func subtractSlots(slot, lag uint64) uint64 {
	if slot <= lag {
		return 0
	}
	return slot - lag
}

func (n *Node) finalizedEventBetween(fromSlot, toSlot uint64) *Event {
	previous := n.resolveSyntheticFinality(fromSlot)
	next := n.resolveSyntheticFinality(toSlot)
	return n.finalizedEventForTransition(previous, next)
}

func (n *Node) finalizedEventForTransition(previous, next syntheticFinalityResolution) *Event {
	if previous.Finalized.Hash() == next.Finalized.Hash() &&
		previous.FinalizedSlot/n.cfg.Chain.SlotsPerEpoch == next.FinalizedSlot/n.cfg.Chain.SlotsPerEpoch {
		return nil
	}
	return &Event{
		Type: "finalized_checkpoint", Slot: next.FinalizedSlot,
		BlockHash: next.Finalized.Hash(), BlockNumber: next.Finalized.NumberU64(),
	}
}
