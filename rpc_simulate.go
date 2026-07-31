// Copyright 2023 The go-ethereum Authors
// Copyright 2026 The ethertest Authors
//
// This file is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This file is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// This implementation follows go-ethereum's eth_simulateV1 execution model,
// adapted to ethertest's public core/state/vm APIs, synthetic chain context,
// configured slot duration, and fixed RPC resource limits. It intentionally
// does not import geth's internal/ethapi package.

package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

const (
	maxSimulateBlocks        = 256
	maxSimulateCallsPerBlock = 5000
	maxSimulateTotalCalls    = 10000
	maxSimulateGas           = 50_000_000
	simulateTimeout          = 5 * time.Second
)

const (
	simErrVM                = -32015
	simErrTimeout           = -32016
	simErrMaxFeeTooLow      = -32005
	simErrNonceTooLow       = -38010
	simErrNonceTooHigh      = -38011
	simErrBaseFeeTooLow     = -38012
	simErrIntrinsicGas      = -38013
	simErrInsufficientFunds = -38014
	simErrBlockGasLimit     = -38015
	simErrBlockNumber       = -38020
	simErrBlockTimestamp    = -38021
	simErrMoveSelf          = -38022
	simErrMoveDuplicate     = -38023
	simErrSenderNotEOA      = -38024
	simErrMaxInitCode       = -38025
)

type simulationRPCError struct {
	code    int
	message string
}

func (err *simulationRPCError) Error() string  { return err.message }
func (err *simulationRPCError) ErrorCode() int { return err.code }

type simulationPayload struct {
	BlockStateCalls        []simulationBlock `json:"blockStateCalls"`
	TraceTransfers         bool              `json:"traceTransfers"`
	Validation             bool              `json:"validation"`
	ReturnFullTransactions bool              `json:"returnFullTransactions"`
}

type simulationBlock struct {
	BlockOverrides *simulationBlockOverrides `json:"blockOverrides"`
	StateOverrides *stateOverride            `json:"stateOverrides"`
	Calls          []callArgs                `json:"calls"`
}

type simulationBlockOverrides struct {
	Number        *hexutil.Uint64    `json:"number"`
	PrevRandao    *hexutil.Big       `json:"prevRandao"`
	Time          *hexutil.Uint64    `json:"time"`
	GasLimit      *hexutil.Uint64    `json:"gasLimit"`
	FeeRecipient  *common.Address    `json:"feeRecipient"`
	BaseFeePerGas *hexutil.Big       `json:"baseFeePerGas"`
	Withdrawals   *types.Withdrawals `json:"withdrawals"`
	BlobBaseFee   *hexutil.Big       `json:"blobBaseFee"`
}

type simulationCallError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
	Data    string `json:"data,omitempty"`
}

type simulationCallResult struct {
	ReturnData hexutil.Bytes        `json:"returnData"`
	Logs       []*types.Log         `json:"logs"`
	GasUsed    hexutil.Uint64       `json:"gasUsed"`
	MaxUsedGas hexutil.Uint64       `json:"maxUsedGas"`
	Status     hexutil.Uint64       `json:"status"`
	Error      *simulationCallError `json:"error,omitempty"`
}

func (result *simulationCallResult) MarshalJSON() ([]byte, error) {
	type alias simulationCallResult
	if result.Logs == nil {
		result.Logs = []*types.Log{}
	}
	return json.Marshal((*alias)(result))
}

type simulationBlockResult struct {
	Block   *types.Block
	Calls   []simulationCallResult
	FullTx  bool
	Config  *params.ChainConfig
	Senders map[common.Hash]common.Address
}

func (result *simulationBlockResult) MarshalJSON() ([]byte, error) {
	data := marshalBlock(result.Block, result.FullTx, result.Config)
	data["calls"] = result.Calls
	if result.FullTx {
		transactions, ok := data["transactions"].([]*rpcTransaction)
		if !ok {
			return nil, errors.New("simulated transaction result has invalid type")
		}
		for _, transaction := range transactions {
			transaction.From = result.Senders[transaction.Hash]
		}
	}
	return json.Marshal(data)
}

type simulationGasBudget struct {
	remaining uint64
}

func (budget *simulationGasBudget) cap(gas uint64) (uint64, bool) {
	if gas > budget.remaining {
		return budget.remaining, true
	}
	return gas, false
}

func (budget *simulationGasBudget) consume(gas uint64) error {
	if gas > budget.remaining {
		return &clientLimitExceededError{message: "simulation gas limit exceeded"}
	}
	budget.remaining -= gas
	return nil
}

