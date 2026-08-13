package ethertest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/params"
)

type failingExecutionRequestDatabase struct{ ethdb.Database }

func (db failingExecutionRequestDatabase) NewBatch() ethdb.Batch {
	return &failingExecutionRequestBatch{Batch: db.Database.NewBatch()}
}

func (db failingExecutionRequestDatabase) NewBatchWithSize(size int) ethdb.Batch {
	return &failingExecutionRequestBatch{Batch: db.Database.NewBatchWithSize(size)}
}

type failingExecutionRequestBatch struct {
	ethdb.Batch
	fail bool
}

func (batch *failingExecutionRequestBatch) Put(key, value []byte) error {
	if bytes.Equal(key, executionRequestQueueKey) {
		batch.fail = true
	}
	return batch.Batch.Put(key, value)
}

func (batch *failingExecutionRequestBatch) Write() error {
	if batch.fail {
		return errors.New("injected execution request queue failure")
	}
	return batch.Batch.Write()
}

func TestExecutionRequestControlsUpdatePendingBeaconAndSafety(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	deposit, withdrawal, consolidation := testExecutionRequests()
	if err := node.AddDepositRequest(context.Background(), deposit); err != nil {
		t.Fatal(err)
	}
	if err := node.AddWithdrawalRequest(context.Background(), withdrawal); err != nil {
		t.Fatal(err)
	}
	if err := node.AddConsolidationRequest(context.Background(), consolidation); err != nil {
		t.Fatal(err)
	}
	if head := node.chain.blockchain.CurrentBlock().Number.Uint64(); head != 0 {
		t.Fatalf("adding execution requests mined block %d", head)
	}
	want := [][]byte{
		append([]byte{executionRequestDeposit}, encodeExecutionDepositRequest(deposit)...),
		append([]byte{executionRequestWithdrawal}, encodeExecutionWithdrawalRequest(withdrawal)...),
		append([]byte{executionRequestConsolidation}, encodeExecutionConsolidationRequest(consolidation)...),
	}
	assertExecutionRequestHash(t, node.chain.pendingBlock(), want)
	client := node.RPCClient()
	defer client.Close()
	var pendingRPC, latestRPC map[string]any
	if err := client.Call(&pendingRPC, "eth_getBlockByNumber", "pending", false); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&latestRPC, "eth_getBlockByNumber", "latest", false); err != nil {
		t.Fatal(err)
	}
	wantHash := types.CalcRequestsHash(want).Hex()
	if pendingRPC["requestsHash"] != wantHash || latestRPC["requestsHash"] == wantHash {
		t.Fatalf("pending/latest requestsHash = %v/%v, want %s/distinct", pendingRPC["requestsHash"], latestRPC["requestsHash"], wantHash)
	}

	hashes, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	assertExecutionRequestHash(t, block, want)
	if err := client.Call(&latestRPC, "eth_getBlockByNumber", "latest", false); err != nil {
		t.Fatal(err)
	}
	if latestRPC["requestsHash"] != wantHash {
		t.Fatalf("mined latest requestsHash = %v, want %s", latestRPC["requestsHash"], wantHash)
	}
	record, exists, err := loadExecutionRequestRecord(node.chain, block.Hash())
	if err != nil || !exists {
		t.Fatalf("load execution request record: exists=%v err=%v", exists, err)
	}
	if !equalExecutionRequestBytes(record.Requests, want) ||
		len(record.Controls.Deposits) != 1 || len(record.Controls.Withdrawals) != 1 ||
		len(record.Controls.Consolidations) != 1 || record.Controls.Deposits[0].ID != 1 ||
		record.Controls.Withdrawals[0].ID != 2 || record.Controls.Consolidations[0].ID != 3 {
		t.Fatalf("stored execution request record = %#v", record)
	}

	signed, err := node.consensus.signedBlock(node.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	if signed.electra == nil || signed.electra.Message == nil || signed.electra.Message.Body == nil {
		t.Fatal("Electra Beacon projection is incomplete")
	}
	projected, err := marshalExecutionRequests(signed.electra.Message.Body.ExecutionRequests)
	if err != nil {
		t.Fatal(err)
	}
	if !equalExecutionRequestBytes(projected, want) {
		t.Fatalf("Beacon projection requests = %x, want %x", projected, want)
	}
	jsonData, err := json.Marshal(signed.electra)
	if err != nil {
		t.Fatal(err)
	}
	var jsonBlock electra.SignedBeaconBlock
	if err := json.Unmarshal(jsonData, &jsonBlock); err != nil {
		t.Fatal(err)
	}
	sszData, err := signed.marshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	var sszBlock electra.SignedBeaconBlock
	if err := sszBlock.UnmarshalSSZ(sszData); err != nil {
		t.Fatal(err)
	}
	jsonRequests, err := marshalExecutionRequests(jsonBlock.Message.Body.ExecutionRequests)
	if err != nil {
		t.Fatal(err)
	}
	sszRequests, err := marshalExecutionRequests(sszBlock.Message.Body.ExecutionRequests)
	if err != nil {
		t.Fatal(err)
	}
	if !equalExecutionRequestBytes(jsonRequests, want) || !equalExecutionRequestBytes(sszRequests, want) {
		t.Fatal("Beacon JSON, SSZ, and persisted execution requests diverged")
	}

	safety, err := node.BlockSafety(block.Hash())
	if err != nil {
		t.Fatal(err)
	}
	status := node.SafetyStatus()
	if !safety.Tainted || !slices.Contains(safety.Reasons, taintExecutionRequestControl) ||
		!status.HeadTainted || !status.SessionTainted || !slices.Contains(status.Reasons, taintExecutionRequestControl) {
		t.Fatalf("execution request control safety = %#v status=%#v", safety, status)
	}
	if !node.pendingExecutionRequests.empty() || node.pendingExecutionRequests.NextID != 4 {
		t.Fatalf("queue after inclusion = %#v", node.pendingExecutionRequests)
	}
	assertExecutionRequestHash(t, node.chain.pendingBlock(), [][]byte{})
	children, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	child := node.chain.blockchain.GetBlockByHash(children[0])
	childSafety, err := node.BlockSafety(child.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if !childSafety.Tainted || !slices.Contains(childSafety.Reasons, taintExecutionRequestControl) ||
		!node.beaconLineageTainted(child) {
		t.Fatalf("execution request taint did not reach descendant/Beacon optimism: %#v", childSafety)
	}
}

func TestExecutionRequestRPCValidationLimitsAndNamespace(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
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

	pubkey := hexutil.Bytes(bytes.Repeat([]byte{0x11}, 48))
	credentials := hexutil.Bytes(bytes.Repeat([]byte{0x22}, 32))
	signature := hexutil.Bytes(bytes.Repeat([]byte{0x33}, 96))
	address := common.HexToAddress("0x1234000000000000000000000000000000005678")
	deposit := map[string]any{
		"pubkey": pubkey, "withdrawalCredentials": credentials, "amount": "0x0",
		"signature": signature, "index": "0x0",
	}
	withdrawal := map[string]any{
		"sourceAddress": address, "validatorPubkey": pubkey, "amount": "0x0",
	}
	consolidation := map[string]any{
		"sourceAddress": address, "sourcePubkey": pubkey, "targetPubkey": hexutil.Bytes(bytes.Repeat([]byte{0x44}, 48)),
	}
	for method, args := range map[string]map[string]any{
		"ethertest_addDepositRequest":       deposit,
		"ethertest_addWithdrawalRequest":    withdrawal,
		"ethertest_addConsolidationRequest": consolidation,
	} {
		var result bool
		if err := client.Call(&result, method, args); err != nil || !result {
			t.Fatalf("%s = %v, %v", method, result, err)
		}
	}

	for method, args := range map[string]map[string]any{
		"ethertest_addDepositRequest": {
			"pubkey": hexutil.Bytes{1}, "withdrawalCredentials": credentials,
			"amount": "0x1", "signature": signature, "index": "0x1",
		},
		"ethertest_addWithdrawalRequest": {"sourceAddress": address, "amount": "0x1"},
		"ethertest_addConsolidationRequest": {
			"sourceAddress": address, "sourcePubkey": pubkey, "targetPubkey": hexutil.Bytes{1},
		},
	} {
		var result bool
		assertRPCErrorCode(t, client.Call(&result, method, args), -32602)
	}
	var result bool
	invalidQuantity := map[string]any{
		"sourceAddress": address, "validatorPubkey": pubkey, "amount": "1",
	}
	assertRPCErrorCode(t, client.Call(&result, "ethertest_addWithdrawalRequest", invalidQuantity), -32602)

	if err := client.Call(&result, "ethertest_addConsolidationRequest", consolidation); err != nil || !result {
		t.Fatalf("second consolidation request = %v, %v", result, err)
	}
	assertRPCErrorCode(t, client.Call(&result, "ethertest_addConsolidationRequest", consolidation), -32602)
	if len(node.pendingExecutionRequests.Consolidations) != maxConsolidationRequestsPerPayload {
		t.Fatal("consolidation queue limit failure changed the accepted queue")
	}
	assertRPCErrorCode(t, client.Call(&result, "anvil_addDepositRequest", deposit), -32601)
	assertRPCErrorCode(t, client.Call(&result, "evm_addWithdrawalRequest", withdrawal), -32601)

	legacyWithdrawal := map[string]any{
		"validatorIndex": "0x1", "address": address, "amount": "0x1",
	}
	if err := client.Call(&result, "ethertest_addWithdrawal", legacyWithdrawal); err != nil || !result {
		t.Fatalf("existing EIP-4895 withdrawal API = %v, %v", result, err)
	}
	if len(node.pendingWithdrawals) != 1 || len(node.pendingExecutionRequests.Withdrawals) != 1 {
		t.Fatal("EIP-4895 and EIP-7002 queues were not isolated")
	}
	var capabilities map[string]any
	if err := client.Call(&capabilities, "ethertest_capabilities"); err != nil {
		t.Fatal(err)
	}
	if capabilities["executionRequests"] != true || capabilities["executionRequestControls"] != true {
		t.Fatalf("execution request capabilities = %#v", capabilities)
	}
}

func TestExecutionRequestParsingCapacityAndOrdering(t *testing.T) {
	deposit, withdrawal, consolidation := testExecutionRequests()
	for _, test := range []struct {
		name        string
		requestType byte
		data        []byte
		limit       int
		fullError   error
		fill        func(*executionRequestQueue, []queuedExecutionRequest)
	}{
		{
			name: "deposit", requestType: executionRequestDeposit, data: encodeExecutionDepositRequest(deposit),
			limit: maxDepositRequestsPerPayload, fullError: ErrDepositRequestQueueFull,
			fill: func(queue *executionRequestQueue, items []queuedExecutionRequest) { queue.Deposits = items },
		},
		{
			name: "withdrawal", requestType: executionRequestWithdrawal, data: encodeExecutionWithdrawalRequest(withdrawal),
			limit: maxWithdrawalRequestsPerPayload, fullError: ErrWithdrawalRequestQueueFull,
			fill: func(queue *executionRequestQueue, items []queuedExecutionRequest) { queue.Withdrawals = items },
		},
		{
			name: "consolidation", requestType: executionRequestConsolidation, data: encodeExecutionConsolidationRequest(consolidation),
			limit: maxConsolidationRequestsPerPayload, fullError: ErrConsolidationRequestQueueFull,
			fill: func(queue *executionRequestQueue, items []queuedExecutionRequest) { queue.Consolidations = items },
		},
	} {
		t.Run(test.name+" control queue limit", func(t *testing.T) {
			queue := newExecutionRequestQueue()
			queue.NextID = uint64(test.limit) + 1
			items := make([]queuedExecutionRequest, test.limit)
			for index := range items {
				items[index] = queuedExecutionRequest{ID: uint64(index) + 1, Data: test.data}
			}
			test.fill(&queue, items)
			if _, err := queue.enqueue(test.requestType, test.data); !errors.Is(err, test.fullError) {
				t.Fatalf("full %s queue error = %v", test.name, err)
			}
		})
	}
	controlData := encodeExecutionWithdrawalRequest(withdrawal)
	queue := newExecutionRequestQueue()
	queue.NextID = 2
	queue.Withdrawals = []queuedExecutionRequest{{ID: 1, Data: controlData}}

	nativeItem := []byte{executionRequestWithdrawal}
	for index := range maxWithdrawalRequestsPerPayload {
		request := withdrawal
		request.Amount = uint64(index + 1)
		nativeItem = append(nativeItem, encodeExecutionWithdrawalRequest(request)...)
	}
	native := [][]byte{nativeItem}
	fullBlock := executionRequestTestBlock(native)
	prepared, err := prepareExecutionRequestBlock(fullBlock, native, queue)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Controlled || len(prepared.Remaining.Withdrawals) != 1 || prepared.Block.Hash() != fullBlock.Hash() {
		t.Fatalf("full native block consumed or mutated controls: %#v", prepared)
	}

	emptyNative := [][]byte{}
	emptyBlock := executionRequestTestBlock(emptyNative)
	prepared, err = prepareExecutionRequestBlock(emptyBlock, emptyNative, prepared.Remaining)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Controlled || len(prepared.Remaining.Withdrawals) != 0 || prepared.Block.Hash() == emptyBlock.Hash() {
		t.Fatalf("deferred control was not included in the next block: %#v", prepared)
	}
	entries, err := executionRequestEntries(prepared.Record.Requests, executionRequestWithdrawal)
	if err != nil || len(entries) != 1 || !bytes.Equal(entries[0], controlData) {
		t.Fatalf("deferred request entries = %x, %v", entries, err)
	}

	nativeRequest := withdrawal
	nativeRequest.Amount = 99
	nativeData := encodeExecutionWithdrawalRequest(nativeRequest)
	mixedNative := [][]byte{append([]byte{executionRequestWithdrawal}, nativeData...)}
	mixedBlock := executionRequestTestBlock(mixedNative)
	prepared, err = prepareExecutionRequestBlock(mixedBlock, mixedNative, queue)
	if err != nil {
		t.Fatal(err)
	}
	entries, err = executionRequestEntries(prepared.Record.Requests, executionRequestWithdrawal)
	if err != nil || len(entries) != 2 || !bytes.Equal(entries[0], nativeData) || !bytes.Equal(entries[1], controlData) {
		t.Fatalf("native/control ordering = %x, %v", entries, err)
	}

	for name, invalid := range map[string][][]byte{
		"unknown type": {{0x03, 0x01}},
		"wrong order": {
			append([]byte{executionRequestWithdrawal}, controlData...),
			append([]byte{executionRequestDeposit}, make([]byte, depositRequestSize)...),
		},
		"invalid fixed size": {{executionRequestWithdrawal, 0x01}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareExecutionRequestBlock(executionRequestTestBlock(invalid), invalid, newExecutionRequestQueue()); err == nil {
				t.Fatal("invalid native execution requests were accepted")
			}
		})
	}
	overLimit := []byte{executionRequestDeposit}
	overLimit = append(overLimit, make([]byte, (maxDepositRequestsPerPayload+1)*depositRequestSize)...)
	if _, err := prepareExecutionRequestBlock(
		executionRequestTestBlock([][]byte{overLimit}), [][]byte{overLimit}, newExecutionRequestQueue(),
	); err == nil {
		t.Fatal("over-limit native deposit requests were accepted")
	}

	prePrague := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1)})
	prepared, err = prepareExecutionRequestBlock(prePrague, nil, queue)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Controlled || len(prepared.Remaining.Withdrawals) != 1 || prepared.Requests != nil {
		t.Fatal("pre-Prague block consumed an execution request control")
	}
}

