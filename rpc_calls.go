package ethertest

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/rpc"
)

func (api *ethAPI) Call(ctx context.Context, args callArgs, selector *rpc.BlockNumberOrHash, overrides *stateOverride) (hexutil.Bytes, error) {
	blockSelector := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if selector != nil {
		blockSelector = *selector
	}
	result, err := api.executeCall(ctx, args, blockSelector, 0, overrides)
	if err != nil {
		return nil, err
	}
	if result.Failed() {
		return nil, result.Err
	}
	return result.Return(), nil
}

func (api *ethAPI) EstimateGas(ctx context.Context, args callArgs, selector *rpc.BlockNumberOrHash, overrides *stateOverride) (hexutil.Uint64, error) {
	blockSelector := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if selector != nil {
		blockSelector = *selector
	}
	header, state, err := api.node.resolveState(blockSelector)
	if err != nil {
		return 0, err
	}
	low, high := uint64(21_000), header.GasLimit
	if args.Gas != nil && uint64(*args.Gas) < high {
		high = uint64(*args.Gas)
	}
	for low < high {
		mid := low + (high-low)/2
		result, callErr := api.executeCallAt(ctx, args, header, state, mid, overrides, nil)
		if callErr != nil || result.Failed() {
			low = mid + 1
		} else {
			high = mid
		}
	}
	result, err := api.executeCallAt(ctx, args, header, state, high, overrides, nil)
	if err != nil || result.Failed() {
		if err != nil {
			return 0, err
		}
		return 0, result.Err
	}
	return hexutil.Uint64(high), nil
}

func (api *ethAPI) executeCall(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, gasOverride uint64, overrides *stateOverride) (*core.ExecutionResult, error) {
	return api.executeCallWithTracer(ctx, args, selector, gasOverride, overrides, nil)
}

func (api *ethAPI) executeCallWithTracer(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, gasOverride uint64, overrides *stateOverride, hooks *tracing.Hooks) (*core.ExecutionResult, error) {
	header, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	return api.executeCallAt(ctx, args, header, state, gasOverride, overrides, hooks)
}

func (api *ethAPI) executeCallAt(ctx context.Context, args callArgs, header *types.Header, state *state.StateDB, gasOverride uint64, overrides *stateOverride, hooks *tracing.Hooks) (*core.ExecutionResult, error) {
	if err := args.validateData(); err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	state = state.Copy()
	if overrides != nil {
		for address, account := range *overrides {
			if account.State != nil && account.StateDiff != nil {
				return nil, errors.New("state and stateDiff cannot be used together")
			}
			if account.Balance != nil {
				balance, err := checkedU256("balance", (*big.Int)(account.Balance))
				if err != nil {
					return nil, &invalidParamsError{message: err.Error()}
				}
				state.SetBalance(address, balance, tracing.BalanceChangeUnspecified)
			}
			if account.Nonce != nil {
				state.SetNonce(address, uint64(*account.Nonce), tracing.NonceChangeUnspecified)
			}
			if account.Code != nil {
				state.SetCode(address, *account.Code, tracing.CodeChangeUnspecified)
			}
			if account.State != nil {
				state.SetStorage(address, *account.State)
			}
			if account.StateDiff != nil {
				for key, value := range *account.StateDiff {
					state.SetState(address, key, value)
				}
			}
		}
	}
	from := common.Address{}
	if args.From != nil {
		from = *args.From
	}
	gas := header.GasLimit
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	if gasOverride != 0 {
		gas = gasOverride
	}
	value := new(big.Int)
	if args.Value != nil {
		value = (*big.Int)(args.Value)
	}
	gasPrice, feeCap, tipCap := new(big.Int), new(big.Int), new(big.Int)
	if args.GasPrice != nil {
		gasPrice.Set((*big.Int)(args.GasPrice))
		feeCap.Set(gasPrice)
		tipCap.Set(gasPrice)
	} else {
		if args.MaxFeePerGas != nil {
			feeCap.Set((*big.Int)(args.MaxFeePerGas))
		}
		if args.MaxPriorityFeePerGas != nil {
			tipCap.Set((*big.Int)(args.MaxPriorityFeePerGas))
		}
		if header.BaseFee != nil && (feeCap.Sign() != 0 || tipCap.Sign() != 0) {
			gasPrice.Add(header.BaseFee, tipCap)
			if gasPrice.Cmp(feeCap) > 0 {
				gasPrice.Set(feeCap)
			}
		}
	}
	data := []byte(nil)
	if args.Input != nil {
		data = *args.Input
	} else if args.Data != nil {
		data = *args.Data
	}
	accessList := types.AccessList(nil)
	if args.AccessList != nil {
		accessList = *args.AccessList
	}
	nonce := uint64(0)
	if args.Nonce != nil {
		nonce = uint64(*args.Nonce)
	}
	blobFeeCap := new(big.Int)
	if args.BlobFeeCap != nil {
		blobFeeCap.Set((*big.Int)(args.BlobFeeCap))
	}
	valueU256, err := checkedU256("value", value)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	gasPriceU256, err := checkedU256("gas price", gasPrice)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	feeCapU256, err := checkedU256("gas fee cap", feeCap)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	tipCapU256, err := checkedU256("priority fee", tipCap)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	blobFeeCapU256, err := checkedU256("blob fee cap", blobFeeCap)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	message := &core.Message{
		From: from, To: args.To, Nonce: nonce, Value: valueU256, GasLimit: gas,
		GasPrice: gasPriceU256, GasFeeCap: feeCapU256,
		GasTipCap: tipCapU256, Data: data, AccessList: accessList,
		BlobGasFeeCap: blobFeeCapU256, BlobHashes: args.BlobHashes,
		SetCodeAuthorizations: args.AuthorizationList,
		SkipNonceChecks:       true, SkipTransactionChecks: true,
	}
	blockContext := core.NewEVMBlockContext(header, api.node.chain.blockchain, nil)
	evm := vm.NewEVM(blockContext, state, api.node.chain.config, vm.Config{NoBaseFee: true, Tracer: hooks})
	evm.SetTxContext(core.NewEVMTxContext(message))
	stop := context.AfterFunc(ctx, evm.Cancel)
	defer stop()
	return core.ApplyMessage(evm, message, core.NewGasPool(gas))
}