type simulator struct {
	node           *Node
	state          *state.StateDB
	base           *types.Header
	config         *params.ChainConfig
	budget         simulationGasBudget
	traceTransfers bool
	validate       bool
	fullTx         bool
	timeIncrement  uint64
}

func (api *ethAPI) SimulateV1(ctx context.Context, payload simulationPayload, selector *rpc.BlockNumberOrHash) ([]*simulationBlockResult, error) {
	if len(payload.BlockStateCalls) == 0 {
		return nil, &invalidParamsError{message: "empty blockStateCalls"}
	}
	if len(payload.BlockStateCalls) > maxSimulateBlocks {
		return nil, &clientLimitExceededError{message: fmt.Sprintf("too many blocks: %d > %d", len(payload.BlockStateCalls), maxSimulateBlocks)}
	}
	totalCalls := 0
	for _, block := range payload.BlockStateCalls {
		if len(block.Calls) > maxSimulateCallsPerBlock {
			return nil, &clientLimitExceededError{message: fmt.Sprintf("too many calls in block: %d > %d", len(block.Calls), maxSimulateCallsPerBlock)}
		}
		totalCalls += len(block.Calls)
		if totalCalls > maxSimulateTotalCalls {
			return nil, &clientLimitExceededError{message: fmt.Sprintf("too many calls: %d > %d", totalCalls, maxSimulateTotalCalls)}
		}
	}
	baseSelector := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if selector != nil {
		baseSelector = *selector
	}
	base, statedb, err := api.node.resolveState(baseSelector)
	if err != nil {
		return nil, err
	}
	increment := api.node.chain.slotDuration
	if increment == 0 {
		increment = 1
	}
	sim := &simulator{
		node: api.node, state: statedb.Copy(), base: types.CopyHeader(base), config: api.node.chain.config,
		budget: simulationGasBudget{remaining: maxSimulateGas}, traceTransfers: payload.TraceTransfers,
		validate: payload.Validation, fullTx: payload.ReturnFullTransactions, timeIncrement: increment,
	}
	simCtx, cancel := context.WithTimeout(ctx, simulateTimeout)
	defer cancel()
	results, err := sim.execute(simCtx, payload.BlockStateCalls)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return nil, &simulationRPCError{code: simErrTimeout, message: "simulation timed out"}
	}
	return results, err
}

func (sim *simulator) execute(ctx context.Context, blocks []simulationBlock) ([]*simulationBlockResult, error) {
	blocks, err := sim.sanitizeChain(blocks)
	if err != nil {
		return nil, err
	}
	headers, err := sim.makeHeaders(blocks)
	if err != nil {
		return nil, err
	}
	results := make([]*simulationBlockResult, len(blocks))
	parent := sim.base
	for index := range blocks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		block, calls, senders, err := sim.processBlock(ctx, &blocks[index], headers[index], parent, headers[:index])
		if err != nil {
			return nil, err
		}
		headers[index] = block.Header()
		results[index] = &simulationBlockResult{Block: block, Calls: calls, FullTx: sim.fullTx, Config: sim.config, Senders: senders}
		parent = block.Header()
	}
	return results, nil
}

func (sim *simulator) sanitizeChain(blocks []simulationBlock) ([]simulationBlock, error) {
	result := make([]simulationBlock, 0, len(blocks))
	baseNumber := sim.base.Number.Uint64()
	previousNumber := baseNumber
	previousTime := sim.base.Time
	for _, block := range blocks {
		if block.BlockOverrides == nil {
			block.BlockOverrides = new(simulationBlockOverrides)
		}
		if block.BlockOverrides.Withdrawals == nil {
			empty := types.Withdrawals{}
			block.BlockOverrides.Withdrawals = &empty
		}
		number := previousNumber + 1
		if number == 0 {
			return nil, &simulationRPCError{code: simErrBlockNumber, message: "block number overflow"}
		}
		if block.BlockOverrides.Number != nil {
			number = uint64(*block.BlockOverrides.Number)
		}
		if number <= previousNumber {
			return nil, &simulationRPCError{code: simErrBlockNumber, message: fmt.Sprintf("block numbers must increase: %d <= %d", number, previousNumber)}
		}
		if number-baseNumber > maxSimulateBlocks {
			return nil, &clientLimitExceededError{message: "too many simulated output blocks"}
		}
		for gapNumber := previousNumber + 1; gapNumber < number; gapNumber++ {
			if previousTime > ^uint64(0)-sim.timeIncrement {
				return nil, &simulationRPCError{code: simErrBlockTimestamp, message: "block timestamp overflow"}
			}
			previousTime += sim.timeIncrement
			gapNumberHex := hexutil.Uint64(gapNumber)
			gapTimeHex := hexutil.Uint64(previousTime)
			empty := types.Withdrawals{}
			result = append(result, simulationBlock{BlockOverrides: &simulationBlockOverrides{Number: &gapNumberHex, Time: &gapTimeHex, Withdrawals: &empty}})
			if len(result) > maxSimulateBlocks {
				return nil, &clientLimitExceededError{message: "too many simulated output blocks"}
			}
		}
		timestamp := previousTime + sim.timeIncrement
		if timestamp < previousTime {
			return nil, &simulationRPCError{code: simErrBlockTimestamp, message: "block timestamp overflow"}
		}
		if block.BlockOverrides.Time != nil {
			timestamp = uint64(*block.BlockOverrides.Time)
		}
		if timestamp <= previousTime {
			return nil, &simulationRPCError{code: simErrBlockTimestamp, message: fmt.Sprintf("block timestamps must increase: %d <= %d", timestamp, previousTime)}
		}
		numberHex := hexutil.Uint64(number)
		timeHex := hexutil.Uint64(timestamp)
		block.BlockOverrides.Number = &numberHex
		block.BlockOverrides.Time = &timeHex
		result = append(result, block)
		if len(result) > maxSimulateBlocks {
			return nil, &clientLimitExceededError{message: "too many simulated output blocks"}
		}
		previousNumber = number
		previousTime = timestamp
	}
	return result, nil
}