func TestNativeExecutionRequestsFromDeveloperPredeploysAndDepositLogs(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	account := testWalletAccount(t, node, 0)

	runtime := depositLogRuntime()
	creation := contractCreationCode(runtime)
	deploy := signExecutionRequestTransaction(t, cfg, account, 0, nil, new(big.Int), creation, 500_000)
	if _, err := node.SendTransaction(context.Background(), deploy); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	depositAddress := crypto.CreateAddress(account.Address, 0)
	state, err := node.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if code := state.GetCode(depositAddress); !bytes.Equal(code, runtime) {
		t.Fatalf("deposit event contract code = %x, want %x", code, runtime)
	}
	deposit, _, _ := testExecutionRequests()
	depositData := depositLogData(t, deposit)
	unconfigured := signExecutionRequestTransaction(t, cfg, account, 1, &depositAddress, new(big.Int), depositData, 250_000)
	if _, err := node.SendTransaction(context.Background(), unconfigured); err != nil {
		t.Fatal(err)
	}
	unconfiguredHashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	unconfiguredRecord, exists, err := loadExecutionRequestRecord(node.chain, unconfiguredHashes[0])
	if err != nil || !exists || len(unconfiguredRecord.Requests) != 0 {
		t.Fatalf("unconfigured deposit address produced requests: record=%#v exists=%v err=%v", unconfiguredRecord, exists, err)
	}
	node.chain.config.DepositContractAddress = depositAddress
	node.chain.blockchain.Config().DepositContractAddress = depositAddress

	withdrawalPubkey := bytes.Repeat([]byte{0x51}, 48)
	withdrawalAmount := make([]byte, 8)
	binary.BigEndian.PutUint64(withdrawalAmount, 3456)
	withdrawalData := append(append([]byte(nil), withdrawalPubkey...), withdrawalAmount...)
	sourcePubkey := bytes.Repeat([]byte{0x61}, 48)
	targetPubkey := bytes.Repeat([]byte{0x71}, 48)
	consolidationData := append(append([]byte(nil), sourcePubkey...), targetPubkey...)
	transactions := []*types.Transaction{
		signExecutionRequestTransaction(t, cfg, account, 2, &depositAddress, new(big.Int), depositData, 250_000),
		signExecutionRequestTransaction(t, cfg, account, 3, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), withdrawalData, 500_000),
		signExecutionRequestTransaction(t, cfg, account, 4, addressPointer(params.ConsolidationQueueAddress), big.NewInt(params.GWei), consolidationData, 500_000),
	}
	for _, transaction := range transactions {
		if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
			t.Fatal(err)
		}
	}
	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	record, exists, err := loadExecutionRequestRecord(node.chain, block.Hash())
	if err != nil || !exists {
		t.Fatalf("load native request record: exists=%v err=%v", exists, err)
	}
	if len(record.Controls.Deposits)+len(record.Controls.Withdrawals)+len(record.Controls.Consolidations) != 0 {
		t.Fatalf("native block recorded synthetic controls: %#v", record.Controls)
	}
	if len(record.Requests) != 3 || record.Requests[0][0] != executionRequestDeposit ||
		record.Requests[1][0] != executionRequestWithdrawal || record.Requests[2][0] != executionRequestConsolidation {
		t.Fatalf("native request groups = %x", record.Requests)
	}
	parsed, err := parseExecutionRequests(record.Requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Deposits) != 1 || len(parsed.Withdrawals) != 1 || len(parsed.Consolidations) != 1 {
		t.Fatalf("native execution requests = %#v", parsed)
	}
	if uint64(parsed.Deposits[0].Amount) != deposit.Amount || parsed.Deposits[0].Index != deposit.Index ||
		!bytes.Equal(parsed.Deposits[0].Pubkey[:], deposit.Pubkey[:]) ||
		!bytes.Equal(parsed.Deposits[0].WithdrawalCredentials, deposit.WithdrawalCredentials[:]) ||
		!bytes.Equal(parsed.Deposits[0].Signature[:], deposit.Signature[:]) {
		t.Fatalf("native deposit = %#v", parsed.Deposits[0])
	}
	if common.Address(parsed.Withdrawals[0].SourceAddress) != account.Address ||
		!bytes.Equal(parsed.Withdrawals[0].ValidatorPubkey[:], withdrawalPubkey) ||
		uint64(parsed.Withdrawals[0].Amount) != 3456 {
		t.Fatalf("native withdrawal = %#v", parsed.Withdrawals[0])
	}
	if common.Address(parsed.Consolidations[0].SourceAddress) != account.Address ||
		!bytes.Equal(parsed.Consolidations[0].SourcePubkey[:], sourcePubkey) ||
		!bytes.Equal(parsed.Consolidations[0].TargetPubkey[:], targetPubkey) {
		t.Fatalf("native consolidation = %#v", parsed.Consolidations[0])
	}
	assertExecutionRequestHash(t, block, record.Requests)
	signed, err := node.consensus.signedBlock(node.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := marshalExecutionRequests(signed.electra.Message.Body.ExecutionRequests)
	if err != nil {
		t.Fatal(err)
	}
	if !equalExecutionRequestBytes(projected, record.Requests) {
		t.Fatal("native Beacon projection diverged from geth requests")
	}
	safety, err := node.BlockSafety(block.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if safety.Tainted || node.SafetyStatus().SessionTainted {
		t.Fatalf("native-only block was tainted: block=%#v status=%#v", safety, node.SafetyStatus())
	}
}

func TestNativeAndControlExecutionRequestOrdering(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	account := testWalletAccount(t, node, 0)
	_, control, _ := testExecutionRequests()
	if err := node.AddWithdrawalRequest(context.Background(), control); err != nil {
		t.Fatal(err)
	}
	nativePubkey := bytes.Repeat([]byte{0x91}, 48)
	nativeData := append(append([]byte(nil), nativePubkey...), make([]byte, 8)...)
	transaction := signExecutionRequestTransaction(
		t, cfg, account, 0, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), nativeData, 500_000,
	)
	if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
		t.Fatal(err)
	}
	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	record, exists, err := loadExecutionRequestRecord(node.chain, block.Hash())
	if err != nil || !exists {
		t.Fatalf("load mixed request record: exists=%v err=%v", exists, err)
	}
	entries, err := executionRequestEntries(record.Requests, executionRequestWithdrawal)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || len(record.Controls.Withdrawals) != 1 ||
		!bytes.Equal(entries[1], record.Controls.Withdrawals[0].Data) {
		t.Fatalf("mixed request record = %#v entries=%x", record, entries)
	}
	parsed, err := parseExecutionRequests(record.Requests)
	if err != nil {
		t.Fatal(err)
	}
	if common.Address(parsed.Withdrawals[0].SourceAddress) != account.Address ||
		!bytes.Equal(parsed.Withdrawals[0].ValidatorPubkey[:], nativePubkey) ||
		common.Address(parsed.Withdrawals[1].SourceAddress) != control.SourceAddress ||
		!bytes.Equal(parsed.Withdrawals[1].ValidatorPubkey[:], control.ValidatorPubkey[:]) {
		t.Fatalf("native/control ordering = %#v", parsed.Withdrawals)
	}
	assertExecutionRequestHash(t, block, record.Requests)
	safety, err := node.BlockSafety(block.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if !safety.Tainted || !slices.Contains(safety.Reasons, taintExecutionRequestControl) {
		t.Fatalf("mixed request block safety = %#v", safety)
	}
}

