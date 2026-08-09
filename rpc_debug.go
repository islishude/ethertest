package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

type traceConfig struct {
	Tracer           *string         `json:"tracer"`
	TracerConfig     json.RawMessage `json:"tracerConfig"`
	EnableMemory     bool            `json:"enableMemory"`
	DisableStack     bool            `json:"disableStack"`
	DisableStorage   bool            `json:"disableStorage"`
	EnableReturnData bool            `json:"enableReturnData"`
	Limit            int             `json:"limit"`
}

type traceResult struct {
	TxHash common.Hash     `json:"txHash"`
	Result json.RawMessage `json:"result,omitempty"`
}

func (api *debugAPI) GetRawHeader(_ context.Context, number rpc.BlockNumber) (hexutil.Bytes, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	return rlp.EncodeToBytes(block.Header())
}

func (api *debugAPI) GetRawBlock(_ context.Context, number rpc.BlockNumber) (hexutil.Bytes, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	return rlp.EncodeToBytes(block)
}

func (api *debugAPI) GetRawReceipts(_ context.Context, number rpc.BlockNumber) ([]hexutil.Bytes, error) {
	selector := rpc.BlockNumberOrHashWithNumber(number)
	block, receipts, err := api.node.blockAndReceipts(selector)
	if err != nil || block == nil {
		return nil, err
	}
	if len(receipts) != len(block.Transactions()) {
		return nil, fmt.Errorf("receipts length mismatch: %d vs %d", len(receipts), len(block.Transactions()))
	}
	result := make([]hexutil.Bytes, len(receipts))
	for index, receipt := range receipts {
		encoded, err := receipt.MarshalBinary()
		if err != nil {
			return nil, err
		}
		result[index] = encoded
	}
	return result, nil
}

func (api *debugAPI) GetRawTransaction(_ context.Context, hash common.Hash) (hexutil.Bytes, error) {
	transaction, _, _, _ := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if transaction == nil {
		transaction = api.node.chain.poolTransaction(hash)
	}
	if transaction == nil {
		return nil, nil
	}
	return transaction.MarshalBinary()
}

func (api *debugAPI) TraceCall(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, config *traceConfig) (json.RawMessage, error) {
	if config == nil || config.Tracer == nil {
		loggerConfig := new(logger.Config)
		if config != nil {
			loggerConfig.EnableMemory = config.EnableMemory
			loggerConfig.DisableStack = config.DisableStack
			loggerConfig.DisableStorage = config.DisableStorage
			loggerConfig.EnableReturnData = config.EnableReturnData
			loggerConfig.Limit = config.Limit
		}
		tracer := logger.NewStructLogger(loggerConfig)
		if _, err := (&ethAPI{node: api.node}).executeCallWithTracer(ctx, args, selector, 0, nil, tracer.Hooks()); err != nil {
			return nil, err
		}
		return tracer.GetResult()
	}
	if tracers.DefaultDirectory.IsJS(*config.Tracer) {
		return nil, errors.New("JavaScript tracers are not supported")
	}
	tracer, err := tracers.DefaultDirectory.New(*config.Tracer, &tracers.Context{}, config.TracerConfig, api.node.chain.config)
	if err != nil {
		return nil, err
	}
	if _, err := (&ethAPI{node: api.node}).executeCallWithTracer(ctx, args, selector, 0, nil, tracer.Hooks); err != nil {
		tracer.Stop(err)
		return nil, err
	}
	return tracer.GetResult()
}

func (api *debugAPI) TraceTransaction(ctx context.Context, hash common.Hash, config *traceConfig) (json.RawMessage, error) {
	transaction, blockHash, blockNumber, transactionIndex := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if transaction == nil {
		return nil, errors.New("transaction not found")
	}
	block := api.node.chain.blockchain.GetBlock(blockHash, blockNumber)
	if block == nil || blockNumber == 0 {
		return nil, errors.New("transaction block is unavailable")
	}
	results, err := api.traceBlock(ctx, block, config, &transactionIndex)
	if err != nil {
		return nil, err
	}
	if len(results) != 1 {
		return nil, errors.New("transaction index is out of bounds")
	}
	return results[0].Result, nil
}

func (api *debugAPI) TraceBlockByHash(ctx context.Context, hash common.Hash, config *traceConfig) ([]*traceResult, error) {
	block := api.node.chain.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil, fmt.Errorf("block %s not found", hash.Hex())
	}
	return api.traceBlock(ctx, block, config, nil)
}

