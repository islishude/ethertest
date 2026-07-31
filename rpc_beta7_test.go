package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"

	gethaccounts "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	gethstate "github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

var beta7ImplementedMethods = []string{
	"debug_getRawBlock", "debug_getRawHeader", "debug_getRawReceipts", "debug_getRawTransaction",
	"eth_accounts", "eth_blobBaseFee", "eth_blockNumber", "eth_call", "eth_capabilities", "eth_chainId",
	"eth_coinbase", "eth_config", "eth_createAccessList", "eth_estimateGas", "eth_feeHistory", "eth_gasPrice",
	"eth_getBalance", "eth_getBlockByHash", "eth_getBlockByNumber", "eth_getBlockReceipts",
	"eth_getBlockTransactionCountByHash", "eth_getBlockTransactionCountByNumber", "eth_getCode",
	"eth_getFilterChanges", "eth_getFilterLogs", "eth_getLogs", "eth_getProof", "eth_getStorageAt",
	"eth_getStorageValues", "eth_getTransactionByBlockHashAndIndex", "eth_getTransactionByBlockNumberAndIndex",
	"eth_getTransactionByHash", "eth_getTransactionCount", "eth_getTransactionReceipt", "eth_maxPriorityFeePerGas",
	"eth_newBlockFilter", "eth_newFilter", "eth_newPendingTransactionFilter", "eth_sendRawTransaction",
	"eth_sendTransaction", "eth_sign", "eth_signTransaction", "eth_simulateV1", "eth_syncing", "eth_uninstallFilter",
	"net_version", "txpool_content", "txpool_contentFrom", "txpool_status",
}

var beta7ExcludedMethods = []string{
	"debug_getBadBlocks", "debug_getRawBlockAccessList",
	"engine_exchangeCapabilities", "engine_exchangeTransitionConfigurationV1",
	"engine_forkchoiceUpdatedV1", "engine_forkchoiceUpdatedV2", "engine_forkchoiceUpdatedV3", "engine_forkchoiceUpdatedV4",
	"engine_getBlobsV1", "engine_getBlobsV2", "engine_getBlobsV3", "engine_getBlobsV4",
	"engine_getPayloadBodiesByHashV1", "engine_getPayloadBodiesByHashV2",
	"engine_getPayloadBodiesByRangeV1", "engine_getPayloadBodiesByRangeV2",
	"engine_getPayloadV1", "engine_getPayloadV2", "engine_getPayloadV3", "engine_getPayloadV4", "engine_getPayloadV5", "engine_getPayloadV6",
	"engine_newPayloadV1", "engine_newPayloadV2", "engine_newPayloadV3", "engine_newPayloadV4", "engine_newPayloadV5",
	"eth_getBlockAccessList", "testing_buildBlockV1",
}

func TestExecutionAPIBeta7RegistrationAudit(t *testing.T) {
	if len(beta7ImplementedMethods) != 49 || len(beta7ExcludedMethods) != 29 {
		t.Fatalf("audit lists have %d implemented and %d excluded methods", len(beta7ImplementedMethods), len(beta7ExcludedMethods))
	}
	node := startRPCNode(t, testConfig())
	client := node.RPCClient()
	defer client.Close()
	for _, method := range beta7ImplementedMethods {
		t.Run("implemented/"+method, func(t *testing.T) {
			var result json.RawMessage
			err := client.Call(&result, method)
			var rpcErr rpc.Error
			if errors.As(err, &rpcErr) && rpcErr.ErrorCode() == -32601 {
				t.Fatalf("%s is not registered", method)
			}
		})
	}
	for _, method := range beta7ExcludedMethods {
		t.Run("excluded/"+method, func(t *testing.T) {
			var result json.RawMessage
			assertRPCErrorCode(t, client.Call(&result, method), -32601)
		})
	}
}