func TestNativeDepositRequestFollowsContainingBlockAcrossReorg(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	account := testWalletAccount(t, node, 0)
	runtime := depositLogRuntime()
	deploy := signExecutionRequestTransaction(t, cfg, account, 0, nil, new(big.Int), contractCreationCode(runtime), 500_000)
	if _, err := node.SendTransaction(context.Background(), deploy); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	depositAddress := crypto.CreateAddress(account.Address, 0)
	node.chain.config.DepositContractAddress = depositAddress
	node.chain.blockchain.Config().DepositContractAddress = depositAddress
	if err := node.CreateBranch(context.Background(), "without-deposit", 1); err != nil {
		t.Fatal(err)
	}
	emptyBranch, err := node.MineBranch(context.Background(), "without-deposit", 1)
	if err != nil {
		t.Fatal(err)
	}
	deposit, _, _ := testExecutionRequests()
	transaction := signExecutionRequestTransaction(
		t, cfg, account, 1, &depositAddress, new(big.Int), depositLogData(t, deposit), 250_000,
	)
	if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
		t.Fatal(err)
	}
	withDeposit, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	depositRecord, exists, err := loadExecutionRequestRecord(node.chain, withDeposit[0])
	if err != nil || !exists || len(depositRecord.Requests) != 1 || depositRecord.Requests[0][0] != executionRequestDeposit {
		t.Fatalf("deposit block request record = %#v exists=%v err=%v", depositRecord, exists, err)
	}
	if err := node.CreateBranch(context.Background(), "with-deposit", 2); err != nil {
		t.Fatal(err)
	}
	if err := node.SwitchBranch(context.Background(), "without-deposit"); err != nil {
		t.Fatal(err)
	}
	if node.chain.blockchain.CurrentBlock().Hash() != emptyBranch[0] || node.chain.pendingCount() != 0 {
		t.Fatalf("orphan deposit branch head/pool = %s/%d", node.chain.blockchain.CurrentBlock().Hash(), node.chain.pendingCount())
	}
	emptyRecord, exists, err := loadExecutionRequestRecord(node.chain, emptyBranch[0])
	if err != nil || !exists || len(emptyRecord.Requests) != 0 {
		t.Fatalf("alternate branch inherited deposit request: %#v exists=%v err=%v", emptyRecord, exists, err)
	}
	next, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	nextRecord, exists, err := loadExecutionRequestRecord(node.chain, next[0])
	if err != nil || !exists || len(nextRecord.Requests) != 0 {
		t.Fatalf("orphan deposit transaction was reprojected: %#v exists=%v err=%v", nextRecord, exists, err)
	}
	if err := node.SwitchBranch(context.Background(), "with-deposit"); err != nil {
		t.Fatal(err)
	}
	restoredRecord, exists, err := loadExecutionRequestRecord(node.chain, withDeposit[0])
	if err != nil || !exists || !equalExecutionRequestBytes(restoredRecord.Requests, depositRecord.Requests) {
		t.Fatalf("restored deposit block record = %#v exists=%v err=%v", restoredRecord, exists, err)
	}
}

