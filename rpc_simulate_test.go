package ethertest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

type simulateWireBlock struct {
	Number       hexutil.Uint64         `json:"number"`
	Hash         common.Hash            `json:"hash"`
	MixHash      common.Hash            `json:"mixHash"`
	Timestamp    hexutil.Uint64         `json:"timestamp"`
	GasUsed      hexutil.Uint64         `json:"gasUsed"`
	Transactions []json.RawMessage      `json:"transactions"`
	Calls        []simulationCallResult `json:"calls"`
}

func TestSimulateV1SequentialStateTransfersAndNoPersistence(t *testing.T) {
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()
	headBefore := node.chain.blockchain.CurrentBlock()
	revisionBefore := node.Revision()

	from := common.HexToAddress("0xc000000000000000000000000000000000000000")
	middle := common.HexToAddress("0xc100000000000000000000000000000000000000")
	to := common.HexToAddress("0xc200000000000000000000000000000000000000")
	payload := map[string]any{
		"blockStateCalls": []any{
			map[string]any{
				"stateOverrides": map[common.Address]any{from: map[string]any{"balance": "0xa"}},
				"calls":          []any{map[string]any{"from": from, "to": middle, "value": "0x5"}},
			},
			map[string]any{"calls": []any{map[string]any{"from": middle, "to": to, "value": "0x5"}}},
		},
		"traceTransfers": true,
	}
	var result []simulateWireBlock
	if err := client.Call(&result, "eth_simulateV1", payload, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || len(result[0].Calls) != 1 || len(result[1].Calls) != 1 {
		t.Fatalf("simulation result = %#v", result)
	}
	for blockIndex := range result {
		call := result[blockIndex].Calls[0]
		if uint64(call.Status) != types.ReceiptStatusSuccessful || call.Error != nil || len(call.Logs) != 1 {
			t.Fatalf("block %d call = %#v", blockIndex, call)
		}
		if call.Logs[0].Address != simulationTransferAddress || call.Logs[0].BlockHash != result[blockIndex].Hash {
			t.Fatalf("block %d transfer log = %#v", blockIndex, call.Logs[0])
		}
	}
	if uint64(result[0].Timestamp) != headBefore.Time+node.chain.slotDuration || uint64(result[1].Timestamp) != headBefore.Time+2*node.chain.slotDuration {
		t.Fatalf("simulated timestamps = %d/%d", result[0].Timestamp, result[1].Timestamp)
	}
	if len(result[0].Transactions) != 1 || len(result[0].Transactions[0]) == 0 || result[0].Transactions[0][0] != '"' {
		t.Fatalf("default transaction result is not a hash: %s", result[0].Transactions[0])
	}
	if node.chain.blockchain.CurrentBlock().Hash() != headBefore.Hash() || node.Revision() != revisionBefore {
		t.Fatal("simulation changed canonical head or revision")
	}
	stateAfter, err := node.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if stateAfter.GetBalance(from).Sign() != 0 || stateAfter.GetBalance(middle).Sign() != 0 || stateAfter.GetBalance(to).Sign() != 0 {
		t.Fatal("simulation state escaped into the canonical state")
	}
}

func TestSimulateV1GapFillOverridesAndFullTransactions(t *testing.T) {
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()
	head := node.chain.blockchain.CurrentBlock()
	from := common.HexToAddress("0xc000000000000000000000000000000000000001")
	to := common.HexToAddress("0xc100000000000000000000000000000000000001")
	feeRecipient := common.HexToAddress("0xc200000000000000000000000000000000000001")
	payload := map[string]any{
		"returnFullTransactions": true,
		"blockStateCalls": []any{map[string]any{
			"blockOverrides": map[string]any{
				"number": hexutil.Uint64(head.Number.Uint64() + 3), "time": hexutil.Uint64(head.Time + 3*node.chain.slotDuration),
				"feeRecipient": feeRecipient, "baseFeePerGas": "0x0",
			},
			"stateOverrides": map[common.Address]any{from: map[string]any{"balance": "0x1"}},
			"calls":          []any{map[string]any{"from": from, "to": to, "value": "0x1"}},
		}},
	}
	var result []simulateWireBlock
	if err := client.Call(&result, "eth_simulateV1", payload, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 3 || uint64(result[0].Number) != head.Number.Uint64()+1 || uint64(result[2].Number) != head.Number.Uint64()+3 {
		t.Fatalf("gap-filled blocks = %#v", result)
	}
	if len(result[0].Calls) != 0 || len(result[1].Calls) != 0 || len(result[2].Calls) != 1 {
		t.Fatalf("gap calls = %d/%d/%d", len(result[0].Calls), len(result[1].Calls), len(result[2].Calls))
	}
	if len(result[2].Transactions) != 1 || len(result[2].Transactions[0]) == 0 || result[2].Transactions[0][0] != '{' {
		t.Fatalf("full transaction = %s", result[2].Transactions[0])
	}
	var transaction rpcTransaction
	if err := json.Unmarshal(result[2].Transactions[0], &transaction); err != nil {
		t.Fatal(err)
	}
	if transaction.From != from || transaction.BlockHash == nil || *transaction.BlockHash != result[2].Hash {
		t.Fatalf("full simulated transaction = %#v", transaction)
	}
}

func TestSimulateV1AllowsEmptyContractCreationCall(t *testing.T) {
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()

	var result []simulateWireBlock
	if err := client.Call(&result, "eth_simulateV1", map[string]any{
		"returnFullTransactions": true,
		"blockStateCalls":        []any{map[string]any{"calls": []any{map[string]any{}}}},
	}, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || len(result[0].Calls) != 1 || uint64(result[0].Calls[0].Status) != types.ReceiptStatusSuccessful || len(result[0].Transactions) != 1 {
		t.Fatalf("empty creation simulation = %#v", result)
	}
	var transaction rpcTransaction
	if err := json.Unmarshal(result[0].Transactions[0], &transaction); err != nil {
		t.Fatal(err)
	}
	if transaction.Type != types.DynamicFeeTxType || transaction.To != nil || transaction.BlockHash == nil || *transaction.BlockHash != result[0].Hash {
		t.Fatalf("empty creation transaction = %#v", transaction)
	}

	to := common.HexToAddress("0xc100000000000000000000000000000000000099")
	if err := client.Call(&result, "eth_simulateV1", map[string]any{
		"validation":             true,
		"returnFullTransactions": true,
		"blockStateCalls": []any{map[string]any{
			"blockOverrides": map[string]any{"baseFeePerGas": "0x0", "prevRandao": "0x1"},
			"calls": []any{map[string]any{
				"to": to, "maxFeePerGas": "0x0", "maxPriorityFeePerGas": "0x0", "maxFeePerBlobGas": "0x0",
			}},
		}},
	}, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].MixHash != common.HexToHash("0x1") || len(result[0].Transactions) != 1 {
		t.Fatalf("maxFeePerBlobGas-only simulation = %#v", result)
	}
	if err := json.Unmarshal(result[0].Transactions[0], &transaction); err != nil {
		t.Fatal(err)
	}
	if transaction.Type != types.DynamicFeeTxType || transaction.MaxFeePerBlobGas != nil {
		t.Fatalf("maxFeePerBlobGas without hashes changed transaction type: %#v", transaction)
	}
}

func TestSimulateV1ValidationAndExecutionErrors(t *testing.T) {
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()
	from := common.HexToAddress("0xc000000000000000000000000000000000000010")
	to := common.HexToAddress("0xc100000000000000000000000000000000000010")

	call := func(payload map[string]any) error {
		var result json.RawMessage
		return client.Call(&result, "eth_simulateV1", payload, "latest")
	}
	stateWithFunds := map[common.Address]any{from: map[string]any{"balance": "0x100000000000000000"}}
	assertRPCErrorCode(t, call(map[string]any{
		"validation":      true,
		"blockStateCalls": []any{map[string]any{"stateOverrides": stateWithFunds, "calls": []any{map[string]any{"from": from, "to": to}}}},
	}), simErrBaseFeeTooLow)
	assertRPCErrorCode(t, call(map[string]any{
		"blockStateCalls": []any{map[string]any{"calls": []any{map[string]any{
			"from": from, "to": to, "maxFeePerGas": "0x1", "maxPriorityFeePerGas": "0x2",
		}}}},
	}), simErrMaxFeeTooLow)
	assertRPCErrorCode(t, call(map[string]any{
		"blockStateCalls": []any{map[string]any{"calls": []any{map[string]any{"from": from, "to": to, "value": "0x1"}}}},
	}), simErrInsufficientFunds)
	assertRPCErrorCode(t, call(map[string]any{
		"validation": true,
		"blockStateCalls": []any{map[string]any{"stateOverrides": stateWithFunds, "calls": []any{map[string]any{
			"from": from, "to": to, "nonce": "0x1", "maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x0",
		}}}},
	}), simErrNonceTooHigh)
	assertRPCErrorCode(t, call(map[string]any{
		"validation": true,
		"blockStateCalls": []any{map[string]any{"stateOverrides": stateWithFunds, "calls": []any{
			map[string]any{"from": from, "to": to, "nonce": "0x0", "maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x0"},
			map[string]any{"from": from, "to": to, "nonce": "0x0", "maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x0"},
		}}},
	}), simErrNonceTooLow)
	assertRPCErrorCode(t, call(map[string]any{
		"blockStateCalls": []any{map[string]any{"stateOverrides": stateWithFunds, "calls": []any{map[string]any{"from": from, "to": to, "gas": "0x1"}}}},
	}), simErrIntrinsicGas)
	assertRPCErrorCode(t, call(map[string]any{
		"blockStateCalls": []any{map[string]any{
			"blockOverrides": map[string]any{"gasLimit": "0x5208"}, "stateOverrides": stateWithFunds,
			"calls": []any{map[string]any{"from": from, "to": to, "gas": "0x5208"}, map[string]any{"from": from, "to": to, "gas": "0x5208"}},
		}},
	}), simErrBlockGasLimit)

	invalid := common.HexToAddress("0xc200000000000000000000000000000000000010")
	var result []simulateWireBlock
	if err := client.Call(&result, "eth_simulateV1", map[string]any{
		"blockStateCalls": []any{map[string]any{
			"stateOverrides": map[common.Address]any{invalid: map[string]any{"code": "0xfe"}},
			"calls":          []any{map[string]any{"to": invalid}},
		}},
	}, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || len(result[0].Calls) != 1 || uint64(result[0].Calls[0].Status) != types.ReceiptStatusFailed || result[0].Calls[0].Error == nil || result[0].Calls[0].Error.Code != simErrVM {
		t.Fatalf("VM failure result = %#v", result)
	}
	reverter := common.HexToAddress("0xc300000000000000000000000000000000000010")
	if err := client.Call(&result, "eth_simulateV1", map[string]any{
		"traceTransfers": true,
		"blockStateCalls": []any{map[string]any{
			"stateOverrides": map[common.Address]any{
				from: map[string]any{"balance": "0x1"}, reverter: map[string]any{"code": "0x60006000fd"},
			},
			"calls": []any{map[string]any{"from": from, "to": reverter, "value": "0x1"}},
		}},
	}, "latest"); err != nil {
		t.Fatal(err)
	}
	if result[0].Calls[0].Error == nil || result[0].Calls[0].Error.Code != 3 || len(result[0].Calls[0].Logs) != 0 {
		t.Fatalf("revert result = %#v", result[0].Calls[0])
	}
}

func TestSimulateV1PrecompileMovesAndSequenceErrors(t *testing.T) {
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()
	identity := common.BytesToAddress([]byte{4})
	sha256Address := common.BytesToAddress([]byte{2})
	destination := common.HexToAddress("0x1000000000000000000000000000000000000100")

	var result []simulateWireBlock
	if err := client.Call(&result, "eth_simulateV1", map[string]any{
		"blockStateCalls": []any{map[string]any{
			"stateOverrides": map[common.Address]any{identity: map[string]any{"movePrecompileToAddress": destination}},
			"calls":          []any{map[string]any{"to": destination, "input": "0x0102"}},
		}},
	}, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || len(result[0].Calls) != 1 || string(result[0].Calls[0].ReturnData) != string([]byte{1, 2}) {
		t.Fatalf("moved identity result = %#v", result)
	}

	call := func(payload map[string]any) error {
		var raw json.RawMessage
		return client.Call(&raw, "eth_simulateV1", payload, "latest")
	}
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{
		"stateOverrides": map[common.Address]any{identity: map[string]any{"movePrecompileToAddress": identity}},
	}}}), simErrMoveSelf)
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{
		"stateOverrides": map[common.Address]any{
			identity: map[string]any{"movePrecompileToAddress": destination}, sha256Address: map[string]any{"movePrecompileToAddress": destination},
		},
	}}}), simErrMoveDuplicate)
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{
		"stateOverrides": map[common.Address]any{destination: map[string]any{"movePrecompileToAddress": identity}},
	}}}), -32000)

	head := node.chain.blockchain.CurrentBlock()
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{
		"blockOverrides": map[string]any{"number": hexutil.Uint64(head.Number.Uint64())},
	}}}), simErrBlockNumber)
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{
		"blockOverrides": map[string]any{"time": hexutil.Uint64(head.Time)},
	}}}), simErrBlockTimestamp)
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{
		"blockOverrides": map[string]any{"number": hexutil.Uint64(head.Number.Uint64() + maxSimulateBlocks + 1)},
	}}}), -38026)
}