func TestBeta7BlockTransactionStorageAndRawQueries(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()

	var emptyReceipts []map[string]any
	if err := client.Call(&emptyReceipts, "eth_getBlockReceipts", "latest"); err != nil {
		t.Fatal(err)
	}
	if emptyReceipts == nil || len(emptyReceipts) != 0 {
		t.Fatalf("genesis receipts = %#v, want []", emptyReceipts)
	}
	var genesisCount hexutil.Uint
	if err := client.Call(&genesisCount, "eth_getBlockTransactionCountByNumber", "earliest"); err != nil || genesisCount != 0 {
		t.Fatalf("genesis transaction count = %d, %v", genesisCount, err)
	}
	var missing json.RawMessage
	if err := client.Call(&missing, "eth_getBlockReceipts", common.HexToHash("0xdead")); err != nil {
		t.Fatal(err)
	}
	if string(missing) != "null" {
		t.Fatalf("missing receipts encoded as %s", missing)
	}
	if err := client.Call(&missing, "eth_getBlockTransactionCountByHash", common.HexToHash("0xdead")); err != nil {
		t.Fatal(err)
	}
	if string(missing) != "null" {
		t.Fatalf("missing transaction count encoded as %s", missing)
	}

	accounts := node.Accounts()
	var defaultCallResult hexutil.Bytes
	if err := client.Call(&defaultCallResult, "eth_call", map[string]any{"to": accounts[0]}); err != nil {
		t.Fatalf("eth_call without a block selector did not default to latest: %v", err)
	}
	var transactionHash common.Hash
	if err := client.Call(&transactionHash, "eth_sendTransaction", map[string]any{
		"from": accounts[0], "to": accounts[1], "value": "0x2a", "gas": "0x5208",
	}); err != nil {
		t.Fatal(err)
	}
	var pendingCount hexutil.Uint
	if err := client.Call(&pendingCount, "eth_getBlockTransactionCountByNumber", "pending"); err != nil || pendingCount != 1 {
		t.Fatalf("pending count = %d, %v", pendingCount, err)
	}
	var pendingTx rpcTransaction
	if err := client.Call(&pendingTx, "eth_getTransactionByBlockNumberAndIndex", "pending", "0x0"); err != nil {
		t.Fatal(err)
	}
	if pendingTx.Hash != transactionHash || pendingTx.BlockHash != nil || pendingTx.BlockNumber != nil || pendingTx.BlockTimestamp != nil || pendingTx.TransactionIndex != nil {
		t.Fatalf("pending transaction inclusion fields = %#v", pendingTx)
	}
	var pendingReceipts []map[string]any
	if err := client.Call(&pendingReceipts, "eth_getBlockReceipts", "pending"); err != nil || len(pendingReceipts) != 1 {
		t.Fatalf("pending receipts = %#v, %v", pendingReceipts, err)
	}
	node.chain.mu.Lock()
	savedPendingReceipts := node.chain.pendingView.receipts
	node.chain.pendingView.receipts = types.Receipts{}
	node.chain.mu.Unlock()
	if err := client.Call(&pendingReceipts, "eth_getBlockReceipts", "pending"); err == nil || !strings.Contains(err.Error(), "receipts length mismatch") {
		t.Fatalf("pending receipt mismatch error = %v", err)
	}
	node.chain.mu.Lock()
	node.chain.pendingView.receipts = savedPendingReceipts
	node.chain.mu.Unlock()

	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	if block == nil {
		t.Fatal("mined block not found")
	}
	for _, tag := range []any{"latest", "safe", "finalized", hexutil.Uint64(block.NumberU64())} {
		var receipts []map[string]any
		if err := client.Call(&receipts, "eth_getBlockReceipts", tag); err != nil {
			t.Fatalf("get receipts for %v: %v", tag, err)
		}
	}
	var countByHash hexutil.Uint
	if err := client.Call(&countByHash, "eth_getBlockTransactionCountByHash", block.Hash()); err != nil || countByHash != 1 {
		t.Fatalf("count by hash = %d, %v", countByHash, err)
	}
	var transactionByHash rpcTransaction
	if err := client.Call(&transactionByHash, "eth_getTransactionByBlockHashAndIndex", block.Hash(), "0x0"); err != nil {
		t.Fatal(err)
	}
	if transactionByHash.Hash != transactionHash || transactionByHash.BlockHash == nil || *transactionByHash.BlockHash != block.Hash() {
		t.Fatalf("transaction by block hash = %#v", transactionByHash)
	}
	if err := client.Call(&missing, "eth_getTransactionByBlockNumberAndIndex", hexutil.Uint64(block.NumberU64()), "0x1"); err != nil {
		t.Fatal(err)
	}
	if string(missing) != "null" {
		t.Fatalf("out-of-range transaction encoded as %s", missing)
	}

	var rawHeader hexutil.Bytes
	if err := client.Call(&rawHeader, "debug_getRawHeader", hexutil.Uint64(block.NumberU64())); err != nil {
		t.Fatal(err)
	}
	var decodedHeader types.Header
	if err := rlp.DecodeBytes(rawHeader, &decodedHeader); err != nil || decodedHeader.Hash() != block.Hash() {
		t.Fatalf("raw header round trip failed: %v", err)
	}
	var rawBlock hexutil.Bytes
	if err := client.Call(&rawBlock, "debug_getRawBlock", hexutil.Uint64(block.NumberU64())); err != nil {
		t.Fatal(err)
	}
	var decodedBlock types.Block
	if err := rlp.DecodeBytes(rawBlock, &decodedBlock); err != nil || decodedBlock.Hash() != block.Hash() {
		t.Fatalf("raw block round trip failed: %v", err)
	}
	var rawReceipts []hexutil.Bytes
	if err := client.Call(&rawReceipts, "debug_getRawReceipts", hexutil.Uint64(block.NumberU64())); err != nil || len(rawReceipts) != 1 {
		t.Fatalf("raw receipts = %d, %v", len(rawReceipts), err)
	}
	var decodedReceipt types.Receipt
	if err := decodedReceipt.UnmarshalBinary(rawReceipts[0]); err != nil || decodedReceipt.Status != types.ReceiptStatusSuccessful || decodedReceipt.CumulativeGasUsed == 0 {
		t.Fatalf("raw receipt round trip failed: %v", err)
	}
	var rawTransaction hexutil.Bytes
	if err := client.Call(&rawTransaction, "debug_getRawTransaction", transactionHash); err != nil {
		t.Fatal(err)
	}
	var decodedTransaction types.Transaction
	if err := decodedTransaction.UnmarshalBinary(rawTransaction); err != nil || decodedTransaction.Hash() != transactionHash {
		t.Fatalf("raw transaction round trip failed: %v", err)
	}

	requests := map[common.Address][]string{accounts[9]: {"0x", "0x1"}}
	var storage map[common.Address][]hexutil.Bytes
	if err := client.Call(&storage, "eth_getStorageValues", requests, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(storage[accounts[9]]) != 2 || len(storage[accounts[9]][0]) != common.HashLength || common.BytesToHash(storage[accounts[9]][1]) != (common.Hash{}) {
		t.Fatalf("unknown storage values = %#v", storage)
	}
	var storageValue hexutil.Bytes
	if err := client.Call(&storageValue, "eth_getStorageAt", accounts[9], "0x1", "latest"); err != nil {
		t.Fatalf("short storage key: %v", err)
	}
	if len(storageValue) != common.HashLength || common.BytesToHash(storageValue) != (common.Hash{}) {
		t.Fatalf("short storage key value = %x", []byte(storageValue))
	}
	assertRPCErrorCode(t, client.Call(&storageValue, "eth_getStorageAt", accounts[9], "0x"+strings.Repeat("00", common.HashLength+1), "latest"), -32602)
	assertRPCErrorCode(t, client.Call(&storageValue, "eth_getStorageAt", accounts[9], "0xgg", "latest"), -32602)
	var proof accountProof
	fullKey := "0x" + strings.Repeat("00", common.HashLength)
	if err := client.Call(&proof, "eth_getProof", accounts[9], []string{"0x1", fullKey}, "latest"); err != nil {
		t.Fatalf("proof with bytesMax32 keys: %v", err)
	}
	if len(proof.StorageProof) != 2 || proof.StorageProof[0].Key != "0x1" || proof.StorageProof[1].Key != fullKey || proof.StorageProof[0].Proof == nil {
		t.Fatalf("storage proof key encoding = %#v", proof.StorageProof)
	}
	assertRPCErrorCode(t, client.Call(&proof, "eth_getProof", accounts[9], []string{"0xgg"}, "latest"), -32602)
	tooManyProofKeys := make([]string, maxGetProofKeys+1)
	assertRPCErrorCode(t, client.Call(&proof, "eth_getProof", accounts[9], tooManyProofKeys, "latest"), -32602)
	assertRPCErrorCode(t, client.Call(&storage, "eth_getStorageValues", map[common.Address][]string{}, "latest"), -32602)
	assertRPCErrorCode(t, client.Call(&storage, "eth_getStorageValues", map[common.Address][]string{accounts[0]: {"0xgg"}}, "latest"), -32602)
	tooMany := make([]string, maxGetStorageSlots+1)
	assertRPCErrorCode(t, client.Call(&storage, "eth_getStorageValues", map[common.Address][]string{accounts[0]: tooMany}, "latest"), -38026)
	var missingBalance hexutil.Big
	assertRPCErrorCode(t, client.Call(&missingBalance, "eth_getBalance", accounts[0], "0xffff"), -32001)
}

func TestBeta7FeeCapabilitiesAndConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.Forks.PragueEpoch = 1
	cfg.Chain.Forks.OsakaEpoch = 2
	cfg.Mining.Mode = "manual"
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()

	var blobFee hexutil.Big
	if err := client.Call(&blobFee, "eth_blobBaseFee"); err != nil {
		t.Fatal(err)
	}
	wantBlobFee := eip4844.CalcBlobFee(node.chain.config, node.chain.blockchain.CurrentBlock())
	if (*big.Int)(&blobFee).Cmp(wantBlobFee) != 0 {
		t.Fatalf("blob fee = %s, want %s", (*big.Int)(&blobFee), wantBlobFee)
	}
	for _, newest := range []string{"latest", "pending", "safe", "finalized"} {
		var history feeHistoryResult
		if err := client.Call(&history, "eth_feeHistory", "0x1", newest, []float64{0, 50, 100}); err != nil {
			t.Fatalf("fee history %s: %v", newest, err)
		}
		if history.OldestBlock == nil || len(history.BaseFee) != 2 || len(history.GasUsedRatio) != 1 || len(history.BlobBaseFee) != 2 || len(history.BlobGasUsedRatio) != 1 {
			t.Fatalf("fee history %s = %#v", newest, history)
		}
	}

	var capabilities capabilitiesResponse
	if err := client.Call(&capabilities, "eth_capabilities"); err != nil {
		t.Fatal(err)
	}
	if capabilities.Head.Number != 0 || capabilities.Head.Hash == (common.Hash{}) {
		t.Fatalf("capabilities head = %#v", capabilities.Head)
	}
	for name, resource := range map[string]capabilityResource{
		"state": capabilities.State, "tx": capabilities.Tx, "logs": capabilities.Logs,
		"receipts": capabilities.Receipts, "blocks": capabilities.Blocks, "stateproofs": capabilities.StateProofs,
	} {
		if resource.Disabled || resource.OldestBlock == nil || *resource.OldestBlock != 0 {
			t.Fatalf("capability %s = %#v", name, resource)
		}
	}

	var configuration executionConfigResponse
	if err := client.Call(&configuration, "eth_config"); err != nil {
		t.Fatal(err)
	}
	if configuration.Current == nil || configuration.Next == nil || configuration.Last == nil {
		t.Fatalf("fork configuration = %#v", configuration)
	}
	genesisTime := uint64(cfg.Chain.GenesisTime)
	pragueTime := genesisTime + cfg.Chain.SlotsPerEpoch*uint64(cfg.Chain.SlotDuration.Seconds())
	osakaTime := genesisTime + 2*cfg.Chain.SlotsPerEpoch*uint64(cfg.Chain.SlotDuration.Seconds())
	if configuration.Current.ActivationTime != 0 || configuration.Next.ActivationTime != pragueTime || configuration.Last.ActivationTime != osakaTime {
		t.Fatalf("activation times = %d/%d/%d", configuration.Current.ActivationTime, configuration.Next.ActivationTime, configuration.Last.ActivationTime)
	}
	if len(configuration.Current.ForkID) != 4 || len(configuration.Current.Precompiles) == 0 || configuration.Current.BlobSchedule == nil {
		t.Fatalf("incomplete current config = %#v", configuration.Current)
	}
}