func TestNativeCapacityDefersControlUntilFollowingBlock(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	account := testWalletAccount(t, node, 0)
	_, control, _ := testExecutionRequests()
	if err := node.AddWithdrawalRequest(context.Background(), control); err != nil {
		t.Fatal(err)
	}
	for nonce := range uint64(maxWithdrawalRequestsPerPayload) {
		pubkey := bytes.Repeat([]byte{byte(nonce + 1)}, 48)
		data := append(pubkey, make([]byte, 8)...)
		transaction := signExecutionRequestTransaction(
			t, cfg, account, nonce, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), data, 500_000,
		)
		if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
			t.Fatal(err)
		}
	}
	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	full := node.chain.blockchain.GetBlockByHash(hashes[0])
	record, exists, err := loadExecutionRequestRecord(node.chain, full.Hash())
	if err != nil || !exists {
		t.Fatalf("load full native record: exists=%v err=%v", exists, err)
	}
	entries, err := executionRequestEntries(record.Requests, executionRequestWithdrawal)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != maxWithdrawalRequestsPerPayload || len(record.Controls.Withdrawals) != 0 ||
		len(node.pendingExecutionRequests.Withdrawals) != 1 {
		t.Fatalf("full native record=%#v pending=%#v", record, node.pendingExecutionRequests.Withdrawals)
	}
	fullSafety, err := node.BlockSafety(full.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if fullSafety.Tainted {
		t.Fatalf("native-full block was tainted: %#v", fullSafety)
	}

	hashes, err = node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	next := node.chain.blockchain.GetBlockByHash(hashes[0])
	record, exists, err = loadExecutionRequestRecord(node.chain, next.Hash())
	if err != nil || !exists {
		t.Fatalf("load deferred control record: exists=%v err=%v", exists, err)
	}
	if len(record.Controls.Withdrawals) != 1 || record.Controls.Withdrawals[0].ID != 1 ||
		!node.pendingExecutionRequests.empty() {
		t.Fatalf("deferred control record=%#v queue=%#v", record, node.pendingExecutionRequests)
	}
}