func TestSimulateV1ResourceLimitsAndTimeout(t *testing.T) {
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()
	call := func(payload map[string]any) error {
		var raw json.RawMessage
		return client.Call(&raw, "eth_simulateV1", payload, "latest")
	}
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{}}), -32602)
	tooManyBlocks := make([]any, maxSimulateBlocks+1)
	for index := range tooManyBlocks {
		tooManyBlocks[index] = map[string]any{}
	}
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": tooManyBlocks}), -38026)
	tooManyCalls := make([]any, maxSimulateCallsPerBlock+1)
	for index := range tooManyCalls {
		tooManyCalls[index] = map[string]any{}
	}
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{map[string]any{"calls": tooManyCalls}}}), -38026)
	totalCalls := make([]any, 4000)
	for index := range totalCalls {
		totalCalls[index] = map[string]any{}
	}
	lastCalls := make([]any, 2001)
	for index := range lastCalls {
		lastCalls[index] = map[string]any{}
	}
	assertRPCErrorCode(t, call(map[string]any{"blockStateCalls": []any{
		map[string]any{"calls": totalCalls}, map[string]any{"calls": totalCalls}, map[string]any{"calls": lastCalls},
	}}), -38026)

	budget := simulationGasBudget{remaining: 1}
	assertRPCErrorCode(t, budget.consume(2), -38026)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	api := &ethAPI{node: node}
	_, err := api.SimulateV1(expired, simulationPayload{BlockStateCalls: []simulationBlock{{}}}, nil)
	assertRPCErrorCode(t, err, simErrTimeout)
}

