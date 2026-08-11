package ethertest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.Chain.GenesisTime = 1_800_000_000
	cfg.HTTP.Enabled = false
	cfg.Beacon.Enabled = false
	return cfg
}

func testWalletAccount(t *testing.T, node *Node, index int) Account {
	t.Helper()
	addresses := node.Accounts()
	if index < 0 || index >= len(addresses) {
		t.Fatalf("wallet account index %d out of bounds", index)
	}
	return testAccountFromWallet(t, node.wallet, addresses[index])
}

func testAccountFromWallet(t *testing.T, wallet *memoryWallet, address common.Address) Account {
	t.Helper()
	wallet.mu.RLock()
	entry, exists := wallet.entries[address]
	wallet.mu.RUnlock()
	if !exists {
		t.Fatalf("wallet account %s is unavailable", address)
	}
	account, err := cloneAccount(entry.account)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func testWalletAccounts(t *testing.T, node *Node) []Account {
	t.Helper()
	addresses := node.Accounts()
	accounts := make([]Account, len(addresses))
	for index := range addresses {
		accounts[index] = testWalletAccount(t, node, index)
	}
	return accounts
}

func TestExecutionChainConfigIsRepositoryOwnedAndStopsAtOsaka(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.Forks.PragueEpoch = 1
	cfg.Chain.Forks.OsakaEpoch = 2
	chainConfig := executionChainConfig(cfg)

	if err := chainConfig.CheckConfigForkOrder(); err != nil {
		t.Fatal(err)
	}
	wantOsaka := uint64(cfg.Chain.GenesisTime) +
		cfg.Chain.Forks.OsakaEpoch*cfg.Chain.SlotsPerEpoch*uint64(cfg.Chain.SlotDuration/time.Second)
	if chainConfig.OsakaTime == nil || *chainConfig.OsakaTime != wantOsaka {
		t.Fatalf("Osaka activation is %v, want %d", chainConfig.OsakaTime, wantOsaka)
	}
	if chainConfig.UBTTime != nil ||
		chainConfig.BPO1Time != nil ||
		chainConfig.BPO2Time != nil ||
		chainConfig.BPO3Time != nil ||
		chainConfig.BPO4Time != nil ||
		chainConfig.BPO5Time != nil ||
		chainConfig.AmsterdamTime != nil ||
		chainConfig.BogotaTime != nil ||
		chainConfig.EnableUBTAtGenesis {
		t.Fatal("execution config inherited a post-Osaka development fork")
	}
	if chainConfig.BlobScheduleConfig == nil ||
		chainConfig.BlobScheduleConfig.Cancun == nil ||
		chainConfig.BlobScheduleConfig.Prague == nil {
		t.Fatal("execution config is missing its repository-owned blob schedule")
	}
	cancunBlobs := chainConfig.BlobScheduleConfig.Cancun
	pragueBlobs := chainConfig.BlobScheduleConfig.Prague
	if cancunBlobs.Target != 3 || cancunBlobs.Max != 6 || cancunBlobs.UpdateFraction != 3_338_477 ||
		pragueBlobs.Target != 6 || pragueBlobs.Max != 9 || pragueBlobs.UpdateFraction != 5_007_716 {
		t.Fatal("execution config changed its pinned blob schedule")
	}
}

func TestSignedTransferAutoMinesAtomically(t *testing.T) {
	cfg := testConfig()
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	accounts, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	chainID := new(big.Int).SetUint64(DefaultChainID)
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: 0,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		Gas: 21_000, To: &accounts[1].Address, Value: big.NewInt(1), Data: nil,
	})
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(chainID), accounts[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := node.SendTransaction(context.Background(), signed)
	if err != nil {
		t.Fatal(err)
	}
	if have := node.chain.blockchain.CurrentBlock().Number.Uint64(); have != 1 {
		t.Fatalf("head is %d, want 1", have)
	}
	state, err := node.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if have := state.GetBalance(accounts[1].Address).ToBig(); have.Cmp(new(big.Int).Add(mustBalance(t, cfg.Accounts.Balance), big.NewInt(1))) != 0 {
		t.Fatalf("unexpected recipient balance %s", have)
	}
	events, err := node.EventsSince(0)
	if err != nil || len(events) != 1 || events[0].BlockNumber != 1 {
		t.Fatalf("unexpected events %#v, %v", events, err)
	}
	client := node.RPCClient()
	defer client.Close()
	var trace map[string]any
	if err := client.Call(&trace, "debug_traceTransaction", hash, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := trace["structLogs"]; !ok {
		t.Fatalf("missing trace logs in %#v", trace)
	}
}

func TestInProcessRPC(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()

	var chainID hexutil.Uint64
	if err := client.Call(&chainID, "eth_chainId"); err != nil {
		t.Fatal(err)
	}
	if uint64(chainID) != DefaultChainID {
		t.Fatalf("chain ID %d", chainID)
	}
	var accounts []common.Address
	if err := client.Call(&accounts, "eth_accounts"); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 10 {
		t.Fatalf("accounts length %d", len(accounts))
	}
	var proof map[string]any
	if err := client.Call(&proof, "eth_getProof", accounts[0], []common.Hash{{1}}, "latest"); err != nil {
		t.Fatal(err)
	}
	if proof["accountProof"] == nil || proof["storageProof"] == nil {
		t.Fatalf("incomplete EIP-1186 proof %#v", proof)
	}
	var transactionHash common.Hash
	if err := client.Call(&transactionHash, "eth_sendTransaction", map[string]any{
		"from": accounts[0], "to": accounts[1], "value": "0x1", "gas": "0x5208",
	}); err != nil {
		t.Fatal(err)
	}
	if transactionHash == (common.Hash{}) {
		t.Fatal("unlocked transaction returned an empty hash")
	}
}

func TestRawTransactionRPCIsCastCompatible(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	accounts, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	chainID := new(big.Int).SetUint64(DefaultChainID)
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: 0,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		Gas: 21_000, To: &accounts[1].Address, Value: big.NewInt(1),
	})
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(chainID), accounts[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	client := node.RPCClient()
	defer client.Close()
	var syncReceipt map[string]any
	if err := client.Call(&syncReceipt, "eth_sendRawTransactionSync", hexutil.Bytes(raw)); err != nil {
		t.Fatal(err)
	}
	assertRPCSenderAndRecipient(t, syncReceipt, accounts[0].Address, accounts[1].Address)
	if syncReceipt["transactionHash"] != signed.Hash().Hex() {
		t.Fatalf("sync receipt transactionHash = %v, want %s", syncReceipt["transactionHash"], signed.Hash())
	}

	var transaction map[string]any
	if err := client.Call(&transaction, "eth_getTransactionByHash", signed.Hash()); err != nil {
		t.Fatal(err)
	}
	assertRPCSenderAndRecipient(t, transaction, accounts[0].Address, accounts[1].Address)
	for _, field := range []string{"blockHash", "blockNumber", "blockTimestamp", "transactionIndex"} {
		if transaction[field] == nil {
			t.Fatalf("transaction is missing mined field %q: %#v", field, transaction)
		}
	}

	var receipt map[string]any
	if err := client.Call(&receipt, "eth_getTransactionReceipt", signed.Hash()); err != nil {
		t.Fatal(err)
	}
	assertRPCSenderAndRecipient(t, receipt, accounts[0].Address, accounts[1].Address)
	if receipt["contractAddress"] != nil {
		t.Fatalf("transfer contractAddress = %v, want null", receipt["contractAddress"])
	}
}