func TestExecutionRequestControlReorgRestoresFIFOAndTemporaryOverflow(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if err := node.CreateBranch(context.Background(), "genesis", 0); err != nil {
		t.Fatal(err)
	}
	_, request, _ := testExecutionRequests()
	request.Amount = 1
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	originalHashes, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.CreateBranch(context.Background(), "original", 1); err != nil {
		t.Fatal(err)
	}
	for index := range maxWithdrawalRequestsPerPayload {
		request.Amount = uint64(index + 2)
		if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if len(node.pendingExecutionRequests.Withdrawals) != maxWithdrawalRequestsPerPayload {
		t.Fatal("test did not fill the control queue")
	}
	branchHashes, err := node.MineBranch(context.Background(), "genesis", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.pendingExecutionRequests.Withdrawals) != maxWithdrawalRequestsPerPayload {
		t.Fatal("non-canonical branch mining consumed controls")
	}
	branchRecord, exists, err := loadExecutionRequestRecord(node.chain, branchHashes[0])
	if err != nil || !exists || len(branchRecord.Controls.Withdrawals) != 0 {
		t.Fatalf("branch request record = %#v exists=%v err=%v", branchRecord, exists, err)
	}
	if err := node.SwitchBranch(context.Background(), "genesis"); err != nil {
		t.Fatal(err)
	}
	assertQueuedExecutionRequestIDRange(t, node.pendingExecutionRequests.Withdrawals, 1, 17)

	firstAlternative, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	assertQueuedExecutionRequestIDs(t, node.pendingExecutionRequests.Withdrawals, 17)
	firstRecord, exists, err := loadExecutionRequestRecord(node.chain, firstAlternative[0])
	if err != nil || !exists || len(firstRecord.Controls.Withdrawals) != maxWithdrawalRequestsPerPayload {
		t.Fatalf("overflow drain record = %#v exists=%v err=%v", firstRecord, exists, err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if !node.pendingExecutionRequests.empty() {
		t.Fatal("temporary overflow did not drain across two blocks")
	}

	revision := node.Revision()
	if err := node.SwitchBranch(context.Background(), "original"); err != nil {
		t.Fatal(err)
	}
	if head := node.chain.blockchain.CurrentBlock().Hash(); head != originalHashes[0] {
		t.Fatalf("switched head = %s, want %s", head, originalHashes[0])
	}
	assertQueuedExecutionRequestIDs(t, node.pendingExecutionRequests.Withdrawals,
		2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17,
	)
	events, err := node.EventsSince(revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || !events[0].Removed || !events[1].Removed || !events[2].Removed ||
		events[0].BlockNumber != 3 || events[1].BlockNumber != 2 || events[2].BlockNumber != 1 ||
		events[3].Removed || events[3].BlockHash != originalHashes[0] || events[4].Type != "chain_reorg" {
		t.Fatalf("reorg publication order = %#v", events)
	}
}

func TestNativeExecutionRequestContractStateFollowsReorg(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if err := node.CreateBranch(context.Background(), "empty", 0); err != nil {
		t.Fatal(err)
	}
	emptyHashes, err := node.MineBranch(context.Background(), "empty", 1)
	if err != nil {
		t.Fatal(err)
	}
	account := testWalletAccount(t, node, 0)
	for nonce := range uint64(maxWithdrawalRequestsPerPayload + 1) {
		pubkey := bytes.Repeat([]byte{byte(nonce + 1)}, 48)
		data := append(pubkey, make([]byte, 8)...)
		transaction := signExecutionRequestTransaction(
			t, cfg, account, nonce, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), data, 500_000,
		)
		if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
			t.Fatal(err)
		}
	}
	queuedHashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	queuedRecord, exists, err := loadExecutionRequestRecord(node.chain, queuedHashes[0])
	if err != nil || !exists {
		t.Fatalf("load queued-state request record: exists=%v err=%v", exists, err)
	}
	queuedEntries, err := executionRequestEntries(queuedRecord.Requests, executionRequestWithdrawal)
	if err != nil || len(queuedEntries) != maxWithdrawalRequestsPerPayload {
		t.Fatalf("first native queue output = %d entries, %v", len(queuedEntries), err)
	}
	if err := node.CreateBranch(context.Background(), "queued", 1); err != nil {
		t.Fatal(err)
	}

	if err := node.SwitchBranch(context.Background(), "empty"); err != nil {
		t.Fatal(err)
	}
	if node.chain.blockchain.CurrentBlock().Hash() != emptyHashes[0] {
		t.Fatal("did not switch to the empty-contract-state branch")
	}
	emptyNext, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	emptyRecord, exists, err := loadExecutionRequestRecord(node.chain, emptyNext[0])
	if err != nil || !exists || len(emptyRecord.Requests) != 0 {
		t.Fatalf("empty branch inherited orphan native queue: record=%#v exists=%v err=%v", emptyRecord, exists, err)
	}

	if err := node.SwitchBranch(context.Background(), "queued"); err != nil {
		t.Fatal(err)
	}
	remainingHash, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	remainingRecord, exists, err := loadExecutionRequestRecord(node.chain, remainingHash[0])
	if err != nil || !exists {
		t.Fatalf("load restored native queue record: exists=%v err=%v", exists, err)
	}
	remainingEntries, err := executionRequestEntries(remainingRecord.Requests, executionRequestWithdrawal)
	if err != nil || len(remainingEntries) != 1 || len(remainingRecord.Controls.Withdrawals) != 0 {
		t.Fatalf("restored native queue output = %x, controls=%#v err=%v", remainingEntries, remainingRecord.Controls, err)
	}
	if node.SafetyStatus().SessionTainted {
		t.Fatal("native request contract reorg tainted the session")
	}
}

func TestNativeExecutionRequestContractQueueSurvivesPebbleRestart(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	account := testWalletAccount(t, first, 0)
	for nonce := range uint64(maxWithdrawalRequestsPerPayload + 1) {
		pubkey := bytes.Repeat([]byte{byte(nonce + 1)}, 48)
		data := append(pubkey, make([]byte, 8)...)
		transaction := signExecutionRequestTransaction(
			t, cfg, account, nonce, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), data, 500_000,
		)
		if _, err := first.SendTransaction(context.Background(), transaction); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	defer second.Close() //nolint:errcheck
	hashes, err := second.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	record, exists, err := loadExecutionRequestRecord(second.chain, hashes[0])
	if err != nil || !exists {
		t.Fatalf("load restarted native queue record: exists=%v err=%v", exists, err)
	}
	entries, err := executionRequestEntries(record.Requests, executionRequestWithdrawal)
	if err != nil || len(entries) != 1 || len(record.Controls.Withdrawals) != 0 {
		t.Fatalf("restarted native queue output = %x controls=%#v err=%v", entries, record.Controls, err)
	}
	if second.SafetyStatus().SessionTainted {
		t.Fatal("native contract queue restart tainted the session")
	}
}

func TestExecutionRequestSnapshotRestoresConsumedAndKeepsLaterPending(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	snapshot, err := node.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, request, _ := testExecutionRequests()
	request.Amount = 1
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	request.Amount = 2
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if ok, err := node.Revert(context.Background(), snapshot); err != nil || !ok {
		t.Fatalf("snapshot revert = %v, %v", ok, err)
	}
	assertQueuedExecutionRequestIDs(t, node.pendingExecutionRequests.Withdrawals, 1, 2)
}

func TestExecutionRequestCheckpointRepeatablyRestoresControls(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if err := node.Checkpoint(context.Background(), "base"); err != nil {
		t.Fatal(err)
	}
	_, request, _ := testExecutionRequests()
	request.Amount = 1
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	request.Amount = 2
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		if err := node.Restore(context.Background(), "base"); err != nil {
			t.Fatal(err)
		}
		assertQueuedExecutionRequestIDs(t, node.pendingExecutionRequests.Withdrawals, 1, 2)
		if _, err := node.Mine(context.Background(), 1, true); err != nil {
			t.Fatal(err)
		}
		if !node.pendingExecutionRequests.empty() {
			t.Fatalf("checkpoint attempt %d did not consume restored controls", attempt)
		}
	}
}

func TestExecutionRequestControlBlockConsumesQueue(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	_, request, _ := testExecutionRequests()
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	nonce := uint64(1)
	hash, err := node.ApplyControl(context.Background(), ControlChanges{
		common.HexToAddress("0xc000000000000000000000000000000000000001"): {Nonce: &nonce},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, exists, err := loadExecutionRequestRecord(node.chain, hash)
	if err != nil || !exists || len(record.Controls.Withdrawals) != 1 || !node.pendingExecutionRequests.empty() {
		t.Fatalf("control block request record = %#v exists=%v err=%v queue=%#v", record, exists, err, node.pendingExecutionRequests)
	}
	if valid, err := node.VerifyControlRecord(context.Background(), hash); err != nil || !valid {
		t.Fatalf("verify state-control block with execution request = %v, %v", valid, err)
	}
	safety, err := node.BlockSafety(hash)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(safety.Reasons, taintControlStateOverride) ||
		!slices.Contains(safety.Reasons, taintExecutionRequestControl) {
		t.Fatalf("control block reasons = %#v", safety)
	}
}

func TestExecutionRequestControlsConsumeOnTransactionAndBatchMining(t *testing.T) {
	t.Run("transaction mining", func(t *testing.T) {
		cfg := testConfig()
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err != nil {
			t.Fatal(err)
		}
		defer node.Close() //nolint:errcheck
		_, request, _ := testExecutionRequests()
		if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if head := node.chain.blockchain.CurrentBlock().Number.Uint64(); head != 0 {
			t.Fatalf("request enqueue auto-mined block %d", head)
		}
		account := testWalletAccount(t, node, 0)
		to := node.Accounts()[1]
		transaction := signExecutionRequestTransaction(t, cfg, account, 0, &to, big.NewInt(1), nil, 21_000)
		if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
			t.Fatal(err)
		}
		head := node.chain.blockchain.GetBlockByNumber(1)
		record, exists, err := loadExecutionRequestRecord(node.chain, head.Hash())
		if err != nil || !exists || len(record.Controls.Withdrawals) != 1 || !node.pendingExecutionRequests.empty() {
			t.Fatalf("transaction-mined request record = %#v exists=%v err=%v", record, exists, err)
		}
	})

	t.Run("batch mining", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = miningModeManual
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err != nil {
			t.Fatal(err)
		}
		defer node.Close() //nolint:errcheck
		_, request, _ := testExecutionRequests()
		if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		hashes, err := node.Mine(context.Background(), 3, true)
		if err != nil {
			t.Fatal(err)
		}
		for index, hash := range hashes {
			record, exists, err := loadExecutionRequestRecord(node.chain, hash)
			if err != nil || !exists {
				t.Fatalf("load batch block %d record: exists=%v err=%v", index, exists, err)
			}
			wantControls := 0
			if index == 0 {
				wantControls = 1
			}
			if len(record.Controls.Withdrawals) != wantControls {
				t.Fatalf("batch block %d controls = %#v, want %d", index, record.Controls, wantControls)
			}
		}
	})
}

func TestPrePragueIntervalMiningQueuesUntilExactBoundary(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.Forks.PragueEpoch = 1
	cfg.Chain.Forks.OsakaEpoch = 1
	cfg.Mining.Mode = "interval"
	cfg.Mining.Interval = 10 * time.Millisecond
	cfg.Mining.AutoMineEmpty = false
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	_, request, _ := testExecutionRequests()
	if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if node.chain.pendingBlock().RequestsHash() != nil {
		t.Fatal("pre-Prague pending block has a requests hash")
	}
	waitForHead(t, node, cfg.Chain.SlotsPerEpoch)
	for number := uint64(1); number < cfg.Chain.SlotsPerEpoch; number++ {
		if hash := node.chain.blockchain.GetBlockByNumber(number).RequestsHash(); hash != nil {
			t.Fatalf("pre-Prague block %d has requests hash %s", number, *hash)
		}
	}
	boundary := node.chain.blockchain.GetBlockByNumber(cfg.Chain.SlotsPerEpoch)
	record, exists, err := loadExecutionRequestRecord(node.chain, boundary.Hash())
	if err != nil || !exists || len(record.Controls.Withdrawals) != 1 {
		t.Fatalf("Prague boundary record = %#v exists=%v err=%v", record, exists, err)
	}
	if !node.pendingExecutionRequests.empty() {
		t.Fatal("Prague boundary did not consume the queued control")
	}
}

func TestExecutionRequestPersistenceRestartArchiveAndRecovery(t *testing.T) {
	t.Run("restart and archive", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = miningModeManual
		cfg.Storage.Engine = "pebble"
		cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
		first, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Start(); err != nil {
			t.Fatal(err)
		}
		deposit, withdrawal, consolidation := testExecutionRequests()
		if err := first.AddDepositRequest(context.Background(), deposit); err != nil {
			t.Fatal(err)
		}
		if err := first.AddWithdrawalRequest(context.Background(), withdrawal); err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}

		second, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		assertQueuedExecutionRequestIDs(t, second.pendingExecutionRequests.Deposits, 1)
		assertQueuedExecutionRequestIDs(t, second.pendingExecutionRequests.Withdrawals, 2)
		if err := second.Start(); err != nil {
			t.Fatal(err)
		}
		hashes, err := second.Mine(context.Background(), 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := second.AddConsolidationRequest(context.Background(), consolidation); err != nil {
			t.Fatal(err)
		}
		archivePath := filepath.Join(t.TempDir(), "state.tar.zst")
		if err := second.DumpState(archivePath); err != nil {
			t.Fatal(err)
		}
		manifest, err := InspectState(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		if !manifest.Tainted || !manifest.HeadTainted || !slices.Contains(manifest.TaintReasons, taintExecutionRequestControl) {
			t.Fatalf("archive manifest = %#v", manifest)
		}
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}

		destination := filepath.Join(t.TempDir(), "loaded")
		if err := LoadState(archivePath, destination); err != nil {
			t.Fatal(err)
		}
		loadedConfig := cfg
		loadedConfig.Storage.Path = destination
		loaded, err := New(loadedConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer loaded.Close() //nolint:errcheck
		if loaded.chain.blockchain.CurrentBlock().Hash() != hashes[0] {
			t.Fatalf("loaded head/queue = %s %#v", loaded.chain.blockchain.CurrentBlock().Hash(), loaded.pendingExecutionRequests)
		}
		assertQueuedExecutionRequestIDs(t, loaded.pendingExecutionRequests.Consolidations, 3)
		if len(loaded.pendingExecutionRequests.Deposits)+len(loaded.pendingExecutionRequests.Withdrawals) != 0 ||
			loaded.pendingExecutionRequests.NextID != 4 {
			t.Fatalf("loaded pending execution request queue = %#v", loaded.pendingExecutionRequests)
		}
		assertExecutionRequestHash(t, loaded.chain.pendingBlock(), [][]byte{
			append([]byte{executionRequestConsolidation}, encodeExecutionConsolidationRequest(consolidation)...),
		})
		record, exists, err := loadExecutionRequestRecord(loaded.chain, hashes[0])
		if err != nil || !exists || len(record.Controls.Deposits) != 1 || len(record.Controls.Withdrawals) != 1 {
			t.Fatalf("loaded request record = %#v exists=%v err=%v", record, exists, err)
		}
		state, err := loaded.chain.blockchain.State()
		if err != nil {
			t.Fatal(err)
		}
		if len(state.GetCode(params.WithdrawalQueueAddress)) == 0 || !loaded.SafetyStatus().SessionTainted {
			t.Fatal("archive did not retain geth predeploy state or request-control taint")
		}
	})

	for _, test := range []struct {
		name         string
		stage        commitStage
		wantHead     uint64
		wantPending  int
		wantConsumed bool
	}{
		{name: "prepared", stage: commitStagePrepared, wantPending: 1},
		{name: "execution", stage: commitStageExecution, wantHead: 1, wantConsumed: true},
		{name: "auxiliary", stage: commitStageAuxiliary, wantHead: 1, wantConsumed: true},
	} {
		t.Run("recovery "+test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Mining.Mode = miningModeManual
			cfg.Storage.Engine = "pebble"
			cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
			first, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Start(); err != nil {
				t.Fatal(err)
			}
			_, request, _ := testExecutionRequests()
			if err := first.AddWithdrawalRequest(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			first.commitHook = func(stage commitStage) error {
				if stage == test.stage {
					return errors.New("injected execution request crash boundary")
				}
				return nil
			}
			if _, err := first.Mine(context.Background(), 1, true); err == nil {
				t.Fatal("expected injected persistence failure")
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}

			second, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close() //nolint:errcheck
			if head := second.chain.blockchain.CurrentBlock().Number.Uint64(); head != test.wantHead {
				t.Fatalf("recovered head = %d, want %d", head, test.wantHead)
			}
			if len(second.pendingExecutionRequests.Withdrawals) != test.wantPending {
				t.Fatalf("recovered queue = %#v", second.pendingExecutionRequests)
			}
			if test.wantConsumed {
				head := second.chain.blockchain.CurrentBlock()
				record, exists, err := loadExecutionRequestRecord(second.chain, head.Hash())
				if err != nil || !exists || len(record.Controls.Withdrawals) != 1 || !second.SafetyStatus().SessionTainted {
					t.Fatalf("recovered record/status = %#v exists=%v err=%v status=%#v", record, exists, err, second.SafetyStatus())
				}
			}
		})
	}
}

func TestExecutionRequestPersistenceFailureDisablesWrites(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	original := node.chain.db
	node.chain.db = failingExecutionRequestDatabase{Database: original}
	_, withdrawal, _ := testExecutionRequests()
	if err := node.AddWithdrawalRequest(context.Background(), withdrawal); err == nil ||
		!strings.Contains(err.Error(), "injected execution request queue failure") {
		t.Fatalf("execution request persistence error = %v", err)
	}
	if node.writeErr == nil || !node.pendingExecutionRequests.empty() {
		t.Fatalf("failed persistence status/queue = %v %#v", node.writeErr, node.pendingExecutionRequests)
	}
	if exists, err := original.Has(executionRequestQueueKey); err != nil || exists {
		t.Fatalf("failed persistence left queue record: exists=%v err=%v", exists, err)
	}
}

func TestCorruptExecutionRequestMetadataFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name string
		key  func(common.Hash) []byte
	}{
		{name: "queue", key: func(common.Hash) []byte { return executionRequestQueueKey }},
		{name: "block record", key: executionRequestRecordKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Mining.Mode = miningModeManual
			cfg.Storage.Engine = "pebble"
			cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
			node, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := node.Start(); err != nil {
				t.Fatal(err)
			}
			_, request, _ := testExecutionRequests()
			if err := node.AddWithdrawalRequest(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			hashes, err := node.Mine(context.Background(), 1, true)
			if err != nil {
				t.Fatal(err)
			}
			if err := node.Close(); err != nil {
				t.Fatal(err)
			}
			kv, err := pebble.New(cfg.Storage.Path, 64, 64, "execution-request-corruption", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := kv.Put(test.key(hashes[0]), []byte("{")); err != nil {
				t.Fatal(err)
			}
			if err := kv.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "execution request") {
				t.Fatalf("corrupt execution request metadata error = %v", err)
			}
		})
	}
}