func (sim *simulator) makeHeaders(blocks []simulationBlock) ([]*types.Header, error) {
	headers := make([]*types.Header, len(blocks))
	previous := sim.base
	for index := range blocks {
		overrides := blocks[index].BlockOverrides
		if overrides == nil || overrides.Number == nil || overrides.Time == nil {
			return nil, errors.New("simulation block was not sanitized")
		}
		number := new(big.Int).SetUint64(uint64(*overrides.Number))
		timestamp := uint64(*overrides.Time)
		difficulty := new(big.Int).Set(previous.Difficulty)
		if sim.config.IsPostMerge(number.Uint64(), timestamp) {
			difficulty.SetUint64(0)
		}
		var withdrawalsRoot *common.Hash
		if sim.config.IsShanghai(number, timestamp) {
			root := types.EmptyWithdrawalsHash
			withdrawalsRoot = &root
		}
		var beaconRoot *common.Hash
		if sim.config.IsCancun(number, timestamp) {
			root := common.Hash{}
			beaconRoot = &root
		}
		header := &types.Header{
			UncleHash: types.EmptyUncleHash, TxHash: types.EmptyTxsHash, ReceiptHash: types.EmptyReceiptsHash,
			Coinbase: previous.Coinbase, Difficulty: difficulty, Number: number, GasLimit: previous.GasLimit, Time: timestamp,
			WithdrawalsHash: withdrawalsRoot, ParentBeaconRoot: beaconRoot,
		}
		if overrides.GasLimit != nil {
			header.GasLimit = uint64(*overrides.GasLimit)
		}
		if overrides.FeeRecipient != nil {
			header.Coinbase = *overrides.FeeRecipient
		}
		if overrides.PrevRandao != nil {
			if overrides.PrevRandao.ToInt().BitLen() > 256 {
				return nil, &invalidParamsError{message: "prevRandao exceeds 256 bits"}
			}
			header.MixDigest = common.BigToHash(overrides.PrevRandao.ToInt())
		}
		if overrides.BaseFeePerGas != nil {
			if overrides.BaseFeePerGas.ToInt().BitLen() > 256 {
				return nil, &invalidParamsError{message: "baseFeePerGas exceeds 256 bits"}
			}
			header.BaseFee = overrides.BaseFeePerGas.ToInt()
		}
		if overrides.BlobBaseFee != nil && overrides.BlobBaseFee.ToInt().BitLen() > 64 {
			return nil, &invalidParamsError{message: "blobBaseFee exceeds 64 bits"}
		}
		if overrides.Withdrawals != nil && len(*overrides.Withdrawals) > 16 {
			return nil, &invalidParamsError{message: "withdrawals exceeds maximum length 16"}
		}
		headers[index] = header
		previous = header
	}
	return headers, nil
}