func TestSendRawTransactionSyncWaitsAndReturnsStructuredTimeout(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	accounts, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	chainID := new(big.Int).SetUint64(DefaultChainID)
	sign := func(nonce uint64) *types.Transaction {
		t.Helper()
		unsigned := types.NewTx(&types.DynamicFeeTx{
			ChainID: chainID, Nonce: nonce,
			GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
			Gas: 21_000, To: &accounts[1].Address, Value: big.NewInt(1),
		})
		signed, signErr := types.SignTx(unsigned, types.LatestSignerForChainID(chainID), accounts[0].PrivateKey)
		if signErr != nil {
			t.Fatal(signErr)
		}
		return signed
	}

	client := node.RPCClient()
	defer client.Close()
	first := sign(0)
	firstRaw, err := first.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	type callResult struct {
		receipt map[string]any
		err     error
	}
	done := make(chan callResult, 1)
	go func() {
		var receipt map[string]any
		callErr := client.Call(&receipt, "eth_sendRawTransactionSync", hexutil.Bytes(firstRaw))
		done <- callResult{receipt: receipt, err: callErr}
	}()
	deadline := time.Now().Add(time.Second)
	for node.chain.pendingCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if node.chain.pendingCount() == 0 {
		t.Fatal("synchronous transaction was not submitted before waiting")
	}
	if _, err := node.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		assertRPCSenderAndRecipient(t, result.receipt, accounts[0].Address, accounts[1].Address)
	case <-time.After(time.Second):
		t.Fatal("synchronous transaction did not return after mining")
	}

	second := sign(1)
	secondRaw, err := second.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	err = client.Call(&receipt, "eth_sendRawTransactionSync", hexutil.Bytes(secondRaw), uint64(10))
	if err == nil {
		t.Fatal("expected synchronous transaction timeout")
	}
	var rpcErr rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.ErrorCode() != 4 {
		t.Fatalf("timeout error = %T %v, want RPC code 4", err, err)
	}
	var dataErr rpc.DataError
	if !errors.As(err, &dataErr) || dataErr.ErrorData() != second.Hash().Hex() {
		t.Fatalf("timeout data = %v, want %s", dataErr.ErrorData(), second.Hash())
	}
}