func TestMissingExecutionRequestRecordMigrationAndProjectionMismatch(t *testing.T) {
	t.Run("derive native record", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = miningModeManual
		cfg.Storage.Engine = "pebble"
		cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
		hash := mineNativeWithdrawalRequestBlock(t, cfg)
		kv, err := pebble.New(cfg.Storage.Path, 64, 64, "execution-request-migration", false)
		if err != nil {
			t.Fatal(err)
		}
		if err := kv.Delete(executionRequestRecordKey(hash)); err != nil {
			t.Fatal(err)
		}
		if err := kv.Close(); err != nil {
			t.Fatal(err)
		}
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer node.Close() //nolint:errcheck
		record, exists, err := loadExecutionRequestRecord(node.chain, hash)
		if err != nil || !exists || len(record.NativeRequests) != 1 ||
			!equalExecutionRequestBytes(record.NativeRequests, record.Requests) ||
			len(record.Controls.Withdrawals) != 0 {
			t.Fatalf("derived native request record = %#v exists=%v err=%v", record, exists, err)
		}
	})

	t.Run("reject old empty projection for nonempty hash", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = miningModeManual
		cfg.Storage.Engine = "pebble"
		cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
		hash := mineNativeWithdrawalRequestBlock(t, cfg)
		kv, err := pebble.New(cfg.Storage.Path, 64, 64, "execution-request-mismatch", false)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := kv.Get(projectionKey(hash))
		if err != nil {
			t.Fatal(err)
		}
		var projection storedProjection
		if err := json.Unmarshal(encoded, &projection); err != nil {
			t.Fatal(err)
		}
		var signed electra.SignedBeaconBlock
		if err := signed.UnmarshalSSZ(projection.SignedSSZ); err != nil {
			t.Fatal(err)
		}
		signed.Message.Body.ExecutionRequests = &electra.ExecutionRequests{
			Deposits: []*electra.DepositRequest{}, Withdrawals: []*electra.WithdrawalRequest{},
			Consolidations: []*electra.ConsolidationRequest{},
		}
		projection.SignedSSZ, err = signed.MarshalSSZ()
		if err != nil {
			t.Fatal(err)
		}
		projection.Root, err = signed.Message.HashTreeRoot()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err = json.Marshal(projection)
		if err != nil {
			t.Fatal(err)
		}
		if err := kv.Put(projectionKey(hash), encoded); err != nil {
			t.Fatal(err)
		}
		if err := kv.Delete(executionRequestRecordKey(hash)); err != nil {
			t.Fatal(err)
		}
		if err := kv.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "requests hash mismatch") {
			t.Fatalf("old empty projection mismatch error = %v", err)
		}
	})
}