func (api *debugAPI) TraceBlockByNumber(ctx context.Context, number rpc.BlockNumber, config *traceConfig) ([]*traceResult, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil {
		return nil, err
	}
	if block == nil {
		return nil, fmt.Errorf("block #%d not found", number)
	}
	return api.traceBlock(ctx, block, config, nil)
}

func (api *debugAPI) traceBlock(ctx context.Context, block *types.Block, config *traceConfig, target *uint64) ([]*traceResult, error) {
	if block.NumberU64() == 0 {
		return nil, errors.New("genesis is not traceable")
	}
	if config != nil && config.Tracer != nil && tracers.DefaultDirectory.IsJS(*config.Tracer) {
		return nil, errors.New("JavaScript tracers are not supported")
	}
	parent := api.node.chain.blockchain.GetHeaderByHash(block.ParentHash())
	if parent == nil {
		return nil, errors.New("block parent is unavailable")
	}
	state, err := api.node.chain.blockchain.StateAt(parent)
	if err != nil {
		return nil, err
	}
	blockContext := core.NewEVMBlockContext(block.Header(), api.node.chain.blockchain, nil)
	gasPool := core.NewGasPool(block.GasLimit())
	signer := types.MakeSigner(api.node.chain.config, block.Number(), block.Time())
	preEVM := vm.NewEVM(blockContext, state, api.node.chain.config, vm.Config{})
	core.PreExecution(ctx, block.BeaconRoot(), parent, api.node.chain.config, preEVM, block.Number(), block.Time())
	preEVM.Release()

	results := make([]*traceResult, 0, len(block.Transactions()))
	for index, tx := range block.Transactions() {
		message, err := core.TransactionToMessage(tx, signer, block.BaseFee())
		if err != nil {
			return nil, err
		}
		traceThis := target == nil || *target == uint64(index)
		var hooks *tracing.Hooks
		var getResult func() (json.RawMessage, error)
		var stopTracer func(error)
		if traceThis {
			hooks, getResult, stopTracer, err = api.newTracer(config, block, index, tx.Hash())
			if err != nil {
				return nil, err
			}
		}
		vmConfig := vm.Config{}
		if traceThis {
			vmConfig.Tracer = hooks
		}
		evm := vm.NewEVM(blockContext, state, api.node.chain.config, vmConfig)
		evm.SetTxContext(core.NewEVMTxContext(message))
		state.SetTxContext(tx.Hash(), index, uint32(index+1))
		stop := context.AfterFunc(ctx, evm.Cancel)
		_, _, applyErr := core.ApplyTransactionWithEVM(
			message, gasPool, state, block.Number(), block.Hash(), block.Time(), tx, evm,
		)
		stop()
		evm.Release()
		if applyErr != nil {
			if traceThis {
				stopTracer(applyErr)
			}
			return nil, applyErr
		}
		if traceThis {
			result, err := getResult()
			if err != nil {
				return nil, err
			}
			results = append(results, &traceResult{TxHash: tx.Hash(), Result: result})
		}
	}
	return results, nil
}

func (api *debugAPI) newTracer(config *traceConfig, block *types.Block, index int, hash common.Hash) (*tracing.Hooks, func() (json.RawMessage, error), func(error), error) {
	if config == nil || config.Tracer == nil {
		loggerConfig := new(logger.Config)
		if config != nil {
			loggerConfig.EnableMemory, loggerConfig.DisableStack = config.EnableMemory, config.DisableStack
			loggerConfig.DisableStorage, loggerConfig.EnableReturnData = config.DisableStorage, config.EnableReturnData
			loggerConfig.Limit = config.Limit
		}
		structured := logger.NewStructLogger(loggerConfig)
		return structured.Hooks(), structured.GetResult, func(error) {}, nil
	}
	if tracers.DefaultDirectory.IsJS(*config.Tracer) {
		return nil, nil, nil, errors.New("JavaScript tracers are not supported")
	}
	native, err := tracers.DefaultDirectory.New(*config.Tracer, &tracers.Context{
		BlockHash: block.Hash(), BlockNumber: new(big.Int).SetUint64(block.NumberU64()),
		TxIndex: index, TxHash: hash,
	}, config.TracerConfig, api.node.chain.config)
	if err != nil {
		return nil, nil, nil, err
	}
	return native.Hooks, native.GetResult, native.Stop, nil
}