func (sim *simulator) processBlock(ctx context.Context, block *simulationBlock, header, parent *types.Header, prior []*types.Header) (*types.Block, []simulationCallResult, map[common.Hash]common.Address, error) {
	header.ParentHash = parent.Hash()
	if sim.config.IsLondon(header.Number) && header.BaseFee == nil {
		if sim.validate {
			header.BaseFee = eip1559.CalcBaseFee(sim.config, parent)
		} else {
			header.BaseFee = new(big.Int)
		}
	}
	if sim.config.IsCancun(header.Number, header.Time) {
		excess := uint64(0)
		if sim.config.IsCancun(parent.Number, parent.Time) {
			excess = eip4844.CalcExcessBlobGas(sim.config, parent, header.Time)
		}
		header.ExcessBlobGas = &excess
	}
	chainContext := &simulationChainContext{node: sim.node, base: sim.base, headers: prior}
	blockContext := core.NewEVMBlockContext(header, chainContext, nil)
	if block.BlockOverrides.BlobBaseFee != nil {
		blockContext.BlobBaseFee = block.BlockOverrides.BlobBaseFee.ToInt()
	}
	rules := sim.config.Rules(header.Number, header.Difficulty.Sign() == 0, header.Time)
	precompiles := maps.Clone(vm.ActivePrecompiledContracts(rules))
	if err := applySimulationStateOverrides(block.StateOverrides, sim.state, precompiles); err != nil {
		return nil, nil, nil, err
	}

	gasPool := core.NewGasPool(blockContext.GasLimit)
	transactions := make([]*types.Transaction, len(block.Calls))
	receipts := make([]*types.Receipt, len(block.Calls))
	callResults := make([]simulationCallResult, len(block.Calls))
	senders := make(map[common.Hash]common.Address, len(block.Calls))
	blockAccessList := bal.NewConstructionBlockAccessList()
	tracer := newSimulationTracer(sim.traceTransfers, header.Number.Uint64(), header.Time)
	hookedState := vm.StateDB(sim.state)
	if hooks := tracer.Hooks(); hooks != nil {
		hookedState = state.NewHookedState(sim.state, hooks)
	}
	evm := vm.NewEVM(blockContext, hookedState, sim.config, vm.Config{NoBaseFee: !sim.validate, Tracer: tracer.Hooks()})
	defer evm.Release()
	evm.SetPrecompiles(precompiles)
	blockAccessList.Merge(core.PreExecution(ctx, header.ParentBeaconRoot, parent, sim.config, evm, header.Number, header.Time))

	var allLogs []*types.Log
	var blobGasUsed uint64
	for index := range block.Calls {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, err
		}
		call := block.Calls[index]
		gasCapped, err := sim.sanitizeCall(&call, gasPool)
		if err != nil {
			return nil, nil, nil, err
		}
		txType, _ := simulationTransactionType(call)
		transaction, err := makeUnsignedTransaction(txType, &call, sim.config.ChainID)
		if err != nil {
			return nil, nil, nil, &invalidParamsError{message: err.Error()}
		}
		transactions[index] = transaction
		senders[transaction.Hash()] = call.from()
		tracer.Reset(transaction.Hash(), uint(index))
		sim.state.SetTxContext(transaction.Hash(), index, uint32(index+1))
		message, err := simulationMessage(&call, header.BaseFee, !sim.validate)
		if err != nil {
			return nil, nil, nil, &invalidParamsError{message: err.Error()}
		}
		result, err := applySimulationMessage(ctx, evm, message, gasPool)
		if err != nil {
			if gasCapped && (errors.Is(err, core.ErrIntrinsicGas) || errors.Is(err, core.ErrFloorDataGas) || errors.Is(err, core.ErrGasLimitReached)) {
				return nil, nil, nil, &clientLimitExceededError{message: "simulation gas limit exceeded"}
			}
			return nil, nil, nil, mapSimulationValidationError(err)
		}
		if err := sim.state.Error(); err != nil {
			return nil, nil, nil, fmt.Errorf("simulation state access failed: %w", err)
		}
		if gasCapped && errors.Is(result.Err, vm.ErrOutOfGas) {
			return nil, nil, nil, &clientLimitExceededError{message: "simulation gas limit exceeded"}
		}
		if err := sim.budget.consume(result.UsedGas); err != nil {
			return nil, nil, nil, err
		}
		var postState []byte
		if sim.config.IsByzantium(header.Number) {
			blockAccessList.Merge(hookedState.Finalise(true))
		} else {
			postState = sim.state.IntermediateRoot(sim.config.IsEIP158(header.Number)).Bytes()
		}
		receipt := core.MakeReceipt(evm, result, sim.state, header.Number, common.Hash{}, header.Time, transaction, gasPool.CumulativeUsed(), postState)
		receipts[index] = receipt
		blobGasUsed += receipt.BlobGasUsed
		logs := tracer.Logs()
		callResult := simulationCallResult{
			ReturnData: result.Return(), Logs: logs, GasUsed: hexutil.Uint64(result.UsedGas),
			MaxUsedGas: hexutil.Uint64(result.MaxUsedGas), Status: hexutil.Uint64(types.ReceiptStatusSuccessful),
		}
		if result.Failed() {
			callResult.Status = hexutil.Uint64(types.ReceiptStatusFailed)
			if errors.Is(result.Err, vm.ErrExecutionReverted) {
				message := result.Err.Error()
				if reason, unpackErr := abi.UnpackRevert(result.Revert()); unpackErr == nil {
					message = fmt.Sprintf("%s: %s", message, reason)
				}
				callResult.Error = &simulationCallError{Message: message, Code: 3, Data: hexutil.Encode(result.Revert())}
			} else {
				callResult.Error = &simulationCallError{Message: "vm execution error: " + result.Err.Error(), Code: simErrVM}
			}
		} else {
			allLogs = append(allLogs, logs...)
		}
		callResults[index] = callResult
	}
	header.GasUsed = gasPool.Used()
	if sim.config.IsCancun(header.Number, header.Time) {
		header.BlobGasUsed = &blobGasUsed
	}
	requests, postAccessList, err := core.PostExecution(ctx, sim.config, header.Number, header.Time, allLogs, evm, uint32(len(block.Calls)+1))
	if err != nil {
		return nil, nil, nil, err
	}
	blockAccessList.Merge(postAccessList)
	if requests != nil {
		hash := types.CalcRequestsHash(requests)
		header.RequestsHash = &hash
	}
	body := &types.Body{Transactions: transactions}
	if sim.config.IsShanghai(header.Number, header.Time) {
		body.Withdrawals = *block.BlockOverrides.Withdrawals
	}
	sim.node.chain.blockchain.Engine().Finalize(chainContext, header, sim.state, body, uint32(len(block.Calls)+1), blockAccessList)
	if err := sim.state.Error(); err != nil {
		return nil, nil, nil, fmt.Errorf("simulation state finalization failed: %w", err)
	}
	assembled := core.AssembleBlock(chainContext, header, sim.state, body, receipts, blockAccessList)
	repairSimulationLogs(callResults, assembled.Hash())
	return assembled, callResults, senders, nil
}