func testExecutionRequests() (ExecutionDepositRequest, ExecutionWithdrawalRequest, ExecutionConsolidationRequest) {
	deposit := ExecutionDepositRequest{Amount: 32_000_000_000, Index: 7}
	copy(deposit.Pubkey[:], bytes.Repeat([]byte{0x11}, len(deposit.Pubkey)))
	copy(deposit.WithdrawalCredentials[:], bytes.Repeat([]byte{0x22}, len(deposit.WithdrawalCredentials)))
	copy(deposit.Signature[:], bytes.Repeat([]byte{0x33}, len(deposit.Signature)))
	withdrawal := ExecutionWithdrawalRequest{
		SourceAddress: common.HexToAddress("0x1000000000000000000000000000000000000001"), Amount: 5,
	}
	copy(withdrawal.ValidatorPubkey[:], bytes.Repeat([]byte{0x44}, len(withdrawal.ValidatorPubkey)))
	consolidation := ExecutionConsolidationRequest{
		SourceAddress: common.HexToAddress("0x2000000000000000000000000000000000000002"),
	}
	copy(consolidation.SourcePubkey[:], bytes.Repeat([]byte{0x55}, len(consolidation.SourcePubkey)))
	copy(consolidation.TargetPubkey[:], bytes.Repeat([]byte{0x66}, len(consolidation.TargetPubkey)))
	return deposit, withdrawal, consolidation
}

