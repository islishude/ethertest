package ethertest

import "github.com/ethereum/go-ethereum/core/types"

// syntheticFinalityResolution is ethertest's deterministic slot projection.
// It is not Casper FFG finality.
type syntheticFinalityResolution struct {
	HeadSlot      uint64
	SafeSlot      uint64
	FinalizedSlot uint64
	Safe          *types.Block
	Finalized     *types.Block
}

func (n *Node) resolveSyntheticFinality(headSlot uint64) syntheticFinalityResolution {
	safeSlot := subtractSlots(headSlot, n.cfg.Chain.SlotsPerEpoch)
	finalizedSlot := subtractSlots(headSlot, 2*n.cfg.Chain.SlotsPerEpoch)
	return syntheticFinalityResolution{
		HeadSlot: headSlot, SafeSlot: safeSlot, FinalizedSlot: finalizedSlot,
		Safe: n.chain.blockAtOrBeforeSlot(safeSlot), Finalized: n.chain.blockAtOrBeforeSlot(finalizedSlot),
	}
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
	if previous.Finalized.Hash() == next.Finalized.Hash() &&
		previous.FinalizedSlot/n.cfg.Chain.SlotsPerEpoch == next.FinalizedSlot/n.cfg.Chain.SlotsPerEpoch {
		return nil
	}
	return &Event{
		Type: "finalized_checkpoint", Slot: next.FinalizedSlot,
		BlockHash: next.Finalized.Hash(), BlockNumber: next.Finalized.NumberU64(),
	}
}