func (sim *simulator) sanitizeCall(call *callArgs, gasPool *core.GasPool) (bool, error) {
	if err := call.validateData(); err != nil {
		return false, &invalidParamsError{message: err.Error()}
	}
	if call.Blobs != nil || call.Commitments != nil || call.Proofs != nil {
		return false, &invalidParamsError{message: "blob sidecars are not accepted by eth_simulateV1"}
	}
	if call.ChainID != nil && call.ChainID.ToInt().Cmp(sim.config.ChainID) != 0 {
		return false, &invalidParamsError{message: "chainId does not match node"}
	}
	if call.ChainID == nil {
		call.ChainID = (*hexutil.Big)(new(big.Int).Set(sim.config.ChainID))
	}
	if call.Nonce == nil {
		nonce := hexutil.Uint64(sim.state.GetNonce(call.from()))
		call.Nonce = &nonce
	}
	if call.Value == nil {
		call.Value = new(hexutil.Big)
	}
	remaining := gasPool.Gas()
	if call.Gas == nil {
		gas := hexutil.Uint64(remaining)
		call.Gas = &gas
	}
	if uint64(*call.Gas) > remaining {
		return false, &simulationRPCError{code: simErrBlockGasLimit, message: fmt.Sprintf("block gas limit reached: remaining %d, requested %d", remaining, *call.Gas)}
	}
	capped, gasCapped := sim.budget.cap(uint64(*call.Gas))
	if capped == 0 {
		return false, &clientLimitExceededError{message: "simulation gas limit exceeded"}
	}
	gas := hexutil.Uint64(capped)
	call.Gas = &gas
	txType, err := simulationTransactionType(*call)
	if err != nil {
		return false, &invalidParamsError{message: err.Error()}
	}
	if err := validateSimulationTransactionFields(txType, call); err != nil {
		return false, &invalidParamsError{message: err.Error()}
	}
	if txType == types.LegacyTxType || txType == types.AccessListTxType {
		if call.GasPrice == nil {
			call.GasPrice = new(hexutil.Big)
		}
	} else {
		if call.MaxFeePerGas == nil {
			call.MaxFeePerGas = new(hexutil.Big)
		}
		if call.MaxPriorityFeePerGas == nil {
			call.MaxPriorityFeePerGas = new(hexutil.Big)
		}
		if call.AccessList == nil {
			empty := types.AccessList{}
			call.AccessList = &empty
		}
	}
	if txType == types.BlobTxType && call.BlobFeeCap == nil {
		call.BlobFeeCap = new(hexutil.Big)
	}
	if call.MaxFeePerGas != nil && call.MaxPriorityFeePerGas != nil && call.MaxFeePerGas.ToInt().Cmp(call.MaxPriorityFeePerGas.ToInt()) < 0 {
		return false, &simulationRPCError{code: simErrMaxFeeTooLow, message: "maxFeePerGas is lower than maxPriorityFeePerGas"}
	}
	return gasCapped, nil
}

