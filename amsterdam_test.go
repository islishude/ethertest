package ethertest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	dynssz "github.com/pk910/dynamic-ssz"
	bls "github.com/protolambda/bls12-381-util"
	"math"
	"math/big"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAmsterdamDefaultSmoke(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = false
	cfg.Beacon.Enabled = false
	cfg.Mining.Mode = "manual"
	cfg.Chain.GenesisTime = 1000
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	defer n.Close() //nolint:errcheck
	if _, err := n.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
	b := n.chain.blockchain.GetBlockByNumber(1)
	if b.Header().SlotNumber == nil || *b.Header().SlotNumber != 1 {
		t.Fatal("missing slot")
	}
	if _, err := n.consensus.signedBlock(n.chain, b); err != nil {
		t.Fatal(err)
	}
}

func amsterdamTestConfig() Config {
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = false
	cfg.Beacon.Enabled = false
	cfg.Mining.Mode = "manual"
	cfg.Chain.GenesisTime = 1800000000
	return cfg
}

func assertAmsterdamReplay(t *testing.T, n *Node, b *types.Block) {
	t.Helper()
	parent := n.chain.blockchain.GetBlockByHash(b.ParentHash())
	state, err := n.chain.blockchain.StateAt(parent.Header())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := blockAccessListBytes(n.chain, b)
	if err != nil {
		t.Fatal(err)
	}
	var list bal.BlockAccessList
	if err := rlp.DecodeBytes(raw, &list); err != nil {
		t.Fatal(err)
	}
	if err := list.Validate(b.GasLimit(), len(b.Transactions())); err != nil {
		t.Fatal(err)
	}
	// Force both upstream sequential and BAL-driven parallel validation.
	for _, parallel := range []bool{false, true} {
		state = state.Copy()
		result, err := n.chain.blockchain.Processor().Process(t.Context(), b.WithAccessListUnsafe(&list), state, nil, nil, vm.Config{DisableParallelExecution: !parallel}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := n.chain.blockchain.Validator().ValidateState(b, state, result, false); err != nil {
			t.Fatalf("replay parallel=%v: %v", parallel, err)
		}
		state, err = n.chain.blockchain.StateAt(parent.Header())
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAmsterdamBoundariesAndSlotnum(t *testing.T) {
	for _, activation := range []int64{-1, 0, 1} {
		t.Run(strconv.FormatInt(activation, 10), func(t *testing.T) {
			cfg := amsterdamTestConfig()
			cfg.Chain.Forks.AmsterdamEpoch = activation
			n := startRPCNode(t, cfg)
			if _, err := n.Mine(t.Context(), 7, true); err != nil {
				t.Fatal(err)
			}
			if _, err := n.MissSlots(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
			if _, err := n.Mine(t.Context(), 1, true); err != nil {
				t.Fatal(err)
			}
			for _, number := range []uint64{0, 7, 8} {
				b := n.chain.blockchain.GetBlockByNumber(number)
				slot := n.chain.slotOf(b)
				active := activation >= 0 && slot >= uint64(activation)*8
				if (b.Header().SlotNumber != nil) != active || (b.Header().BlockAccessListHash != nil) != active {
					t.Fatalf("incorrect fork at slot %d", slot)
				}
				projection, err := n.consensus.signedBlock(n.chain, b)
				if err != nil {
					t.Fatal(err)
				}
				if (projection.gloas != nil) != active {
					t.Fatal("EL/CL fork mismatch")
				}
				if active {
					if *b.Header().SlotNumber != slot {
						t.Fatal("slotNumber used block number")
					}
					if number > 0 {
						assertAmsterdamReplay(t, n, b)
					}
				}
			}
			if activation >= 0 {
				code := hexutil.Bytes{0x4b, 0x60, 0, 0x52, 0x60, 0x20, 0x60, 0, 0xf3}
				address := common.HexToAddress("0x1234")
				var result hexutil.Bytes
				if err := n.RPCClient().Call(&result, "eth_call", map[string]any{"to": address}, "latest", map[common.Address]any{address: map[string]any{"code": code}}); err != nil {
					t.Fatal(err)
				}
				if new(big.Int).SetBytes(result).Uint64() != 10 {
					t.Fatalf("SLOTNUM = %x", result)
				}
			}
		})
	}
}

func TestAmsterdamTransferBALAndBeacon(t *testing.T) {
	n := startRPCNode(t, amsterdamTestConfig())
	client := n.RPCClient()
	defer client.Close()
	accounts := n.Accounts()
	args := map[string]any{"from": accounts[0], "to": accounts[1], "value": "0x0"}
	var gas hexutil.Uint64
	if err := client.Call(&gas, "eth_estimateGas", args); err != nil {
		t.Fatal(err)
	}
	if gas == 21000 || gas == 0 {
		t.Fatalf("Amsterdam estimate = %d", gas)
	}
	args["gas"] = gas
	var hash common.Hash
	if err := client.Call(&hash, "eth_sendTransaction", args); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Mine(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	b := n.chain.blockchain.GetBlockByNumber(1)
	if len(b.Transactions()) != 1 {
		t.Fatal("transfer omitted")
	}
	assertAmsterdamReplay(t, n, b)
	for _, selector := range []any{"latest", "pending", "earliest", b.Hash()} {
		var raw hexutil.Bytes
		var decoded []map[string]any
		if err := client.Call(&raw, "debug_getRawBlockAccessList", selector); err != nil {
			t.Fatal(err)
		}
		if len(raw) == 0 {
			t.Fatal("missing RLP")
		}
		if err := client.Call(&decoded, "eth_getBlockAccessList", selector); err != nil {
			t.Fatal(err)
		}
		if decoded == nil {
			t.Fatal("BAL returned null")
		}
	}
	for _, endpoint := range []string{"/eth/v2/beacon/blocks/head", "/eth/v1/beacon/execution_payload_envelopes/head"} {
		handler := n.beaconHandler()
		j := httptest.NewRecorder()
		handler.ServeHTTP(j, httptest.NewRequest("GET", endpoint, nil))
		if j.Code != 200 || j.Header().Get("Eth-Consensus-Version") != "gloas" {
			t.Fatalf("%s: %d %s", endpoint, j.Code, j.Body.String())
		}
		var response struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(j.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("GET", endpoint, nil)
		req.Header.Set("Accept", "application/octet-stream")
		s := httptest.NewRecorder()
		handler.ServeHTTP(s, req)
		var value any
		if strings.Contains(endpoint, "envelopes") {
			value = new(gloasSignedPayloadEnvelope)
		} else {
			value = new(gloasSignedBeaconBlock)
		}
		if err := json.Unmarshal(response.Data, value); err != nil {
			t.Fatal(err)
		}
		encoded, err := gloasSSZ.MarshalSSZ(value)
		if err != nil {
			t.Fatal(err)
		}
		if s.Code != 200 || !bytes.Equal(encoded, s.Body.Bytes()) {
			t.Fatalf("JSON/SSZ mismatch %s: %s", endpoint, s.Body.String())
		}
	}
}

func TestAmsterdamPersistenceArchiveAndCorruption(t *testing.T) {
	cfg := amsterdamTestConfig()
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "db")
	n := startRPCNode(t, cfg)
	if _, err := n.Mine(t.Context(), 2, true); err != nil {
		t.Fatal(err)
	}
	b := n.chain.blockchain.GetBlockByNumber(2)
	archive := filepath.Join(t.TempDir(), "state.tar.zst")
	if err := n.DumpState(archive); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n = startRPCNode(t, cfg)
	if n.chain.blockchain.CurrentBlock().Hash() != b.Hash() {
		t.Fatal("restart head changed")
	}
	assertAmsterdamReplay(t, n, n.chain.blockchain.GetBlockByNumber(2))
	destination := filepath.Join(t.TempDir(), "loaded")
	if err := LoadState(archive, destination); err != nil {
		t.Fatal(err)
	}
	loaded := cfg
	loaded.Storage.Path = destination
	restored := startRPCNode(t, loaded)
	if restored.chain.blockchain.CurrentBlock().Hash() != b.Hash() {
		t.Fatal("archive head changed")
	}
	projection, record, _, err := loadProjection(n.chain, b.Hash())
	if err != nil {
		t.Fatal(err)
	}
	projection.envelope.Message.Payload.SlotNumber++
	record.EnvelopeSSZ, err = gloasSSZ.MarshalSSZ(projection.envelope)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.chain.db.Put(projectionKey(b.Hash()), encoded); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if broken, err := New(cfg); err == nil {
		_ = broken.Close()
		t.Fatal("corrupt envelope accepted")
	}
}

func TestAmsterdamConfiguration(t *testing.T) {
	for _, epoch := range []int64{-2, -1, 0, 1, math.MaxInt64} {
		cfg := amsterdamTestConfig()
		cfg.Chain.Forks.AmsterdamEpoch = epoch
		err := cfg.Validate()
		valid := epoch >= -1 && epoch != math.MaxInt64
		if (err == nil) != valid {
			t.Fatalf("epoch %d: %v", epoch, err)
		}
	}
	cfg := amsterdamTestConfig()
	cfg.Chain.Forks.OsakaEpoch = 2
	cfg.Chain.Forks.AmsterdamEpoch = 1
	if cfg.Validate() == nil {
		t.Fatal("reversed fork schedule accepted")
	}
	t.Setenv("ETHERTEST_AMSTERDAM_EPOCH", "-1")
	read, err := ReadConfig("")
	if err != nil || read.Chain.Forks.AmsterdamEpoch != -1 {
		t.Fatalf("env: %v", err)
	}
}

func TestAmsterdamNativeBuilderRequests(t *testing.T) {
	n := startRPCNode(t, amsterdamTestConfig())
	client := n.RPCClient()
	defer client.Close()
	account := n.Accounts()[0]
	deposit := make([]byte, 184)
	deposit[0] = 42
	deposit[48] = 0xb0
	binary.BigEndian.PutUint64(deposit[80:88], 1_000_000_000)
	deposit[88] = 0xc0
	exit := make([]byte, 48)
	exit[0] = 42
	for _, call := range []struct {
		to    common.Address
		data  []byte
		value string
	}{
		{params.BuilderDepositAddress, deposit, "0xde0b6b3a7640001"},
		{params.BuilderExitAddress, exit, "0x1"},
	} {
		var hash common.Hash
		if err := client.Call(&hash, "eth_sendTransaction", map[string]any{"from": account, "to": call.to, "data": hexutil.Bytes(call.data), "value": call.value, "gas": "0x1e8480"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.Mine(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	b := n.chain.blockchain.GetBlockByNumber(1)
	if len(b.Transactions()) != 2 {
		t.Fatalf("transactions=%d", len(b.Transactions()))
	}
	record, exists, err := loadExecutionRequestRecord(n.chain, b.Hash())
	if err != nil || !exists {
		t.Fatalf("record: %v", err)
	}
	requests, err := parseExecutionRequests(record.Requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests.BuilderDeposits) != 1 || len(requests.BuilderExits) != 1 {
		t.Fatalf("builder requests missing: deposits=%d exits=%d, receipts=%+v", len(requests.BuilderDeposits), len(requests.BuilderExits), n.chain.blockchain.GetReceiptsByHash(b.Hash()))
	}
	assertAmsterdamReplay(t, n, b)
	if _, err := n.Mine(t.Context(), 1, true); err != nil {
		t.Fatal(err)
	}
	signed, err := n.consensus.signedBlock(n.chain, n.chain.blockchain.GetBlockByNumber(2))
	if err != nil {
		t.Fatal(err)
	}
	parentBytes, err := marshalExecutionRequests(signed.gloas.Message.Body.ParentExecutionRequests)
	if err != nil {
		t.Fatal(err)
	}
	if !equalExecutionRequestBytes(parentBytes, record.Requests) {
		t.Fatal("parent request linkage mismatch")
	}
}

func TestAmsterdamBlobColumns(t *testing.T) {
	cfg := amsterdamTestConfig()
	n := startRPCNode(t, cfg)
	blob, err := EncodePackedBytesV1([]byte("Gloas columns"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := SignBlobTransaction(BlobTransactionRequest{ChainID: new(big.Int).SetUint64(DefaultChainID), To: n.Accounts()[1], Gas: 100000, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(3e9), BlobFeeCap: big.NewInt(1e9), Blob: blob}, testWalletAccount(t, n, 0).PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	bad := tx.BlobTxSidecar().Copy()
	bad.Proofs[0][0] ^= 1
	if _, err := n.SendTransaction(t.Context(), tx.WithBlobTxSidecar(bad)); err == nil {
		t.Fatal("bad KZG accepted")
	}
	if _, err := n.SendTransaction(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Mine(t.Context(), 1, false); err != nil {
		t.Fatal(err)
	}
	b := n.chain.blockchain.GetBlockByNumber(1)
	assertAmsterdamReplay(t, n, b)
	handler := n.beaconHandler()
	path := "/eth/v1/debug/beacon/data_column_sidecars/head?indices=0"
	j := httptest.NewRecorder()
	handler.ServeHTTP(j, httptest.NewRequest("GET", path, nil))
	if j.Code != 200 {
		t.Fatalf("columns: %s", j.Body.String())
	}
	var response struct {
		Data []struct {
			Index     string           `json:"index"`
			Slot      string           `json:"slot"`
			Column    []hexutil.Bytes  `json:"column"`
			Proofs    []deneb.KZGProof `json:"kzg_proofs"`
			Root      phase0.Root      `json:"beacon_block_root"`
			OldHeader json.RawMessage  `json:"signed_block_header"`
		} `json:"data"`
	}
	if err := json.Unmarshal(j.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].Slot != "1" || len(response.Data[0].OldHeader) != 0 {
		t.Fatal("wrong Gloas data column JSON")
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Accept", "application/octet-stream")
	s := httptest.NewRecorder()
	handler.ServeHTTP(s, req)
	if s.Code != 200 {
		t.Fatalf("column SSZ: %s", s.Body.String())
	}
	var columns []*gloasDataColumn
	// The REST result is an SSZ List[DataColumnSidecar, 128].
	codec := dynssz.NewDynSsz(nil)
	var container struct {
		Columns []*gloasDataColumn `ssz-max:"128"`
	}
	wrapped := append([]byte{4, 0, 0, 0}, s.Body.Bytes()...)
	if err := codec.UnmarshalSSZ(&container, wrapped); err != nil {
		t.Fatal(err)
	}
	columns = container.Columns
	if len(columns) != 1 || columns[0].BeaconBlockRoot != response.Data[0].Root || !bytes.Equal(columns[0].Column[0][:], response.Data[0].Column[0]) {
		t.Fatal("column JSON/SSZ mismatch")
	}
}

func TestAmsterdamExactBoundaryAndImport(t *testing.T) {
	for _, activation := range []int64{0, 1, -1} {
		t.Run(strconv.FormatInt(activation, 10), func(t *testing.T) {
			cfg := amsterdamTestConfig()
			cfg.Chain.Forks.AmsterdamEpoch = activation
			genesis := externalGenesisForTest(t, cfg, cfg.Chain.ChainID, 0, 0)
			genesis.Config = executionChainConfig(cfg)
			cfg.Chain.GenesisFile = writeExternalGenesis(t, genesis)
			n := startRPCNode(t, cfg)
			if activation == 1 {
				config, err := (&ethAPI{node: n}).Config(t.Context())
				if err != nil || config.Next == nil || config.Next.ActivationTime != uint64(cfg.Chain.GenesisTime)+48 {
					t.Fatalf("Amsterdam next config: %#v %v", config, err)
				}
			}
			if _, err := n.Mine(t.Context(), 8, true); err != nil {
				t.Fatal(err)
			}
			for _, number := range []uint64{0, 7, 8} {
				block := n.chain.blockchain.GetBlockByNumber(number)
				active := activation >= 0 && number >= uint64(activation)*8
				if (block.Header().SlotNumber != nil) != active {
					t.Fatalf("slot %d activation %d", number, activation)
				}
				projection, err := n.consensus.signedBlock(n.chain, block)
				if err != nil || (projection.gloas != nil) != active {
					t.Fatalf("projection %v", err)
				}
				if active && number > 0 {
					assertAmsterdamReplay(t, n, block)
				}
			}
			if activation == 1 {
				if n.cfg.Chain.Forks.AmsterdamEpoch != 1 {
					t.Fatal("import schedule changed")
				}
			}
		})
	}
}

func TestAmsterdamControlBALAndTrace(t *testing.T) {
	n := startRPCNode(t, amsterdamTestConfig())
	address := common.HexToAddress("0x1234")
	balance := big.NewInt(1234567)
	nonce := uint64(3)
	code := []byte{0x4b, 0x60, 0, 0x52, 0x60, 0x20, 0x60, 0, 0xf3}
	storage := map[common.Hash]common.Hash{common.HexToHash("0x1"): common.HexToHash("0x2")}
	hash, err := n.ApplyControl(t.Context(), ControlChanges{address: {Balance: balance, Nonce: &nonce, Code: &code, StorageDiff: &storage}})
	if err != nil {
		t.Fatal(err)
	}
	b := n.chain.blockchain.GetBlockByHash(hash)
	raw, err := blockAccessListBytes(n.chain, b)
	if err != nil {
		t.Fatal(err)
	}
	var list bal.BlockAccessList
	if err := rlp.DecodeBytes(raw, &list); err != nil {
		t.Fatal(err)
	}
	if err := list.Validate(b.GasLimit(), 0); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, account := range list {
		if account.Address != address {
			continue
		}
		found = true
		if len(account.BalanceChanges) != 1 || account.BalanceChanges[0].PostBalance.ToBig().Cmp(balance) != 0 || len(account.NonceChanges) != 1 || account.NonceChanges[0].PostNonce != nonce || len(account.CodeChanges) != 1 || !bytes.Equal(account.CodeChanges[0].NewCode, code) || len(account.StorageChanges) != 1 {
			t.Fatalf("incomplete override BAL: %#v", account)
		}
	}
	if !found {
		t.Fatal("override missing from BAL")
	}
	var traced struct {
		ReturnValue string `json:"returnValue"`
	}
	if err := n.RPCClient().Call(&traced, "debug_traceCall", map[string]any{"to": address}, "latest"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(traced.ReturnValue, "01") {
		t.Fatalf("SLOTNUM trace %s", traced.ReturnValue)
	}
	if _, err := n.Mine(t.Context(), 1, true); err != nil {
		t.Fatal(err)
	}
	assertAmsterdamReplay(t, n, n.chain.blockchain.GetBlockByNumber(2))
}

func TestGloasSignaturesAndEvents(t *testing.T) {
	n := startRPCNode(t, amsterdamTestConfig())
	if _, err := n.Mine(t.Context(), 1, true); err != nil {
		t.Fatal(err)
	}
	block := n.chain.blockchain.GetBlockByNumber(1)
	projection, err := n.consensus.signedBlock(n.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		object    any
		signature phase0.BLSSignature
		domain    phase0.DomainType
	}{
		{projection.gloas.Message, projection.gloas.Signature, phase0.DomainType{}},
		{projection.envelope.Message, projection.envelope.Signature, phase0.DomainType{0x0b}},
	} {
		root, err := gloasSSZ.HashTreeRoot(item.object)
		if err != nil {
			t.Fatal(err)
		}
		domain, err := n.consensus.domain(item.domain, phase0.Version{7})
		if err != nil {
			t.Fatal(err)
		}
		signingRoot, err := (&phase0.SigningData{ObjectRoot: root, Domain: domain}).HashTreeRoot()
		if err != nil {
			t.Fatal(err)
		}
		pubkey := new(bls.Pubkey)
		if err := pubkey.Deserialize(&n.consensus.pubkeys[projection.gloas.Message.ProposerIndex]); err != nil {
			t.Fatal(err)
		}
		signature := new(bls.Signature)
		encoded := [96]byte(item.signature)
		if err := signature.Deserialize(&encoded); err != nil {
			t.Fatal(err)
		}
		if !bls.Verify(pubkey, signingRoot[:], signature) {
			t.Fatal("invalid Gloas signature")
		}
	}
	events, err := n.EventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	seenHead, seenPayload := false, false
	for _, event := range events {
		for _, message := range n.beaconEventMessages(event, map[string]bool{"head": true, "head_v2": true, "execution_payload_available": true}) {
			data := message.data.(map[string]any)
			if message.topic == "head" && data["version"] != nil {
				t.Fatal("legacy head envelope changed")
			}
			if message.topic == "head_v2" {
				seenHead = true
				if data["version"] != "gloas" || data["data"].(map[string]any)["payload_status"] != "full" {
					t.Fatal("invalid Gloas head event")
				}
			}
			if message.topic == "execution_payload_available" {
				seenPayload = true
				if data["slot"] != "1" {
					t.Fatal("invalid payload event")
				}
			}
		}
	}
	if !seenHead || !seenPayload {
		t.Fatal("missing Gloas events")
	}
}

func TestAmsterdamRejectsMissingAndCorruptBAL(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(strconv.FormatBool(corrupt), func(t *testing.T) {
			cfg := amsterdamTestConfig()
			cfg.Storage.Engine = "pebble"
			cfg.Storage.Path = filepath.Join(t.TempDir(), "db")
			n := startRPCNode(t, cfg)
			if _, err := n.Mine(t.Context(), 1, true); err != nil {
				t.Fatal(err)
			}
			block := n.chain.blockchain.GetBlockByNumber(1)
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			db := openTestPebbleDatabase(t, cfg.Storage.Path)
			if corrupt {
				empty := bal.BlockAccessList{}
				rawdb.WriteAccessList(db, block.Hash(), block.NumberU64(), &empty)
			} else {
				rawdb.DeleteAccessList(db, block.Hash(), block.NumberU64())
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if restored, err := New(cfg); err == nil {
				_ = restored.Close()
				t.Fatal("missing/corrupt BAL accepted")
			}
		})
	}
}

func TestAmsterdamGenesisRejectsInvalidSystemContracts(t *testing.T) {
	for _, address := range []common.Address{params.BuilderDepositAddress, params.BuilderExitAddress, params.DeterministicFactoryAddress} {
		t.Run(address.Hex(), func(t *testing.T) {
			cfg := amsterdamTestConfig()
			genesis := externalGenesisForTest(t, cfg, cfg.Chain.ChainID, 0, 0)
			genesis.Config = executionChainConfig(cfg)
			delete(genesis.Alloc, address)
			cfg.Chain.GenesisFile = writeExternalGenesis(t, genesis)
			if node, err := New(cfg); err == nil {
				_ = node.Close()
				t.Fatal("missing system contract accepted")
			}
		})
	}
}

func TestAmsterdamSimulationSlotAndBAL(t *testing.T) {
	n := startRPCNode(t, amsterdamTestConfig())
	if _, err := n.MissSlots(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Mine(t.Context(), 1, true); err != nil {
		t.Fatal(err)
	}
	address := common.HexToAddress("0x1234")
	payload := map[string]any{"blockStateCalls": []any{map[string]any{
		"stateOverrides": map[common.Address]any{address: map[string]any{"code": "0x4b60005260206000f3"}},
		"calls":          []any{map[string]any{"to": address}},
	}}}
	var result []struct {
		SlotNumber          hexutil.Uint64 `json:"slotNumber"`
		BlockAccessListHash common.Hash    `json:"blockAccessListHash"`
		Calls               []struct {
			ReturnData hexutil.Bytes `json:"returnData"`
		} `json:"calls"`
	}
	if err := n.RPCClient().Call(&result, "eth_simulateV1", payload, "latest"); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].SlotNumber != 5 || result[0].BlockAccessListHash == (common.Hash{}) || len(result[0].Calls) != 1 || new(big.Int).SetBytes(result[0].Calls[0].ReturnData).Uint64() != 5 {
		t.Fatalf("simulation result: %#v", result)
	}
	if n.chain.blockchain.CurrentBlock().Number.Uint64() != 1 {
		t.Fatal("simulation persisted")
	}
}

func TestAmsterdamFullStorageReplacementBAL(t *testing.T) {
	cfg := amsterdamTestConfig()
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "db")
	n := startRPCNode(t, cfg)
	address := common.HexToAddress("0x1234")
	oldKey, newKey := common.HexToHash("0x1"), common.HexToHash("0x2")
	oldStorage := map[common.Hash]common.Hash{oldKey: common.HexToHash("0xff")}
	if _, err := n.ApplyControl(t.Context(), ControlChanges{address: {StorageDiff: &oldStorage, Nonce: new(uint64(1))}}); err != nil {
		t.Fatal(err)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	n = startRPCNode(t, cfg)
	replacement := map[common.Hash]common.Hash{newKey: common.HexToHash("0xab")}
	hash, err := n.ApplyControl(t.Context(), ControlChanges{address: {Storage: &replacement}})
	if err != nil {
		t.Fatal(err)
	}
	block := n.chain.blockchain.GetBlockByHash(hash)
	raw, err := blockAccessListBytes(n.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	var list bal.BlockAccessList
	if err := rlp.DecodeBytes(raw, &list); err != nil {
		t.Fatal(err)
	}
	changes := constructionAccessList(&list).Accounts[address]
	if changes == nil || len(changes.StorageWrites) != 2 || changes.StorageWrites[oldKey][1] != (common.Hash{}) || changes.StorageWrites[newKey][1] != replacement[newKey] {
		t.Fatalf("incomplete storage reset BAL: %#v", changes)
	}
	if valid, err := n.VerifyControlRecord(t.Context(), hash); err != nil || !valid {
		t.Fatalf("control verification: %v %v", valid, err)
	}
	state, err := n.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if state.GetState(address, oldKey) != (common.Hash{}) || state.GetState(address, newKey) != replacement[newKey] {
		t.Fatal("storage not replaced")
	}
}

func TestAmsterdamBuilderRequestValidation(t *testing.T) {
	deposit := append([]byte{3}, make([]byte, 184)...)
	exit := append([]byte{4}, make([]byte, 68)...)
	valid := [][]byte{deposit, exit}
	hash := types.CalcRequestsHash(valid)
	before := types.NewBlockWithHeader(&types.Header{RequestsHash: &hash, Number: big.NewInt(1)})
	if err := verifyExecutionRequestsHash(before, valid); err == nil {
		t.Fatal("builder requests accepted before Amsterdam")
	}
	after := before.Header()
	after.SlotNumber = new(uint64(1))
	if err := verifyExecutionRequestsHash(types.NewBlockWithHeader(after), valid); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][][]byte{
		{deposit[:184]}, {exit[:68]}, {exit, deposit}, {deposit, deposit}, {append([]byte{3}, make([]byte, 184*65)...)}, {append([]byte{4}, make([]byte, 68*17)...)},
	} {
		if _, err := parseExecutionRequests(invalid); err == nil {
			t.Fatal("invalid builder request accepted")
		}
	}
	wrong := common.Hash{}
	after.RequestsHash = &wrong
	if err := verifyExecutionRequestsHash(types.NewBlockWithHeader(after), valid); err == nil {
		t.Fatal("incorrect request hash accepted")
	}
}
