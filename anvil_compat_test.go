package ethertest

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func compatNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := node.Close(); err != nil {
			t.Error(err)
		}
	})
	return node
}
func compatCall(t *testing.T, client *rpc.Client, method string, args ...any) json.RawMessage {
	t.Helper()
	var result json.RawMessage
	if err := client.Call(&result, method, args...); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return result
}
func TestAnvilAdapters(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node := compatNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	address := common.HexToAddress("0x1234")
	for _, test := range []struct {
		method string
		args   []any
		want   string
	}{
		{"anvil_setBalance", []any{address, 42}, "null"},
		{"anvil_setNonce", []any{address, "0x3"}, "null"},
		{"anvil_setCode", []any{address, "0x00"}, "null"},
		{"anvil_setStorageAt", []any{address, "0x0", common.HexToHash("0x1")}, "true"},
	} {
		before := node.chain.blockchain.CurrentBlock().Number.Uint64()
		if got := string(compatCall(t, client, test.method, test.args...)); got != test.want {
			t.Fatalf("%s=%s", test.method, got)
		}
		if node.chain.blockchain.CurrentBlock().Number.Uint64() != before+1 {
			t.Fatal("setter must create a control block")
		}
	}
	if !node.SafetyStatus().HeadTainted {
		t.Fatal("missing setter taint")
	}
	if got := string(compatCall(t, client, "eth_getBalance", address, "latest")); got != `"0x2a"` {
		t.Fatal(got)
	}
	var proof accountProof
	if err := client.Call(&proof, "eth_getProof", address, []common.Hash{{}}, "latest"); err != nil {
		t.Fatal(err)
	}
	if (*big.Int)(proof.Balance).Uint64() != 42 {
		t.Fatal("proof balance")
	}
	before := node.chain.blockchain.CurrentBlock().Hash()
	rev := node.Revision()
	for _, test := range []struct {
		method string
		args   []any
	}{
		{"anvil_setBalance", []any{address, -1}},
		{"anvil_setBalance", []any{address, "0x1" + strings.Repeat("0", 64)}},
		{"anvil_setNonce", []any{address, "0x10000000000000000"}},
		{"anvil_setStorageAt", []any{address, "0x0", "0x1"}},
		{"anvil_mine", []any{2, 0}},
		{"evm_mine", []any{1}},
		{"evm_mine", []any{map[string]any{"blocks": 1, "timestamp": 1}}},
		{"evm_mine", []any{map[string]any{"unknown": 1}}},
		{"evm_mine", []any{map[string]any{"blocks": 1}}},
		{"anvil_setBalance", []any{address, json.RawMessage("18446744073709551616")}},
	} {
		var result any
		assertRPCErrorCode(t, client.Call(&result, test.method, test.args...), -32602)
	}
	if node.chain.blockchain.CurrentBlock().Hash() != before || node.Revision() != rev {
		t.Fatal("invalid call changed state")
	}
	if got := string(compatCall(t, client, "anvil_mine", 2, 6)); got != "null" {
		t.Fatal(got)
	}
	timestamp := node.chain.genesisTime + (node.chain.currentSlot()+1)*node.chain.slotDuration
	if got := string(compatCall(t, client, "evm_mine", map[string]any{"timestamp": timestamp, "blocks": 2})); got != `"0x0"` {
		t.Fatal(got)
	}
	snapshot := compatCall(t, client, "evm_snapshot")
	compatCall(t, client, "anvil_setBalance", address, 99)
	if got := string(compatCall(t, client, "evm_revert", snapshot)); got != "true" {
		t.Fatal(got)
	}
	if got := string(compatCall(t, client, "evm_revert", snapshot)); got != "false" {
		t.Fatal(got)
	}
	if got := string(compatCall(t, client, "eth_getBalance", address, "latest")); got != `"0x2a"` {
		t.Fatal(got)
	}
	path := filepath.Join(t.TempDir(), "state.tar.zst")
	if err := node.DumpState(path); err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectState(path)
	if err != nil || !manifest.Tainted || !manifest.HeadTainted || manifest.Secrets {
		t.Fatal("control archive safety", err)
	}
	destination := filepath.Join(t.TempDir(), "restored")
	if err := LoadState(path, destination); err != nil {
		t.Fatal(err)
	}
	restoredConfig := cfg
	restoredConfig.Storage.Engine = "pebble"
	restoredConfig.Storage.Path = destination
	restored := compatNode(t, restoredConfig)
	restoredClient := restored.RPCClient()
	defer restoredClient.Close()
	if got := string(compatCall(t, restoredClient, "eth_getBalance", address, "latest")); got != `"0x2a"` {
		t.Fatal("archive balance", got)
	}
	if !restored.SafetyStatus().HeadTainted {
		t.Fatal("archive lost taint")
	}
	// Namespace-specific extensions remain explicitly available without leaking new controls.
	compatCall(t, client, "anvil_capabilities")
	var result any
	assertRPCErrorCode(t, client.Call(&result, "anvil_pauseFinality"), -32601)
}