func simulationTransactionType(args callArgs) (uint8, error) {
	if args.Type != nil {
		return transactionType(args)
	}
	switch {
	case args.AuthorizationList != nil:
		return types.SetCodeTxType, nil
	case args.BlobHashes != nil:
		return types.BlobTxType, nil
	case args.GasPrice != nil:
		return types.LegacyTxType, nil
	default:
		// eth_simulateV1 represents calls as EIP-1559 transactions by
		// default, including when maxFeePerBlobGas is present without hashes.
		return types.DynamicFeeTxType, nil
	}
}

func validateSimulationTransactionFields(txType uint8, args *callArgs) error {
	hasDynamicFee := args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil
	hasBlobHashes := args.BlobHashes != nil
	hasAuth := args.AuthorizationList != nil
	switch txType {
	case types.LegacyTxType:
		if hasDynamicFee || args.AccessList != nil || hasBlobHashes || hasAuth {
			return errors.New("legacy transaction contains typed-transaction fields")
		}
	case types.AccessListTxType:
		if hasDynamicFee || hasBlobHashes || hasAuth {
			return errors.New("access-list transaction contains incompatible fee, blob, or authorization fields")
		}
	case types.DynamicFeeTxType:
		if args.GasPrice != nil || hasBlobHashes || hasAuth {
			return errors.New("dynamic-fee transaction contains gasPrice, blob hashes, or authorization fields")
		}
	case types.BlobTxType:
		if args.GasPrice != nil || hasAuth {
			return errors.New("blob transaction contains gasPrice or authorization fields")
		}
		if args.To == nil {
			return errors.New(`missing "to" in blob transaction`)
		}
		if len(args.BlobHashes) == 0 {
			return errors.New("need at least 1 blobVersionedHash for a blob transaction")
		}
	case types.SetCodeTxType:
		if args.GasPrice != nil || hasBlobHashes {
			return errors.New("set-code transaction contains gasPrice or blob hashes")
		}
		if args.To == nil {
			return errors.New(`missing "to" in set-code transaction`)
		}
		if len(args.AuthorizationList) == 0 {
			return errors.New("need at least 1 authorization for a set-code transaction")
		}
	default:
		return fmt.Errorf("unsupported transaction type 0x%x", txType)
	}
	return nil
}