func assertRPCSenderAndRecipient(t *testing.T, value map[string]any, from, to common.Address) {
	t.Helper()
	fromValue, fromOK := value["from"].(string)
	if !fromOK || common.HexToAddress(fromValue) != from {
		t.Fatalf("from = %v, want %s in %#v", value["from"], from, value)
	}
	toValue, toOK := value["to"].(string)
	if !toOK || common.HexToAddress(toValue) != to {
		t.Fatalf("to = %v, want %s in %#v", value["to"], to, value)
	}
}

func TestSnapshotIsOneShotAndReorgEventsAreOrdered(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := node.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 2, true); err != nil {
		t.Fatal(err)
	}
	if ok, err := node.Revert(context.Background(), snapshot); err != nil || !ok {
		t.Fatalf("revert: ok=%v err=%v", ok, err)
	}
	if have := node.chain.blockchain.CurrentBlock().Number.Uint64(); have != 1 {
		t.Fatalf("head is %d, want 1", have)
	}
	if ok, err := node.Revert(context.Background(), snapshot); err != nil || ok {
		t.Fatalf("second revert: ok=%v err=%v", ok, err)
	}
	events, err := node.EventsSince(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || !events[0].Removed || !events[1].Removed ||
		events[0].BlockNumber != 3 || events[1].BlockNumber != 2 || events[2].Type != "chain_reorg" {
		t.Fatalf("unexpected reorg events %#v", events)
	}
}