func TestSimulationValidationErrorCodeMapping(t *testing.T) {
	tests := []struct {
		err  error
		code int
	}{
		{coreErrorNonceLow(), simErrNonceTooLow},
		{coreErrorNonceHigh(), simErrNonceTooHigh},
		{fmt.Errorf("fee: %w", core.ErrTipAboveFeeCap), simErrMaxFeeTooLow},
		{coreErrorFeeCapLow(), simErrBaseFeeTooLow},
		{coreErrorIntrinsicGas(), simErrIntrinsicGas},
		{coreErrorInsufficientFunds(), simErrInsufficientFunds},
		{fmt.Errorf("gas: %w", core.ErrGasLimitReached), simErrBlockGasLimit},
		{fmt.Errorf("sender: %w", core.ErrSenderNoEOA), simErrSenderNotEOA},
		{fmt.Errorf("initcode: %w", vm.ErrMaxInitCodeSizeExceeded), simErrMaxInitCode},
		{fmt.Errorf("unclassified validation failure"), -32603},
	}
	for _, test := range tests {
		assertRPCErrorCode(t, mapSimulationValidationError(test.err), test.code)
	}
}

// These wrappers keep the table above tied to the public core sentinels while
// producing messages representative of the wrapped errors returned by the EVM.
func coreErrorNonceLow() error          { return fmt.Errorf("nonce: %w", core.ErrNonceTooLow) }
func coreErrorNonceHigh() error         { return fmt.Errorf("nonce: %w", core.ErrNonceTooHigh) }
func coreErrorFeeCapLow() error         { return fmt.Errorf("fee: %w", core.ErrFeeCapTooLow) }
func coreErrorIntrinsicGas() error      { return fmt.Errorf("gas: %w", core.ErrIntrinsicGas) }
func coreErrorInsufficientFunds() error { return fmt.Errorf("funds: %w", core.ErrInsufficientFunds) }