func simulationMessage(args *callArgs, baseFee *big.Int, skipNonce bool) (*core.Message, error) {
	value, err := checkedU256("value", args.Value.ToInt())
	if err != nil {
		return nil, err
	}
	var gasPrice, feeCap, tipCap *uint256.Int
	if args.GasPrice != nil {
		gasPrice, err = checkedU256("gasPrice", args.GasPrice.ToInt())
		if err != nil {
			return nil, err
		}
		feeCap, tipCap = gasPrice.Clone(), gasPrice.Clone()
	} else {
		feeCap, err = checkedU256("maxFeePerGas", args.MaxFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		tipCap, err = checkedU256("maxPriorityFeePerGas", args.MaxPriorityFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		price := new(big.Int)
		if baseFee != nil && (feeCap.BitLen() != 0 || tipCap.BitLen() != 0) {
			price.Add(baseFee, tipCap.ToBig())
			if price.Cmp(feeCap.ToBig()) > 0 {
				price.Set(feeCap.ToBig())
			}
		}
		gasPrice, err = checkedU256("gasPrice", price)
		if err != nil {
			return nil, err
		}
	}
	blobFee := new(uint256.Int)
	if args.BlobFeeCap != nil {
		blobFee, err = checkedU256("maxFeePerBlobGas", args.BlobFeeCap.ToInt())
		if err != nil {
			return nil, err
		}
	}
	var accessList types.AccessList
	if args.AccessList != nil {
		accessList = *args.AccessList
	}
	return &core.Message{
		From: args.from(), To: args.To, Nonce: uint64(*args.Nonce), Value: value, GasLimit: uint64(*args.Gas),
		GasPrice: gasPrice, GasFeeCap: feeCap, GasTipCap: tipCap, Data: args.data(), AccessList: accessList,
		BlobGasFeeCap: blobFee, BlobHashes: args.BlobHashes, SetCodeAuthorizations: args.AuthorizationList,
		SkipNonceChecks: skipNonce, SkipTransactionChecks: true,
	}, nil
}

func applySimulationMessage(ctx context.Context, evm *vm.EVM, message *core.Message, gasPool *core.GasPool) (*core.ExecutionResult, error) {
	stop := context.AfterFunc(ctx, evm.Cancel)
	defer stop()
	result, err := core.ApplyMessage(evm, message, gasPool)
	if evm.Cancelled() {
		return nil, context.DeadlineExceeded
	}
	return result, err
}

func mapSimulationValidationError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &simulationRPCError{code: simErrTimeout, message: "simulation timed out"}
	case errors.Is(err, core.ErrNonceTooLow):
		return &simulationRPCError{code: simErrNonceTooLow, message: err.Error()}
	case errors.Is(err, core.ErrNonceTooHigh):
		return &simulationRPCError{code: simErrNonceTooHigh, message: err.Error()}
	case errors.Is(err, core.ErrFeeCapTooLow):
		return &simulationRPCError{code: simErrBaseFeeTooLow, message: err.Error()}
	case errors.Is(err, core.ErrIntrinsicGas), errors.Is(err, core.ErrFloorDataGas):
		return &simulationRPCError{code: simErrIntrinsicGas, message: err.Error()}
	case errors.Is(err, core.ErrInsufficientFunds), errors.Is(err, core.ErrInsufficientFundsForTransfer):
		return &simulationRPCError{code: simErrInsufficientFunds, message: err.Error()}
	case errors.Is(err, core.ErrGasLimitReached), errors.Is(err, core.ErrGasLimitTooHigh):
		return &simulationRPCError{code: simErrBlockGasLimit, message: err.Error()}
	case errors.Is(err, core.ErrSenderNoEOA):
		return &simulationRPCError{code: simErrSenderNotEOA, message: err.Error()}
	case errors.Is(err, vm.ErrMaxInitCodeSizeExceeded):
		return &simulationRPCError{code: simErrMaxInitCode, message: err.Error()}
	case errors.Is(err, core.ErrTipAboveFeeCap):
		return &simulationRPCError{code: simErrMaxFeeTooLow, message: err.Error()}
	case errors.Is(err, core.ErrFeeCapVeryHigh), errors.Is(err, core.ErrTipVeryHigh):
		return &invalidParamsError{message: err.Error()}
	default:
		return &simulationRPCError{code: -32603, message: err.Error()}
	}
}

func applySimulationStateOverrides(overrides *stateOverride, statedb *state.StateDB, precompiles vm.PrecompiledContracts) error {
	if overrides == nil {
		return nil
	}
	addresses := slices.SortedFunc(maps.Keys(*overrides), common.Address.Cmp)
	destinations := make(map[common.Address]common.Address)
	for _, address := range addresses {
		account := (*overrides)[address]
		if account.State != nil && account.StateDiff != nil {
			return &invalidParamsError{message: fmt.Sprintf("account %s has both state and stateDiff", address)}
		}
		precompile, isPrecompile := precompiles[address]
		if account.MovePrecompileTo != nil {
			destination := *account.MovePrecompileTo
			if destination == address {
				return &simulationRPCError{code: simErrMoveSelf, message: "movePrecompileToAddress references itself"}
			}
			if !isPrecompile {
				return &invalidInputError{message: fmt.Sprintf("account %s is not a precompile", address)}
			}
			if previous, exists := destinations[destination]; exists {
				return &simulationRPCError{code: simErrMoveDuplicate, message: fmt.Sprintf("precompiles %s and %s target %s", previous, address, destination)}
			}
			if _, overridden := (*overrides)[destination]; overridden {
				return &invalidInputError{message: fmt.Sprintf("move destination %s is also overridden", destination)}
			}
			destinations[destination] = address
			precompiles[destination] = precompile
		}
		if isPrecompile {
			delete(precompiles, address)
		}
		if account.Nonce != nil {
			statedb.SetNonce(address, uint64(*account.Nonce), tracing.NonceChangeUnspecified)
		}
		if account.Code != nil {
			statedb.SetCode(address, *account.Code, tracing.CodeChangeUnspecified)
		}
		if account.Balance != nil {
			balance, err := checkedU256("balance", account.Balance.ToInt())
			if err != nil {
				return &invalidParamsError{message: err.Error()}
			}
			statedb.SetBalance(address, balance, tracing.BalanceChangeUnspecified)
		}
		if account.State != nil {
			statedb.SetStorage(address, *account.State)
		}
		if account.StateDiff != nil {
			for key, value := range *account.StateDiff {
				statedb.SetState(address, key, value)
			}
		}
	}
	statedb.Finalise(false)
	return nil
}

type simulationChainContext struct {
	node    *Node
	base    *types.Header
	headers []*types.Header
}