func TestExplicitBranchSwitch(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if err := node.CreateBranch(context.Background(), "alternative", 1); err != nil {
		t.Fatal(err)
	}
	canonical, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	alternative, err := node.MineBranch(context.Background(), "alternative", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.SwitchBranch(context.Background(), "alternative"); err != nil {
		t.Fatal(err)
	}
	head := node.chain.blockchain.CurrentBlock()
	if head.Hash() != alternative[1] || head.Hash() == canonical[0] {
		t.Fatalf("unexpected canonical head %s", head.Hash())
	}
}

func TestMissedSlotsRemainDistinctFromExecutionBlockNumbers(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if _, err := node.MissSlots(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByNumber(1)
	if slot := node.chain.slotOf(block); slot != 3 {
		t.Fatalf("slot is %d, want 3", slot)
	}
	wantTime := uint64(cfg.Chain.GenesisTime) + 3*uint64(cfg.Chain.SlotDuration.Seconds())
	if block.Time() != wantTime {
		t.Fatalf("timestamp is %d, want %d", block.Time(), wantTime)
	}
	if _, err := node.beaconBlockID("1"); err == nil {
		t.Fatal("missed slot unexpectedly resolved to a block")
	}
	resolved, err := node.beaconBlockID("3")
	if err != nil || resolved.Hash() != block.Hash() {
		t.Fatalf("slot 3 resolution: block=%v err=%v", resolved, err)
	}
}

func TestPebbleRestartRetainsCanonicalHead(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	cfg.Events.Capacity = 1
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	hashes, err := first.Mine(context.Background(), 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Checkpoint(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if err := first.CreateBranch(context.Background(), "alternate", 1); err != nil {
		t.Fatal(err)
	}
	branchHashes, err := first.MineBranch(context.Background(), "alternate", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close() //nolint:errcheck
	head := second.chain.blockchain.CurrentBlock()
	if head.Number.Uint64() != 2 || head.Hash() != hashes[1] || second.chain.currentSlot() != 2 {
		t.Fatalf("restart head number=%d hash=%s slot=%d", head.Number.Uint64(), head.Hash(), second.chain.currentSlot())
	}
	events, err := second.EventsSince(0)
	if !errors.Is(err, ErrEventGap) {
		t.Fatalf("events before retained window: events=%#v err=%v", events, err)
	}
	events, err = second.EventsSince(1)
	if err != nil || len(events) != 1 || events[0].Revision != 2 || events[0].BlockHash != hashes[1] {
		t.Fatalf("retained events after restart: events=%#v err=%v", events, err)
	}
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	if err := second.Restore(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if err := second.SwitchBranch(context.Background(), "alternate"); err != nil {
		t.Fatal(err)
	}
	if got := second.chain.blockchain.CurrentBlock().Hash(); got != branchHashes[0] {
		t.Fatalf("restored branch head %s, want %s", got, branchHashes[0])
	}
}

func TestNewHeadsSubscriptionAndSequentialBatch(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()
	headers := make(chan *types.Header, 1)
	subscription, err := client.Subscribe(context.Background(), "eth", headers, "newHeads")
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Unsubscribe()
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	select {
	case header := <-headers:
		if header.Number.Uint64() != 1 {
			t.Fatalf("subscription block %d", header.Number.Uint64())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for newHeads")
	}

	var snapshot hexutil.Uint64
	var mined []common.Hash
	var reverted bool
	batch := []rpc.BatchElem{
		{Method: "evm_snapshot", Result: &snapshot},
		{Method: "evm_mine", Result: &mined},
		{Method: "evm_revert", Args: []any{hexutil.Uint64(1)}, Result: &reverted},
	}
	if err := client.BatchCall(batch); err != nil {
		t.Fatal(err)
	}
	for _, element := range batch {
		if element.Error != nil {
			t.Fatal(element.Error)
		}
	}
	if snapshot != 1 || !reverted || node.chain.blockchain.CurrentBlock().Number.Uint64() != 1 {
		t.Fatalf("batch was not sequential: snapshot=%d reverted=%v head=%d", snapshot, reverted, node.chain.blockchain.CurrentBlock().Number.Uint64())
	}
}

func TestBeaconSSEStreamsFutureEvents(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	cfg.HTTP.Enabled = true
	cfg.HTTP.Address = "127.0.0.1:0"
	cfg.Beacon.Enabled = true
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	ctx := t.Context()
	request, _ := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		node.Endpoints().Beacon+"/eth/v1/events?topics=block",
		nil,
	)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status %d", response.StatusCode)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	record := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(response.Body)
		var lines strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				record <- lines.String()
				return
			}
			lines.WriteString(line)
			if line == "\n" {
				record <- lines.String()
				return
			}
		}
	}()
	select {
	case event := <-record:
		if !strings.Contains(event, "event: block") || !strings.Contains(event, `"slot":"1"`) || !strings.Contains(event, `"execution_optimistic":false`) {
			t.Fatalf("unexpected SSE event %q", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
}

func TestEndpointDiscoveryAndDisabledBeaconRouting(t *testing.T) {
	cfg := testConfig()
	cfg.HTTP.Enabled = true
	cfg.HTTP.Address = "127.0.0.1:0"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	endpoints := node.Endpoints()
	if endpoints.Execution == "" || endpoints.Beacon != "" {
		t.Fatalf("endpoints with Beacon disabled = %#v", endpoints)
	}
	response, err := http.Get(endpoints.Execution + "/eth/v1/node/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled Beacon route status = %d, want 404", response.StatusCode)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	offline, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.Start(); err != nil {
		t.Fatal(err)
	}
	if endpoints := offline.Endpoints(); endpoints.Execution != "" || endpoints.Beacon != "" {
		t.Fatalf("disabled HTTP endpoints = %#v", endpoints)
	}
	if err := offline.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCallAndNativeTracing(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()
	args := map[string]any{
		"from": node.Accounts()[0], "to": node.Accounts()[1],
		"value": "0x1", "gas": "0x5208",
	}
	var output hexutil.Bytes
	if err := client.Call(&output, "eth_call", args, "latest"); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&output, "eth_call", map[string]any{
		"from": node.Accounts()[0], "to": node.Accounts()[1],
	}, "latest", map[string]any{
		node.Accounts()[1].Hex(): map[string]any{"code": "0x602a60005260206000f3"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(output) != 32 || output[31] != 42 {
		t.Fatalf("state override returned %x", output)
	}
	var structured map[string]any
	if err := client.Call(&structured, "debug_traceCall", args, "latest", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := structured["structLogs"]; !ok {
		t.Fatalf("missing structLogs in %#v", structured)
	}
	var native map[string]any
	if err := client.Call(&native, "debug_traceCall", args, "latest", map[string]any{"tracer": "callTracer"}); err != nil {
		t.Fatal(err)
	}
	if native["type"] != "CALL" {
		t.Fatalf("unexpected native trace %#v", native)
	}
	var rejected any
	if err := client.Call(&rejected, "debug_traceCall", args, "latest", map[string]any{"tracer": "{ return {}; }"}); err == nil {
		t.Fatal("JavaScript tracer unexpectedly accepted")
	}
}

func TestBlockTracingByHashAndNumber(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()

	emptyHashes, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	var empty []traceResult
	if err := client.Call(&empty, "debug_traceBlockByHash", emptyHashes[0]); err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty block trace = %#v, want []", empty)
	}
	var rejected json.RawMessage
	if err := client.Call(&rejected, "debug_traceBlockByHash", emptyHashes[0], map[string]any{"tracer": "{ return {}; }"}); err == nil {
		t.Fatal("JavaScript tracer unexpectedly accepted for an empty block")
	}

	account := testWalletAccount(t, node, 0)
	first := signedDynamicTransaction(t, cfg, account, 0, node.Accounts()[1], big.NewInt(1), nil)
	second := signedDynamicTransaction(t, cfg, account, 1, node.Accounts()[2], big.NewInt(2), nil)
	if _, err := node.SendTransaction(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	if block == nil {
		t.Fatal("mined block not found")
	}

	var structured []traceResult
	if err := client.Call(&structured, "debug_traceBlockByHash", block.Hash(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if len(structured) != 2 || structured[0].TxHash != first.Hash() || structured[1].TxHash != second.Hash() {
		t.Fatalf("hash block trace = %#v", structured)
	}
	for _, trace := range structured {
		var result map[string]any
		if err := json.Unmarshal(trace.Result, &result); err != nil {
			t.Fatal(err)
		}
		if _, ok := result["structLogs"]; !ok {
			t.Fatalf("missing structLogs in %#v", result)
		}
	}

	var native []traceResult
	if err := client.Call(&native, "debug_traceBlockByNumber", hexutil.Uint64(block.NumberU64()), map[string]any{"tracer": "callTracer"}); err != nil {
		t.Fatal(err)
	}
	if len(native) != 2 || native[0].TxHash != first.Hash() || native[1].TxHash != second.Hash() {
		t.Fatalf("number block trace = %#v", native)
	}
	for _, trace := range native {
		var result map[string]any
		if err := json.Unmarshal(trace.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result["type"] != "CALL" {
			t.Fatalf("unexpected native trace %#v", result)
		}
	}

	var latest []traceResult
	if err := client.Call(&latest, "debug_traceBlockByNumber", "latest", map[string]any{"tracer": "callTracer"}); err != nil || len(latest) != 2 {
		t.Fatalf("latest block trace = %d, %v", len(latest), err)
	}
	var pending []traceResult
	if err := client.Call(&pending, "debug_traceBlockByNumber", "pending", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("pending block trace encoded as null")
	}

	if err := client.Call(&rejected, "debug_traceBlockByHash", block.Hash(), map[string]any{"tracer": "{ return {}; }"}); err == nil {
		t.Fatal("JavaScript tracer unexpectedly accepted")
	}
	if err := client.Call(&rejected, "debug_traceBlockByHash", common.HexToHash("0xdead")); err == nil {
		t.Fatal("missing block unexpectedly traced")
	}
	if err := client.Call(&rejected, "debug_traceBlockByNumber", "earliest"); err == nil {
		t.Fatal("genesis unexpectedly traced")
	}
}

func TestVerifiableZeroTransactionControlBlock(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	address := node.Accounts()[1]
	balance := big.NewInt(123)
	nonce := uint64(7)
	code := []byte{0x60, 0x00, 0x56}
	storage := map[common.Hash]common.Hash{{1}: {2}}
	hash, err := node.ApplyControl(context.Background(), ControlChanges{
		address: {Balance: balance, Nonce: &nonce, Code: &code, StorageDiff: &storage},
	})
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hash)
	if block == nil || len(block.Transactions()) != 0 || block.NumberU64() != 1 {
		t.Fatalf("invalid control block %#v", block)
	}
	state, err := node.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.GetBalance(address).ToBig().Cmp(balance) != 0 ||
		state.GetNonce(address) != nonce ||
		state.GetState(address, common.Hash{1}) != (common.Hash{2}) {
		t.Fatal("control state was not committed")
	}
	if changes, ok := node.ControlChanges(hash); !ok || changes[address].Balance.Cmp(balance) != 0 {
		t.Fatalf("missing verifiable control record %#v", changes)
	}
	if valid, err := node.VerifyControlBlock(context.Background(), hash); err != nil || !valid {
		t.Fatalf("control verification valid=%v err=%v", valid, err)
	}
}

func mustBalance(t *testing.T, value string) *big.Int {
	t.Helper()
	balance, err := parseBalance(value)
	if err != nil {
		t.Fatal(err)
	}
	return balance
}