func executionRequestTestBlock(requests [][]byte) *types.Block {
	hash := types.CalcRequestsHash(requests)
	return types.NewBlockWithHeader(&types.Header{Number: big.NewInt(1), RequestsHash: &hash})
}

func assertExecutionRequestHash(t *testing.T, block *types.Block, requests [][]byte) {
	t.Helper()
	want := types.CalcRequestsHash(requests)
	if block.RequestsHash() == nil || *block.RequestsHash() != want {
		t.Fatalf("block requests hash = %v, want %s", block.RequestsHash(), want)
	}
}

func assertQueuedExecutionRequestIDs(t *testing.T, items []queuedExecutionRequest, ids ...uint64) {
	t.Helper()
	if len(items) != len(ids) {
		t.Fatalf("queued execution request IDs = %#v, want %v", items, ids)
	}
	for index, id := range ids {
		if items[index].ID != id {
			t.Fatalf("queued execution request ID %d = %d, want %d", index, items[index].ID, id)
		}
	}
}

func assertQueuedExecutionRequestIDRange(t *testing.T, items []queuedExecutionRequest, first, last uint64) {
	t.Helper()
	ids := make([]uint64, 0, last-first+1)
	for id := first; id <= last; id++ {
		ids = append(ids, id)
	}
	assertQueuedExecutionRequestIDs(t, items, ids...)
}

func depositLogRuntime() []byte {
	topic := common.HexToHash("0x649bbc62d0e31342afea4e5cd82d4049e7e1ee912fc0889aa790803be39038c5")
	runtime := []byte{0x36, 0x60, 0x00, 0x60, 0x00, 0x37, 0x7f}
	runtime = append(runtime, topic.Bytes()...)
	return append(runtime, 0x36, 0x60, 0x00, 0xa1, 0x00)
}

func contractCreationCode(runtime []byte) []byte {
	prefix := []byte{0x60, byte(len(runtime)), 0x60, 0x0c, 0x60, 0x00, 0x39, 0x60, byte(len(runtime)), 0x60, 0x00, 0xf3}
	return append(prefix, runtime...)
}

func depositLogData(t *testing.T, request ExecutionDepositRequest) []byte {
	t.Helper()
	bytesType, err := abi.NewType("bytes", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	arguments := abi.Arguments{{Type: bytesType}, {Type: bytesType}, {Type: bytesType}, {Type: bytesType}, {Type: bytesType}}
	var amount, index [8]byte
	binary.LittleEndian.PutUint64(amount[:], request.Amount)
	binary.LittleEndian.PutUint64(index[:], request.Index)
	data, err := arguments.Pack(
		request.Pubkey[:], request.WithdrawalCredentials[:], amount[:], request.Signature[:], index[:],
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 576 {
		t.Fatalf("deposit event data has length %d, want 576", len(data))
	}
	return data
}

func signExecutionRequestTransaction(
	t *testing.T,
	cfg Config,
	account Account,
	nonce uint64,
	to *common.Address,
	value *big.Int,
	data []byte,
	gas uint64,
) *types.Transaction {
	t.Helper()
	chainID := new(big.Int).SetUint64(cfg.Chain.ChainID)
	transaction, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: nonce, To: to, Value: new(big.Int).Set(value), Data: bytes.Clone(data), Gas: gas,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(5_000_000_000),
	}), types.LatestSignerForChainID(chainID), account.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return transaction
}

func mineNativeWithdrawalRequestBlock(t *testing.T, cfg Config) common.Hash {
	t.Helper()
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	account := testWalletAccount(t, node, 0)
	data := append(bytes.Repeat([]byte{0xa1}, 48), make([]byte, 8)...)
	transaction := signExecutionRequestTransaction(
		t, cfg, account, 0, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), data, 500_000,
	)
	if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
		t.Fatal(err)
	}
	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	return hashes[0]
}

func addressPointer(address common.Address) *common.Address {
	value := address
	return &value
}