func TestAnvilRuntimeControls(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node := compatNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	account := testWalletAccount(t, node, 0)
	to := node.Accounts()[1]
	tx0 := signedDynamicTransaction(t, cfg, account, 0, to, big.NewInt(1), nil)
	tx1 := signedDynamicTransaction(t, cfg, account, 1, to, big.NewInt(1), nil)
	for _, tx := range []*types.Transaction{tx0, tx1} {
		if _, err := node.SendTransaction(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
	}
	oldView := node.chain.pendingView
	original := node.chain.db
	node.chain.db = failingPendingProjectionDatabase{Database: original}
	if _, err := node.DropTransaction(context.Background(), tx0.Hash()); err == nil {
		t.Fatal("expected rebuild failure")
	}
	node.chain.db = original
	if node.chain.pendingCount() != 2 || node.chain.pendingView != oldView {
		t.Fatal("failed delete changed pool")
	}
	if got := string(compatCall(t, client, "anvil_dropTransaction", tx0.Hash())); got != `"`+tx0.Hash().Hex()+`"` {
		t.Fatal(got)
	}
	if node.chain.pendingCount() != 1 || len(node.chain.pendingView.executable) != 0 {
		t.Fatal("nonce successor must queue")
	}
	if got := string(compatCall(t, client, "anvil_dropTransaction", tx0.Hash())); got != "null" {
		t.Fatal(got)
	}
	compatCall(t, client, "txpool_inspect")
	compatCall(t, client, "anvil_removePoolTransactions", account.Address)
	if node.chain.pendingCount() != 0 {
		t.Fatal("account pool not cleared")
	}
	if _, err := node.SendTransaction(context.Background(), tx0); err != nil {
		t.Fatal(err)
	}
	compatCall(t, client, "anvil_setAutomine", true)
	if !node.Automine() || node.chain.pendingCount() != 0 || node.chain.blockchain.CurrentBlock().Number.Uint64() != 1 {
		t.Fatal("automine did not drain existing transactions")
	}
	compatCall(t, client, "evm_setIntervalMining", 3600)
	if node.Automine() || string(compatCall(t, client, "anvil_getIntervalMining")) != "3600" {
		t.Fatal("interval mode")
	}
	compatCall(t, client, "evm_setAutomine", false)
	if node.IntervalMining() != time.Hour {
		t.Fatal("disabling automine must preserve interval mode")
	}
	compatCall(t, client, "anvil_setIntervalMining", 0)
	if node.IntervalMining() != 0 || string(compatCall(t, client, "anvil_getIntervalMining")) != "null" {
		t.Fatal("manual mode")
	}
	compatCall(t, client, "anvil_setCoinbase", to)
	if node.chain.pendingBlock().Coinbase() != to {
		t.Fatal("pending coinbase stale")
	}
	if got := string(compatCall(t, client, "anvil_getGenesisTime")); got != "1800000000" {
		t.Fatal(got)
	}
	compatCall(t, client, "anvil_dropAllTransactions")
	before := node.chain.blockchain.CurrentBlock().Number.Uint64()
	compatCall(t, client, "anvil_setIntervalMining", 1)
	deadline := time.Now().Add(3 * time.Second)
	for node.chain.blockchain.CurrentBlock().Number.Uint64() == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	compatCall(t, client, "anvil_setIntervalMining", 0)
	if node.chain.blockchain.CurrentBlock().Number.Uint64() == before {
		t.Fatal("interval did not mine empty block")
	}
}

func TestCallBlockOverridesReadOnly(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node := compatNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	address := common.HexToAddress("0x1234")
	before := node.chain.blockchain.CurrentBlock().Hash()
	revision := node.Revision()
	pending := node.chain.pendingView
	for _, test := range []struct {
		key    string
		opcode byte
		value  string
	}{
		{"number", 0x43, "0x123"}, {"time", 0x42, "0x70000000"}, {"gasLimit", 0x45, "0x100000"},
		{"feeRecipient", 0x41, common.HexToAddress("0x1234").Hex()}, {"prevRandao", 0x44, common.HexToHash("0x123").Hex()},
		{"baseFeePerGas", 0x48, "0x123"}, {"blobBaseFee", 0x4a, "0x123"},
	} {
		code := hexutil.Encode([]byte{test.opcode, 0x60, 0, 0x52, 0x60, 0x20, 0x60, 0, 0xf3})
		overrides := map[string]any{address.Hex(): map[string]any{"code": code}}
		args := map[string]any{"to": address}
		var result hexutil.Bytes
		if err := client.Call(&result, "eth_call", args, "latest", overrides, map[string]any{test.key: test.value}); err != nil {
			t.Fatalf("%s: %v", test.key, err)
		}
		if common.BytesToHash(result) != common.HexToHash(test.value) {
			t.Fatalf("%s=%x", test.key, result)
		}
		var gas hexutil.Uint64
		if err := client.Call(&gas, "eth_estimateGas", args, "latest", overrides, map[string]any{test.key: test.value}); err != nil || gas < 21000 {
			t.Fatalf("estimate %s: %v %d", test.key, err, gas)
		}
	}
	for _, override := range []map[string]any{{"withdrawals": []any{}}, {"unknown": "0x1"}, {"number": "0x10000000000000000"}, {"baseFeePerGas": "-0x1"}} {
		var result any
		assertRPCErrorCode(t, client.Call(&result, "eth_call", map[string]any{"to": address}, "latest", nil, override), -32602)
	}
	if before != node.chain.blockchain.CurrentBlock().Hash() || revision != node.Revision() || pending != node.chain.pendingView || node.SafetyStatus().SessionTainted {
		t.Fatal("read-only overrides mutated node")
	}
}

func TestAnvilBlobPoolDeletionRetainsHistory(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node := compatNode(t, cfg)
	account := testWalletAccount(t, node, 0)
	blob, err := EncodePackedBytesV1([]byte("compat retained blob"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := SignBlobTransaction(BlobTransactionRequest{
		ChainID: new(big.Int).SetUint64(cfg.Chain.ChainID), Nonce: 0, To: common.Address{}, Gas: 100000,
		GasTipCap: big.NewInt(1000000000), GasFeeCap: big.NewInt(3000000000), BlobFeeCap: big.NewInt(1000000000), Blob: blob,
	}, account.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := node.DropAllTransactions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if node.chain.blobSidecarForVersion(tx.Hash(), types.BlobSidecarVersion0) != nil {
		t.Fatal("unretained blob cache leaked")
	}
	snapshot, err := node.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	if ok, err := node.Revert(t.Context(), snapshot); err != nil || !ok {
		t.Fatal("revert", err)
	}
	if _, err := node.SendTransaction(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := node.DropAllTransactions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if node.chain.blobSidecarForVersion(tx.Hash(), types.BlobSidecarVersion0) == nil {
		t.Fatal("historical blob removed with pending tx")
	}
}

func TestAnvilControlsAcrossAmsterdamBoundary(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Chain.Forks.AmsterdamEpoch = 1
	node := compatNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	compatCall(t, client, "anvil_mine", cfg.Chain.SlotsPerEpoch-1, uint64(cfg.Chain.SlotDuration/time.Second))
	compatCall(t, client, "anvil_setBalance", common.HexToAddress("0x1234"), 42)
	block := node.chain.blockchain.GetBlockByNumber(cfg.Chain.SlotsPerEpoch)
	if block.Header().BlockAccessListHash == nil || node.consensus.forkName(node.chain.currentSlot()) != "gloas" {
		t.Fatal("unpaired Amsterdam/Gloas boundary")
	}
	if !node.SafetyStatus().HeadTainted {
		t.Fatal("boundary control lost taint")
	}
}

func TestAnvilRuntimeSettingsAreNotPersisted(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "db")
	first := compatNode(t, cfg)
	if err := first.SetIntervalMining(t.Context(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := first.SetFeeRecipient(t.Context(), common.HexToAddress("0x1234")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := compatNode(t, cfg)
	if second.IntervalMining() != 0 || second.Automine() || second.chain.feeRecipientAddress() == common.HexToAddress("0x1234") {
		t.Fatal("runtime settings persisted")
	}
}

func TestAnvilMiningRangeOverflowIsAtomic(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node := compatNode(t, cfg)
	// Exercise arithmetic before any block construction near the uint64 limit.
	originalSlot := node.chain.slot
	node.chain.mu.Lock()
	node.chain.slot = (math.MaxUint64-node.chain.genesisTime)/node.chain.slotDuration - 1
	node.chain.mu.Unlock()
	head := node.chain.blockchain.CurrentBlock().Hash()
	revision := node.Revision()
	if err := node.mineCompatible(t.Context(), 2, nil, nil); err == nil {
		t.Fatal("overflow accepted")
	}
	if err := node.mineCompatible(t.Context(), 0, nil, nil); err != nil {
		t.Fatal("zero count", err)
	}
	node.chain.mu.Lock()
	node.chain.slot = originalSlot
	node.chain.mu.Unlock()
	if head != node.chain.blockchain.CurrentBlock().Hash() || revision != node.Revision() {
		t.Fatal("overflow partially mined")
	}
}

func TestAnvilNumberFormats(t *testing.T) {
	for _, test := range []struct {
		input string
		value string
	}{
		{`42`, "42"}, {`"42"`, "42"}, {`"0X2a"`, "42"}, {`"0b10"`, "2"}, {`"0o10"`, "8"}, {`"0x1_0"`, "16"}, {`""`, "0"}, {`"0x"`, "0"},
		{`"0x10000000000000000"`, "18446744073709551616"},
	} {
		var n anvilNumber
		if err := json.Unmarshal([]byte(test.input), &n); err != nil || (*big.Int)(&n).String() != test.value {
			t.Errorf("%s: %v, %s", test.input, err, (*big.Int)(&n))
		}
	}
	for _, input := range []string{`-1`, `1.1`, `18446744073709551616`, `"+1"`, `"abc"`, `"0b2"`, `null`} {
		var n anvilNumber
		if err := json.Unmarshal([]byte(input), &n); err == nil {
			t.Errorf("accepted %s", input)
		}
	}
}
