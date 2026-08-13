package ethertest

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type depositRequestArgs struct {
	Pubkey                *hexutil.Bytes  `json:"pubkey"`
	WithdrawalCredentials *hexutil.Bytes  `json:"withdrawalCredentials"`
	Amount                *hexutil.Uint64 `json:"amount"`
	Signature             *hexutil.Bytes  `json:"signature"`
	Index                 *hexutil.Uint64 `json:"index"`
}

type executionWithdrawalRequestArgs struct {
	SourceAddress   *common.Address `json:"sourceAddress"`
	ValidatorPubkey *hexutil.Bytes  `json:"validatorPubkey"`
	Amount          *hexutil.Uint64 `json:"amount"`
}

type consolidationRequestArgs struct {
	SourceAddress *common.Address `json:"sourceAddress"`
	SourcePubkey  *hexutil.Bytes  `json:"sourcePubkey"`
	TargetPubkey  *hexutil.Bytes  `json:"targetPubkey"`
}

func (api *executionRequestAPI) AddDepositRequest(ctx context.Context, args depositRequestArgs) (bool, error) {
	if args.Pubkey == nil {
		return false, &invalidParamsError{message: "deposit request pubkey is required"}
	}
	if len(*args.Pubkey) != 48 {
		return false, &invalidParamsError{message: "deposit request pubkey must be 48 bytes"}
	}
	if args.WithdrawalCredentials == nil {
		return false, &invalidParamsError{message: "deposit request withdrawalCredentials is required"}
	}
	if len(*args.WithdrawalCredentials) != 32 {
		return false, &invalidParamsError{message: "deposit request withdrawalCredentials must be 32 bytes"}
	}
	if args.Amount == nil {
		return false, &invalidParamsError{message: "deposit request amount is required"}
	}
	if args.Signature == nil {
		return false, &invalidParamsError{message: "deposit request signature is required"}
	}
	if len(*args.Signature) != 96 {
		return false, &invalidParamsError{message: "deposit request signature must be 96 bytes"}
	}
	if args.Index == nil {
		return false, &invalidParamsError{message: "deposit request index is required"}
	}
	request := ExecutionDepositRequest{Amount: uint64(*args.Amount), Index: uint64(*args.Index)}
	copy(request.Pubkey[:], *args.Pubkey)
	copy(request.WithdrawalCredentials[:], *args.WithdrawalCredentials)
	copy(request.Signature[:], *args.Signature)
	if err := api.node.AddDepositRequest(ctx, request); err != nil {
		return false, executionRequestRPCError(err)
	}
	return true, nil
}

func (api *executionRequestAPI) AddWithdrawalRequest(
	ctx context.Context,
	args executionWithdrawalRequestArgs,
) (bool, error) {
	if args.SourceAddress == nil {
		return false, &invalidParamsError{message: "withdrawal request sourceAddress is required"}
	}
	if args.ValidatorPubkey == nil {
		return false, &invalidParamsError{message: "withdrawal request validatorPubkey is required"}
	}
	if len(*args.ValidatorPubkey) != 48 {
		return false, &invalidParamsError{message: "withdrawal request validatorPubkey must be 48 bytes"}
	}
	if args.Amount == nil {
		return false, &invalidParamsError{message: "withdrawal request amount is required"}
	}
	request := ExecutionWithdrawalRequest{SourceAddress: *args.SourceAddress, Amount: uint64(*args.Amount)}
	copy(request.ValidatorPubkey[:], *args.ValidatorPubkey)
	if err := api.node.AddWithdrawalRequest(ctx, request); err != nil {
		return false, executionRequestRPCError(err)
	}
	return true, nil
}

func (api *executionRequestAPI) AddConsolidationRequest(
	ctx context.Context,
	args consolidationRequestArgs,
) (bool, error) {
	if args.SourceAddress == nil {
		return false, &invalidParamsError{message: "consolidation request sourceAddress is required"}
	}
	if args.SourcePubkey == nil {
		return false, &invalidParamsError{message: "consolidation request sourcePubkey is required"}
	}
	if len(*args.SourcePubkey) != 48 {
		return false, &invalidParamsError{message: "consolidation request sourcePubkey must be 48 bytes"}
	}
	if args.TargetPubkey == nil {
		return false, &invalidParamsError{message: "consolidation request targetPubkey is required"}
	}
	if len(*args.TargetPubkey) != 48 {
		return false, &invalidParamsError{message: "consolidation request targetPubkey must be 48 bytes"}
	}
	request := ExecutionConsolidationRequest{SourceAddress: *args.SourceAddress}
	copy(request.SourcePubkey[:], *args.SourcePubkey)
	copy(request.TargetPubkey[:], *args.TargetPubkey)
	if err := api.node.AddConsolidationRequest(ctx, request); err != nil {
		return false, executionRequestRPCError(err)
	}
	return true, nil
}

func executionRequestRPCError(err error) error {
	if errors.Is(err, ErrDepositRequestQueueFull) || errors.Is(err, ErrWithdrawalRequestQueueFull) ||
		errors.Is(err, ErrConsolidationRequestQueueFull) || errors.Is(err, ErrExecutionRequestIDOverflow) {
		return &invalidParamsError{message: err.Error()}
	}
	return err
}