func TestBeta7NonArchiveCapabilitiesDescribeStateWindow(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Archive = false
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()

	var capabilities capabilitiesResponse
	if err := client.Call(&capabilities, "eth_capabilities"); err != nil {
		t.Fatal(err)
	}
	for name, resource := range map[string]capabilityResource{"state": capabilities.State, "stateproofs": capabilities.StateProofs} {
		if resource.Disabled || resource.OldestBlock == nil || *resource.OldestBlock != 0 || resource.DeleteStrategy == nil || resource.DeleteStrategy.Type != "window" || resource.DeleteStrategy.RetentionBlocks == nil || *resource.DeleteStrategy.RetentionBlocks == 0 {
			t.Fatalf("non-archive capability %s = %#v", name, resource)
		}
	}
	if capabilities.Blocks.DeleteStrategy != nil || capabilities.Tx.DeleteStrategy != nil || capabilities.Receipts.DeleteStrategy != nil || capabilities.Logs.DeleteStrategy != nil {
		t.Fatalf("non-state resources unexpectedly pruned: %#v", capabilities)
	}

	if _, err := node.Mine(context.Background(), uint64(gethstate.TriesInMemory)+2, true); err != nil {
		t.Fatalf("advance non-archive state window: %v", err)
	}
	if err := client.Call(&capabilities, "eth_capabilities"); err != nil {
		t.Fatal(err)
	}
	if capabilities.State.OldestBlock == nil || *capabilities.State.OldestBlock != 0 || capabilities.StateProofs.OldestBlock == nil || *capabilities.StateProofs.OldestBlock != *capabilities.State.OldestBlock {
		t.Fatalf("advanced non-archive capabilities = %#v", capabilities)
	}
	var oldProof accountProof
	if err := client.Call(&oldProof, "eth_getProof", params.BeaconRootsAddress, []string{"0x0"}, "0x1"); err != nil {
		t.Fatalf("capabilities omitted actually readable historical state: %v", err)
	}
	var oldStorage hexutil.Bytes
	if err := client.Call(&oldStorage, "eth_getStorageAt", params.BeaconRootsAddress, "0x0", "latest"); err != nil {
		t.Fatalf("latest state became unreadable: %v", err)
	}
}

