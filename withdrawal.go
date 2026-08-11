package ethertest

import (
	"context"
	"errors"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
)

const maxWithdrawalsPerBlock = 16

var (
	ErrWithdrawalAmountZero    = errors.New("withdrawal amount must be greater than zero")
	ErrWithdrawalQueueFull     = errors.New("next block already has 16 withdrawals")
	ErrWithdrawalIndexOverflow = errors.New("withdrawal index overflow")
)

// WithdrawalRequest describes a withdrawal to include in the next canonical
// block. Amount is denominated in Gwei. The globally monotonic withdrawal
// index is assigned by the node when it builds the pending candidate.
type WithdrawalRequest struct {
	ValidatorIndex uint64
	Address        common.Address
	Amount         uint64
}

// AddWithdrawal queues a withdrawal for the next canonical block. It updates
// the pending candidate without triggering block production.
func (n *Node) AddWithdrawal(ctx context.Context, request WithdrawalRequest) error {
	if request.Amount == 0 {
		return ErrWithdrawalAmountZero
	}
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		if len(n.pendingWithdrawals) >= maxWithdrawalsPerBlock {
			return nil, ErrWithdrawalQueueFull
		}
		n.pendingWithdrawals = append(n.pendingWithdrawals, request)
		if err := n.rebuildPendingView(chain); err != nil {
			n.pendingWithdrawals = n.pendingWithdrawals[:len(n.pendingWithdrawals)-1]
			return nil, err
		}
		return nil, nil
	})
	return err
}

func assignedWithdrawals(chain *core.BlockChain, parent *types.Block, requests []WithdrawalRequest) (types.Withdrawals, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	next, err := nextWithdrawalIndex(chain, parent)
	if err != nil {
		return nil, err
	}
	if uint64(len(requests)-1) > math.MaxUint64-next {
		return nil, ErrWithdrawalIndexOverflow
	}
	withdrawals := make(types.Withdrawals, len(requests))
	for index, request := range requests {
		withdrawals[index] = &types.Withdrawal{
			Index: next + uint64(index), Validator: request.ValidatorIndex,
			Address: request.Address, Amount: request.Amount,
		}
	}
	return withdrawals, nil
}

func nextWithdrawalIndex(chain *core.BlockChain, parent *types.Block) (uint64, error) {
	for block := parent; block != nil; {
		if withdrawals := block.Withdrawals(); len(withdrawals) != 0 {
			last := withdrawals[len(withdrawals)-1].Index
			if last == math.MaxUint64 {
				return 0, ErrWithdrawalIndexOverflow
			}
			return last + 1, nil
		}
		if block.NumberU64() == 0 {
			break
		}
		block = chain.GetBlock(block.ParentHash(), block.NumberU64()-1)
	}
	return 0, nil
}

func addWithdrawals(generator *core.BlockGen, withdrawals types.Withdrawals) {
	for _, withdrawal := range withdrawals {
		generator.AddWithdrawal(withdrawal)
	}
}

// BlockGen assigns indices using only its local generation window. Rebuild the
// body with ethertest's canonical-chain-derived indices while preserving the
// state transition that BlockGen already applied for the same recipients and
// amounts.
func replaceGeneratedWithdrawals(block *types.Block, receipts types.Receipts, withdrawals types.Withdrawals) *types.Block {
	if len(withdrawals) == 0 {
		return block
	}
	body := block.Body()
	body.Withdrawals = withdrawals
	return types.NewBlock(block.Header(), body, receipts, trie.NewStackTrie(nil))
}