func (chain *simulationChainContext) Config() *params.ChainConfig { return chain.node.chain.config }
func (chain *simulationChainContext) CurrentHeader() *types.Header {
	return chain.node.chain.blockchain.CurrentHeader()
}
func (chain *simulationChainContext) Engine() consensus.Engine {
	return chain.node.chain.blockchain.Engine()
}

func (chain *simulationChainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	header := chain.GetHeaderByNumber(number)
	if header == nil || header.Hash() != hash {
		return nil
	}
	return header
}

func (chain *simulationChainContext) GetHeaderByNumber(number uint64) *types.Header {
	if chain.base.Number.Uint64() == number {
		return chain.base
	}
	if number < chain.base.Number.Uint64() {
		return chain.node.chain.blockchain.GetHeaderByNumber(number)
	}
	for _, header := range chain.headers {
		if header.Number.Uint64() == number {
			return header
		}
	}
	return nil
}

func (chain *simulationChainContext) GetHeaderByHash(hash common.Hash) *types.Header {
	if chain.base.Hash() == hash {
		return chain.base
	}
	for _, header := range chain.headers {
		if header.Hash() == hash {
			return header
		}
	}
	return chain.node.chain.blockchain.GetHeaderByHash(hash)
}

func repairSimulationLogs(calls []simulationCallResult, blockHash common.Hash) {
	for callIndex := range calls {
		for logIndex := range calls[callIndex].Logs {
			calls[callIndex].Logs[logIndex].BlockHash = blockHash
		}
	}
}

var (
	simulationTransferTopic   = common.HexToHash("ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	simulationTransferAddress = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")
)

type simulationTracer struct {
	logs           [][]*types.Log
	count          int
	traceTransfers bool
	blockNumber    uint64
	blockTimestamp uint64
	txHash         common.Hash
	txIndex        uint
}

func newSimulationTracer(traceTransfers bool, blockNumber, blockTimestamp uint64) *simulationTracer {
	return &simulationTracer{traceTransfers: traceTransfers, blockNumber: blockNumber, blockTimestamp: blockTimestamp}
}

func (tracer *simulationTracer) Hooks() *tracing.Hooks {
	return &tracing.Hooks{OnEnter: tracer.onEnter, OnExit: tracer.onExit, OnLog: tracer.onLog}
}

func (tracer *simulationTracer) onEnter(_ int, operation byte, from, to common.Address, _ []byte, _ uint64, value *big.Int) {
	tracer.logs = append(tracer.logs, make([]*types.Log, 0))
	if vm.OpCode(operation) != vm.DELEGATECALL && vm.OpCode(operation) != vm.CALLCODE && value != nil && value.Sign() > 0 {
		tracer.captureTransfer(from, to, value)
	}
}

func (tracer *simulationTracer) onExit(depth int, _ []byte, _ uint64, _ error, reverted bool) {
	if depth == 0 {
		if reverted && len(tracer.logs) > 0 {
			tracer.logs[0] = nil
		}
		return
	}
	last := len(tracer.logs) - 1
	if last < 1 {
		return
	}
	logs := tracer.logs[last]
	tracer.logs = tracer.logs[:last]
	if !reverted {
		tracer.logs[last-1] = append(tracer.logs[last-1], logs...)
	}
}

func (tracer *simulationTracer) onLog(log *types.Log) {
	tracer.captureLog(log.Address, log.Topics, log.Data)
}

func (tracer *simulationTracer) captureTransfer(from, to common.Address, value *big.Int) {
	if !tracer.traceTransfers {
		return
	}
	tracer.captureLog(simulationTransferAddress, []common.Hash{simulationTransferTopic, common.BytesToHash(from.Bytes()), common.BytesToHash(to.Bytes())}, common.BigToHash(value).Bytes())
}

func (tracer *simulationTracer) captureLog(address common.Address, topics []common.Hash, data []byte) {
	if len(tracer.logs) == 0 {
		return
	}
	tracer.logs[len(tracer.logs)-1] = append(tracer.logs[len(tracer.logs)-1], &types.Log{
		Address: address, Topics: topics, Data: data, BlockNumber: tracer.blockNumber,
		BlockTimestamp: tracer.blockTimestamp, TxHash: tracer.txHash, TxIndex: tracer.txIndex, Index: uint(tracer.count),
	})
	tracer.count++
}

func (tracer *simulationTracer) Reset(hash common.Hash, index uint) {
	tracer.logs = nil
	tracer.txHash = hash
	tracer.txIndex = index
}

func (tracer *simulationTracer) Logs() []*types.Log {
	if len(tracer.logs) == 0 {
		return []*types.Log{}
	}
	return tracer.logs[0]
}