func TestOldestContiguousState(t *testing.T) {
	if got := oldestContiguousState(130, func(uint64) bool { return true }); got != 0 {
		t.Fatalf("all-readable oldest state = %d, want 0", got)
	}
	if got := oldestContiguousState(130, func(number uint64) bool {
		return number == 0 || number >= 3
	}); got != 3 {
		t.Fatalf("pruned oldest state = %d, want 3", got)
	}
	if got := oldestContiguousState(130, func(number uint64) bool {
		return number <= 1 || number >= 3
	}); got != 3 {
		t.Fatalf("state window with an early repeated root = %d, want 3", got)
	}
}

func TestBeta7SigningAndAccessList(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	accounts := node.chain.accounts

	message := hexutil.Bytes("beta.7")
	var signature hexutil.Bytes
	if err := client.Call(&signature, "eth_sign", accounts[0].Address, message); err != nil {
		t.Fatal(err)
	}
	if len(signature) != crypto.SignatureLength || signature[crypto.RecoveryIDOffset] < 27 {
		t.Fatalf("signature = %x", []byte(signature))
	}
	recovery := append([]byte(nil), signature...)
	recovery[crypto.RecoveryIDOffset] -= 27
	publicKey, err := crypto.SigToPub(gethaccounts.TextHash(message), recovery)
	if err != nil || crypto.PubkeyToAddress(*publicKey) != accounts[0].Address {
		t.Fatalf("signature recovery failed: %v", err)
	}
	if err := client.Call(&signature, "eth_sign", common.HexToAddress("0xdead"), message); err == nil || !strings.Contains(err.Error(), "unknown unlocked account") {
		t.Fatalf("unknown account error = %v", err)
	}
	var ignored hexutil.Bytes
	if err := client.Call(&ignored, "eth_signTransaction", map[string]any{"from": common.HexToAddress("0xdead")}); err == nil || !strings.Contains(err.Error(), "unknown unlocked account") {
		t.Fatalf("unknown transaction signer error = %v", err)
	}

	chainID := new(big.Int).SetUint64(cfg.Chain.ChainID)
	authorization, err := types.SignSetCode(accounts[1].PrivateKey, types.SetCodeAuthorization{
		ChainID: *uint256.MustFromBig(chainID), Address: accounts[2].Address, Nonce: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		typeID uint64
		args   map[string]any
	}{
		{name: "legacy", typeID: types.LegacyTxType, args: map[string]any{"gasPrice": "0x3b9aca00"}},
		{name: "access-list", typeID: types.AccessListTxType, args: map[string]any{"gasPrice": "0x3b9aca00", "accessList": types.AccessList{}}},
		{name: "dynamic-fee", typeID: types.DynamicFeeTxType, args: map[string]any{"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00", "accessList": types.AccessList{}}},
		{name: "set-code", typeID: types.SetCodeTxType, args: map[string]any{"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00", "accessList": types.AccessList{}, "authorizationList": []types.SetCodeAuthorization{authorization}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := map[string]any{
				"type": hexutil.Uint64(test.typeID), "from": accounts[0].Address, "to": accounts[3].Address,
				"nonce": "0x0", "gas": "0x186a0", "value": "0x1", "chainId": hexutil.Uint64(cfg.Chain.ChainID),
			}
			for key, value := range test.args {
				args[key] = value
			}
			var raw hexutil.Bytes
			if err := client.Call(&raw, "eth_signTransaction", args); err != nil {
				t.Fatal(err)
			}
			var transaction types.Transaction
			if err := transaction.UnmarshalBinary(raw); err != nil {
				t.Fatal(err)
			}
			if transaction.Type() != uint8(test.typeID) {
				t.Fatalf("transaction type = %d", transaction.Type())
			}
			sender, err := types.Sender(types.LatestSignerForChainID(chainID), &transaction)
			if err != nil || sender != accounts[0].Address {
				t.Fatalf("sender = %s, %v", sender, err)
			}
		})
	}

	var blob kzg4844.Blob
	var rawBlob hexutil.Bytes
	if err := client.Call(&rawBlob, "eth_signTransaction", map[string]any{
		"type": "0x3", "from": accounts[0].Address, "to": accounts[3].Address, "nonce": "0x0", "gas": "0x186a0",
		"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00", "maxFeePerBlobGas": "0x2",
		"value": "0x0", "chainId": hexutil.Uint64(cfg.Chain.ChainID), "accessList": types.AccessList{}, "blobs": []kzg4844.Blob{blob},
	}); err != nil {
		t.Fatal(err)
	}
	var blobTransaction types.Transaction
	if err := blobTransaction.UnmarshalBinary(rawBlob); err != nil {
		t.Fatal(err)
	}
	sidecar := blobTransaction.BlobTxSidecar()
	if blobTransaction.Type() != types.BlobTxType || sidecar == nil || sidecar.Version != types.BlobSidecarVersion1 {
		t.Fatalf("signed blob transaction sidecar = %#v", sidecar)
	}
	if err := kzg4844.VerifyCellProofs(sidecar.Blobs, sidecar.Commitments, sidecar.Proofs); err != nil {
		t.Fatalf("signed sidecar proof verification: %v", err)
	}
	wrongHash := common.HexToHash("0x01")
	if err := client.Call(&ignored, "eth_signTransaction", map[string]any{
		"type": "0x3", "from": accounts[0].Address, "to": accounts[3].Address, "nonce": "0x0", "gas": "0x186a0",
		"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00", "maxFeePerBlobGas": "0x2",
		"value": "0x0", "chainId": hexutil.Uint64(cfg.Chain.ChainID), "blobs": []kzg4844.Blob{blob}, "blobVersionedHashes": []common.Hash{wrongHash},
	}); err == nil || !strings.Contains(err.Error(), "blob hash verification failed") {
		t.Fatalf("blob hash mismatch error = %v", err)
	}

	var status map[string]hexutil.Uint
	if err := client.Call(&status, "txpool_status"); err != nil || status["pending"] != 0 || status["queued"] != 0 {
		t.Fatalf("signTransaction broadcast into pool: %#v, %v", status, err)
	}
	assertRPCErrorCode(t, client.Call(&ignored, "eth_signTransaction", map[string]any{
		"type": "0x2", "from": accounts[0].Address, "to": accounts[3].Address, "nonce": "0x0", "gas": "0x5208",
		"gasPrice": "0x1", "maxFeePerGas": "0x2", "maxPriorityFeePerGas": "0x1",
	}), -32000)
	if err := client.Call(&ignored, "eth_signTransaction", map[string]any{
		"from": accounts[0].Address, "to": accounts[3].Address, "nonce": "0x0", "gas": "0x5208",
		"maxFeePerGas": "0x2", "maxPriorityFeePerGas": "0x1", "chainId": "0x1",
	}); err == nil || !strings.Contains(err.Error(), "chainId does not match") {
		t.Fatalf("chain ID mismatch error = %v", err)
	}

	contract := common.HexToAddress("0x1000000000000000000000000000000000000001")
	if err := client.Call(&ignored, "ethertest_setCode", contract, hexutil.Bytes{0x60, 0x00, 0x54, 0x00}); err != nil {
		t.Fatal(err)
	}
	var access accessListResult
	if err := client.Call(&access, "eth_createAccessList", map[string]any{"from": accounts[0].Address, "to": contract}); err != nil {
		t.Fatal(err)
	}
	if access.Error != "" || access.GasUsed == 0 || len(access.AccessList) != 1 || access.AccessList[0].Address != contract || len(access.AccessList[0].StorageKeys) != 1 {
		t.Fatalf("access list = %#v", access)
	}
	authorityReader := common.HexToAddress("0x1000000000000000000000000000000000000003")
	authorityCode := append([]byte{byte(vm.PUSH20)}, accounts[1].Address.Bytes()...)
	authorityCode = append(authorityCode, byte(vm.BALANCE), byte(vm.POP), byte(vm.STOP))
	if err := client.Call(&ignored, "ethertest_setCode", authorityReader, hexutil.Bytes(authorityCode)); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&access, "eth_createAccessList", map[string]any{
		"from": accounts[0].Address, "to": authorityReader, "authorizationList": []types.SetCodeAuthorization{authorization},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tuple := range access.AccessList {
		if tuple.Address == accounts[1].Address {
			t.Fatalf("valid EIP-7702 authority was not excluded: %#v", access)
		}
	}
	reverter := common.HexToAddress("0x1000000000000000000000000000000000000002")
	if err := client.Call(&ignored, "ethertest_setCode", reverter, hexutil.Bytes{0x60, 0x00, 0x60, 0x00, 0xfd}); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&access, "eth_createAccessList", map[string]any{"from": accounts[0].Address, "to": reverter}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(access.Error, "execution reverted") {
		t.Fatalf("reverting access-list result = %#v", access)
	}
}

func TestBeta7PreOsakaBlobSigningAndSubmission(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	cfg.Chain.Forks.PragueEpoch = 0
	cfg.Chain.Forks.OsakaEpoch = 1
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	accounts := node.Accounts()

	var blob kzg4844.Blob
	args := map[string]any{
		"type": "0x3", "from": accounts[0], "to": accounts[1], "nonce": "0x0", "gas": "0x186a0",
		"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00", "maxFeePerBlobGas": "0x2",
		"value": "0x0", "chainId": hexutil.Uint64(cfg.Chain.ChainID), "accessList": types.AccessList{}, "blobs": []kzg4844.Blob{blob},
	}
	commitment, err := kzg4844.BlobToCommitment(&blob)
	if err != nil {
		t.Fatal(err)
	}
	invalidProofArgs := make(map[string]any, len(args)+2)
	for key, value := range args {
		invalidProofArgs[key] = value
	}
	invalidProofArgs["commitments"] = []kzg4844.Commitment{commitment}
	invalidProofArgs["proofs"] = []kzg4844.Proof{{}}
	var rejectedRaw hexutil.Bytes
	if err := client.Call(&rejectedRaw, "eth_signTransaction", invalidProofArgs); err == nil || !strings.Contains(err.Error(), "failed to verify blob proof") {
		t.Fatalf("invalid blob proof error = %v", err)
	}
	var raw hexutil.Bytes
	if err := client.Call(&raw, "eth_signTransaction", args); err != nil {
		t.Fatal(err)
	}
	var transaction types.Transaction
	if err := transaction.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	sidecar := transaction.BlobTxSidecar()
	if sidecar == nil || sidecar.Version != types.BlobSidecarVersion0 || len(sidecar.Proofs) != 1 {
		t.Fatalf("pre-Osaka blob sidecar = %#v", sidecar)
	}
	if err := kzg4844.VerifyBlobProof(&sidecar.Blobs[0], sidecar.Commitments[0], sidecar.Proofs[0]); err != nil {
		t.Fatalf("pre-Osaka blob proof verification: %v", err)
	}

	var hash common.Hash
	if err := client.Call(&hash, "eth_sendTransaction", args); err != nil {
		t.Fatal(err)
	}
	if hash == (common.Hash{}) {
		t.Fatal("submitted blob transaction has a zero hash")
	}
	var status map[string]hexutil.Uint
	if err := client.Call(&status, "txpool_status"); err != nil || status["pending"] != 1 {
		t.Fatalf("pre-Osaka blob transaction was not accepted: %#v, %v", status, err)
	}
	if _, err := node.Mine(context.Background(), 1, false); err != nil {
		t.Fatalf("mine pre-Osaka blob transaction: %v", err)
	}
	if cfg.Chain.SlotsPerEpoch < 2 {
		t.Fatal("test requires at least two slots per epoch")
	}
	if _, err := node.Mine(context.Background(), cfg.Chain.SlotsPerEpoch-2, true); err != nil {
		t.Fatalf("advance to Osaka boundary: %v", err)
	}
	args["nonce"] = "0x1"
	if err := client.Call(&raw, "eth_signTransaction", args); err != nil {
		t.Fatal(err)
	}
	if err := transaction.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	sidecar = transaction.BlobTxSidecar()
	if sidecar == nil || sidecar.Version != types.BlobSidecarVersion1 || len(sidecar.Proofs) != kzg4844.CellProofsPerBlob {
		t.Fatalf("Osaka-boundary blob sidecar = %#v", sidecar)
	}
	if err := kzg4844.VerifyCellProofs(sidecar.Blobs, sidecar.Commitments, sidecar.Proofs); err != nil {
		t.Fatalf("Osaka-boundary cell proof verification: %v", err)
	}
	if err := client.Call(&hash, "eth_sendTransaction", args); err != nil {
		t.Fatalf("submit Osaka-boundary blob transaction: %v", err)
	}
}

func TestBeta7FiltersAndTxPoolContentFrom(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	accounts := node.Accounts()

	var blockFilter, pendingFilter, logFilter rpc.ID
	if err := client.Call(&blockFilter, "eth_newBlockFilter"); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&pendingFilter, "eth_newPendingTransactionFilter"); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&logFilter, "eth_newFilter", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	var hashes []common.Hash
	if err := client.Call(&hashes, "eth_getFilterChanges", blockFilter); err != nil || hashes == nil || len(hashes) != 0 {
		t.Fatalf("initial block changes = %#v, %v", hashes, err)
	}
	if err := client.Call(&hashes, "eth_getFilterChanges", pendingFilter); err != nil || hashes == nil || len(hashes) != 0 {
		t.Fatalf("initial pending changes = %#v, %v", hashes, err)
	}
	var logs []*types.Log
	if err := client.Call(&logs, "eth_getFilterChanges", logFilter); err != nil || logs == nil || len(logs) != 0 {
		t.Fatalf("initial log changes = %#v, %v", logs, err)
	}
	var controlBlock common.Hash
	if err := client.Call(&controlBlock, "ethertest_setBalance", accounts[9], "0x1"); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&hashes, "eth_getFilterChanges", blockFilter); err != nil || len(hashes) != 1 || hashes[0] != controlBlock {
		t.Fatalf("control block changes = %#v, %v", hashes, err)
	}
	if err := client.Call(&logs, "eth_getFilterChanges", logFilter); err != nil || logs == nil || len(logs) != 0 {
		t.Fatalf("control block log changes = %#v, %v", logs, err)
	}

	var transactionHash common.Hash
	if err := client.Call(&transactionHash, "eth_sendTransaction", map[string]any{
		"from": accounts[0], "to": accounts[1], "nonce": "0x0", "gas": "0x5208",
		"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00",
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&hashes, "eth_getFilterChanges", pendingFilter); err != nil || len(hashes) != 1 || hashes[0] != transactionHash {
		t.Fatalf("pending changes = %#v, %v", hashes, err)
	}
	if _, err := node.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&hashes, "eth_getFilterChanges", blockFilter); err != nil || len(hashes) != 1 {
		t.Fatalf("block changes = %#v, %v", hashes, err)
	}
	if err := client.Call(&logs, "eth_getFilterLogs", blockFilter); err == nil {
		t.Fatal("getFilterLogs accepted a block filter")
	}
	if ok := false; client.Call(&ok, "eth_uninstallFilter", blockFilter) != nil || !ok {
		t.Fatal("failed to uninstall block filter")
	}
	if ok := true; client.Call(&ok, "eth_uninstallFilter", blockFilter) != nil || ok {
		t.Fatal("second filter uninstall succeeded")
	}

	var pendingHash common.Hash
	if err := client.Call(&pendingHash, "eth_sendTransaction", map[string]any{
		"from": accounts[0], "to": accounts[1], "nonce": "0x2", "gas": "0x5208",
		"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00",
	}); err != nil {
		t.Fatal(err)
	}
	var content map[string]map[string]map[string]any
	if err := client.Call(&content, "txpool_content"); err != nil {
		t.Fatal(err)
	}
	if content["queued"][accounts[0].Hex()]["2"] == nil || content["queued"][accounts[0].Hex()]["0x2"] != nil {
		t.Fatalf("txpool_content nonce keys = %#v", content)
	}
	var fromContent map[string]map[string]any
	if err := client.Call(&fromContent, "txpool_contentFrom", accounts[0]); err != nil {
		t.Fatal(err)
	}
	if fromContent["queued"]["2"] == nil || fromContent["pending"] == nil {
		t.Fatalf("txpool_contentFrom = %#v", fromContent)
	}
}

