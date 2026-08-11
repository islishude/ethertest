package ethertest

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type withdrawalArgs struct {
	Index          json.RawMessage `json:"index"`
	ValidatorIndex *hexutil.Uint64 `json:"validatorIndex"`
	Address        *common.Address `json:"address"`
	Amount         *hexutil.Uint64 `json:"amount"`
}

func (api *withdrawalAPI) AddWithdrawal(ctx context.Context, args withdrawalArgs) (bool, error) {
	if args.Index != nil {
		return false, &invalidParamsError{message: "withdrawal index is assigned automatically"}
	}
	if args.ValidatorIndex == nil {
		return false, &invalidParamsError{message: "withdrawal validatorIndex is required"}
	}
	if args.Address == nil {
		return false, &invalidParamsError{message: "withdrawal address is required"}
	}
	if args.Amount == nil {
		return false, &invalidParamsError{message: "withdrawal amount is required"}
	}
	request := WithdrawalRequest{
		ValidatorIndex: uint64(*args.ValidatorIndex), Address: *args.Address, Amount: uint64(*args.Amount),
	}
	if err := api.node.AddWithdrawal(ctx, request); err != nil {
		if errors.Is(err, ErrWithdrawalAmountZero) || errors.Is(err, ErrWithdrawalQueueFull) || errors.Is(err, ErrWithdrawalIndexOverflow) {
			return false, &invalidParamsError{message: err.Error()}
		}
		return false, err
	}
	return true, nil
}