func TestBeta7FilterReorgReplacementAutoMineAndRingGaps(t *testing.T) {
	t.Run("reorg removed logs precede new canonical logs", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = "manual"
		node := startRPCNode(t, cfg)
		client := node.RPCClient()
		defer client.Close()
		contract := common.HexToAddress("0x2000000000000000000000000000000000000001")
		var controlHash common.Hash
		if err := client.Call(&controlHash, "ethertest_setCode", contract, hexutil.Bytes{0x60, 0x00, 0x60, 0x00, 0xa0, 0x00}); err != nil {
			t.Fatal(err)
		}
		base := node.chain.blockchain.CurrentBlock().Number.Uint64()
		if err := node.CreateBranch(context.Background(), "filter-alt", base); err != nil {
			t.Fatal(err)
		}
		if _, err := node.MineBranch(context.Background(), "filter-alt", 1); err != nil {
			t.Fatal(err)
		}
		var filter rpc.ID
		if err := client.Call(&filter, "eth_newFilter", map[string]any{"address": contract}); err != nil {
			t.Fatal(err)
		}
		account := node.Accounts()[0]
		var firstTx common.Hash
		if err := client.Call(&firstTx, "eth_sendTransaction", map[string]any{
			"from": account, "to": contract, "nonce": "0x0", "gas": "0x186a0",
			"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(context.Background(), 1, false); err != nil {
			t.Fatal(err)
		}
		var logs []*types.Log
		if err := client.Call(&logs, "eth_getFilterChanges", filter); err != nil || len(logs) != 1 || logs[0].Removed {
			t.Fatalf("initial canonical logs = %#v, %v", logs, err)
		}
		firstBlock := logs[0].BlockHash
		if err := node.SwitchBranch(context.Background(), "filter-alt"); err != nil {
			t.Fatal(err)
		}
		var secondTx common.Hash
		if err := client.Call(&secondTx, "eth_sendTransaction", map[string]any{
			"from": account, "to": contract, "nonce": "0x0", "gas": "0x186a0", "input": "0x01",
			"maxFeePerGas": "0xee6b2800", "maxPriorityFeePerGas": "0x77359400",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(context.Background(), 1, false); err != nil {
			t.Fatal(err)
		}
		if err := client.Call(&logs, "eth_getFilterChanges", filter); err != nil {
			t.Fatal(err)
		}
		if len(logs) != 2 || !logs[0].Removed || logs[0].BlockHash != firstBlock || logs[1].Removed || logs[1].BlockHash == firstBlock {
			t.Fatalf("reorg log order = %#v", logs)
		}
	})

	t.Run("pending replacement and transaction automine", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = "manual"
		node := startRPCNode(t, cfg)
		client := node.RPCClient()
		defer client.Close()
		var filter rpc.ID
		if err := client.Call(&filter, "eth_newPendingTransactionFilter"); err != nil {
			t.Fatal(err)
		}
		accounts := node.Accounts()
		var first, replacement common.Hash
		if err := client.Call(&first, "eth_sendTransaction", map[string]any{
			"from": accounts[0], "to": accounts[1], "nonce": "0x0", "gas": "0x5208",
			"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00",
		}); err != nil {
			t.Fatal(err)
		}
		var changes []common.Hash
		if err := client.Call(&changes, "eth_getFilterChanges", filter); err != nil || len(changes) != 1 || changes[0] != first {
			t.Fatalf("first pending change = %#v, %v", changes, err)
		}
		if err := client.Call(&replacement, "eth_sendTransaction", map[string]any{
			"from": accounts[0], "to": accounts[1], "nonce": "0x0", "gas": "0x5208", "value": "0x1",
			"maxFeePerGas": "0xee6b2800", "maxPriorityFeePerGas": "0x77359400",
		}); err != nil {
			t.Fatal(err)
		}
		if replacement == first {
			t.Fatal("replacement hash did not change")
		}
		if err := client.Call(&changes, "eth_getFilterChanges", filter); err != nil || len(changes) != 1 || changes[0] != replacement {
			t.Fatalf("replacement pending change = %#v, %v", changes, err)
		}

		autoNode := startRPCNode(t, testConfig())
		autoClient := autoNode.RPCClient()
		defer autoClient.Close()
		if err := autoClient.Call(&filter, "eth_newPendingTransactionFilter"); err != nil {
			t.Fatal(err)
		}
		autoAccounts := autoNode.Accounts()
		var autoHash common.Hash
		if err := autoClient.Call(&autoHash, "eth_sendTransaction", map[string]any{
			"from": autoAccounts[0], "to": autoAccounts[1], "gas": "0x5208",
		}); err != nil {
			t.Fatal(err)
		}
		if err := autoClient.Call(&changes, "eth_getFilterChanges", filter); err != nil || len(changes) != 1 || changes[0] != autoHash {
			t.Fatalf("automined pending event = %#v, %v", changes, err)
		}
	})

	t.Run("block and pending ring gaps are explicit", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = "manual"
		cfg.Events.Capacity = 2
		node := startRPCNode(t, cfg)
		client := node.RPCClient()
		defer client.Close()
		var blockFilter rpc.ID
		if err := client.Call(&blockFilter, "eth_newBlockFilter"); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(context.Background(), 3, true); err != nil {
			t.Fatal(err)
		}
		var hashes []common.Hash
		assertRPCErrorCode(t, client.Call(&hashes, "eth_getFilterChanges", blockFilter), -32000)
		if err := client.Call(&hashes, "eth_getFilterChanges", blockFilter); err != nil || hashes == nil || len(hashes) != 0 {
			t.Fatalf("block gap cursor was not advanced: %#v, %v", hashes, err)
		}

		var pendingFilter rpc.ID
		if err := client.Call(&pendingFilter, "eth_newPendingTransactionFilter"); err != nil {
			t.Fatal(err)
		}
		accounts := node.Accounts()
		for index := 0; index < 3; index++ {
			var hash common.Hash
			if err := client.Call(&hash, "eth_sendTransaction", map[string]any{
				"from": accounts[index], "to": accounts[9], "nonce": "0x0", "gas": "0x5208",
				"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00",
			}); err != nil {
				t.Fatal(err)
			}
		}
		assertRPCErrorCode(t, client.Call(&hashes, "eth_getFilterChanges", pendingFilter), -32000)
		if err := client.Call(&hashes, "eth_getFilterChanges", pendingFilter); err != nil || hashes == nil || len(hashes) != 0 {
			t.Fatalf("pending gap cursor was not advanced: %#v, %v", hashes, err)
		}
	})
}

func startRPCNode(t *testing.T, cfg Config) *Node {
	t.Helper()
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		_ = node.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	return node
}
