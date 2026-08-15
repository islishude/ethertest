package ethertest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/params"
)

func externalGenesisForTest(t *testing.T, cfg Config, chainID uint64, pragueEpoch, osakaEpoch uint64) *core.Genesis {
	t.Helper()
	cfg.Chain.ChainID = chainID
	cfg.Chain.Forks = ForkConfig{CancunEpoch: 0, PragueEpoch: pragueEpoch, OsakaEpoch: osakaEpoch}
	genesis := core.DeveloperGenesisBlock(31_000_000, nil)
	genesis.Config = executionChainConfig(cfg)
	genesis.Timestamp = uint64(cfg.Chain.GenesisTime)
	genesis.Nonce = 42
	genesis.ExtraData = []byte("ethertest external genesis")
	genesis.Coinbase = common.HexToAddress("0x1000000000000000000000000000000000000001")
	genesis.ParentHash = common.HexToHash("0x1234")
	genesis.GasUsed = 7
	return genesis
}

func writeExternalGenesis(t *testing.T, genesis *core.Genesis) string {
	t.Helper()
	encoded, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cloneExternalGenesis(t *testing.T, genesis *core.Genesis) *core.Genesis {
	t.Helper()
	encoded, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	var cloned core.Genesis
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatal(err)
	}
	return &cloned
}

func assertImportedBeaconJSONSSZ(t *testing.T, node *Node, id, wantVersion string) {
	t.Helper()
	handler := node.beaconHandler()
	path := "/eth/v2/beacon/blocks/" + id
	jsonResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsonResponse, httptest.NewRequest(http.MethodGet, path, nil))
	if jsonResponse.Code != http.StatusOK || jsonResponse.Header().Get("Eth-Consensus-Version") != wantVersion {
		t.Fatalf("Beacon JSON status=%d version=%q body=%s", jsonResponse.Code, jsonResponse.Header().Get("Eth-Consensus-Version"), jsonResponse.Body.String())
	}

	var jsonRoot common.Hash
	switch wantVersion {
	case "deneb":
		var envelope struct {
			Version string                  `json:"version"`
			Data    deneb.SignedBeaconBlock `json:"data"`
		}
		if err := json.NewDecoder(jsonResponse.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Version != wantVersion || envelope.Data.Message == nil {
			t.Fatalf("Beacon JSON envelope = %#v", envelope)
		}
		root, err := envelope.Data.Message.HashTreeRoot()
		if err != nil {
			t.Fatal(err)
		}
		jsonRoot = common.Hash(root)
	default:
		var envelope struct {
			Version string                    `json:"version"`
			Data    electra.SignedBeaconBlock `json:"data"`
		}
		if err := json.NewDecoder(jsonResponse.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Version != wantVersion || envelope.Data.Message == nil {
			t.Fatalf("Beacon JSON envelope = %#v", envelope)
		}
		root, err := envelope.Data.Message.HashTreeRoot()
		if err != nil {
			t.Fatal(err)
		}
		jsonRoot = common.Hash(root)
	}

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Accept", "application/octet-stream")
	sszResponse := httptest.NewRecorder()
	handler.ServeHTTP(sszResponse, request)
	if sszResponse.Code != http.StatusOK || sszResponse.Header().Get("Eth-Consensus-Version") != wantVersion {
		t.Fatalf("Beacon SSZ status=%d version=%q", sszResponse.Code, sszResponse.Header().Get("Eth-Consensus-Version"))
	}
	var sszRoot common.Hash
	if wantVersion == "deneb" {
		var block deneb.SignedBeaconBlock
		if err := block.UnmarshalSSZ(sszResponse.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		root, err := block.Message.HashTreeRoot()
		if err != nil {
			t.Fatal(err)
		}
		sszRoot = common.Hash(root)
	} else {
		var block electra.SignedBeaconBlock
		if err := block.UnmarshalSSZ(sszResponse.Body.Bytes()); err != nil {
			t.Fatal(err)
		}
		root, err := block.Message.HashTreeRoot()
		if err != nil {
			t.Fatal(err)
		}
		sszRoot = common.Hash(root)
	}
	if jsonRoot != sszRoot {
		t.Fatalf("Beacon JSON root %s differs from SSZ root %s", jsonRoot, sszRoot)
	}
}

func TestExternalGenesisPreservesHeaderStateAndChainConfig(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.NetworkID = 0
	genesis := externalGenesisForTest(t, cfg, 4242, 1, 2)
	accounts, err := DeriveAccounts(cfg.Accounts.Mnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	walletBalance := big.NewInt(12_345)
	genesis.Alloc[accounts[0].Address] = types.Account{Balance: walletBalance, Nonce: 7}
	contract := common.HexToAddress("0x2000000000000000000000000000000000000002")
	storageKey := common.HexToHash("0x01")
	storageValue := common.HexToHash("0x02")
	genesis.Alloc[contract] = types.Account{
		Balance: big.NewInt(99), Nonce: 3, Code: common.FromHex("0x6001600055"),
		Storage: map[common.Hash]common.Hash{storageKey: storageValue},
	}
	cfg.Chain.GenesisFile = writeExternalGenesis(t, genesis)

	resolved, err := ResolveConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Chain.ChainID != 4242 || resolved.Chain.NetworkID != 0 || resolved.EffectiveNetworkID() != 4242 ||
		resolved.Chain.GenesisTime != cfg.Chain.GenesisTime || resolved.Chain.GasLimit != genesis.GasLimit ||
		resolved.Chain.Forks != (ForkConfig{CancunEpoch: 0, PragueEpoch: 1, OsakaEpoch: 2}) {
		t.Fatalf("resolved chain configuration = %#v", resolved.Chain)
	}

	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	if !node.chain.externalGenesis || node.cfg.Chain.NetworkID != 4242 || !reflect.DeepEqual(node.chain.config, genesis.Config) {
		t.Fatalf("node did not retain imported configuration: external=%t chain=%#v", node.chain.externalGenesis, node.cfg.Chain)
	}
	wantHash, err := executionGenesisHash(genesis)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.Genesis()
	wantBlock := genesis.ToBlock()
	if block.Hash() != wantHash || block.ParentHash() != genesis.ParentHash || block.GasUsed() != genesis.GasUsed ||
		block.Nonce() != genesis.Nonce || string(block.Extra()) != string(genesis.ExtraData) || block.Root() != wantBlock.Root() {
		t.Fatalf("imported genesis header = %#v, want hash %s", block.Header(), wantHash)
	}
	state, err := node.chain.blockchain.StateAt(block.Header())
	if err != nil {
		t.Fatal(err)
	}
	if state.GetBalance(accounts[0].Address).ToBig().Cmp(walletBalance) != 0 || state.GetNonce(accounts[0].Address) != 7 {
		t.Fatal("configured wallet account was overwritten or funded")
	}
	if state.GetBalance(accounts[1].Address).Sign() != 0 {
		t.Fatal("wallet account absent from alloc was automatically funded")
	}
	if state.GetBalance(contract).ToBig().Cmp(big.NewInt(99)) != 0 || state.GetNonce(contract) != 3 ||
		string(state.GetCode(contract)) != string(genesis.Alloc[contract].Code) || state.GetState(contract, storageKey) != storageValue {
		t.Fatal("custom genesis account state was not preserved")
	}

	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	client := node.RPCClient()
	defer client.Close()
	var chainID hexutil.Uint64
	if err := client.Call(&chainID, "eth_chainId"); err != nil || uint64(chainID) != 4242 {
		t.Fatalf("eth_chainId = %d, %v", chainID, err)
	}
	var networkID string
	if err := client.Call(&networkID, "net_version"); err != nil || networkID != "4242" {
		t.Fatalf("net_version = %q, %v", networkID, err)
	}
}

func TestExternalGenesisAcceptsGethDecimalQuantities(t *testing.T) {
	cfg := testConfig()
	genesis := externalGenesisForTest(t, cfg, 4242, 1, 2)
	address := common.HexToAddress("0x2000000000000000000000000000000000000002")
	genesis.Alloc[address] = types.Account{Balance: big.NewInt(12_345), Nonce: 7}
	encoded, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["nonce"] = genesis.Nonce
	document["timestamp"] = genesis.Timestamp
	document["gasLimit"] = genesis.GasLimit
	document["difficulty"] = 0
	alloc, ok := document["alloc"].(map[string]any)
	if !ok {
		t.Fatalf("encoded alloc = %#v", document["alloc"])
	}
	accountKey := strings.TrimPrefix(strings.ToLower(address.Hex()), "0x")
	account, ok := alloc[accountKey].(map[string]any)
	if !ok {
		t.Fatalf("encoded account %s = %#v", accountKey, alloc[accountKey])
	}
	account["balance"] = 12_345
	account["nonce"] = 7
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadExecutionGenesis(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateExecutionGenesis(cfg, loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Timestamp != genesis.Timestamp || loaded.GasLimit != genesis.GasLimit || loaded.Nonce != genesis.Nonce ||
		loaded.Alloc[address].Balance.Cmp(big.NewInt(12_345)) != 0 || loaded.Alloc[address].Nonce != 7 {
		t.Fatalf("decimal quantities were not decoded with geth semantics: %#v", loaded)
	}
}

func TestExternalGenesisForkBoundariesRemainCrossLayerAligned(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	genesis := externalGenesisForTest(t, cfg, 4242, 1, 2)
	accounts, err := DeriveAccounts(cfg.Accounts.Mnemonic, 1)
	if err != nil {
		t.Fatal(err)
	}
	genesis.Alloc[accounts[0].Address] = types.Account{Balance: new(big.Int).Mul(big.NewInt(100), big.NewInt(params.Ether))}
	cfg.Chain.GenesisFile = writeExternalGenesis(t, genesis)
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	if node.consensus.forkName(0) != "deneb" {
		t.Fatal("imported genesis did not start at Deneb")
	}
	assertImportedBeaconJSONSSZ(t, node, "genesis", "deneb")
	hashes, err := node.Mine(context.Background(), cfg.Chain.SlotsPerEpoch, true)
	if err != nil {
		t.Fatal(err)
	}
	prague := node.chain.blockchain.GetBlockByHash(hashes[cfg.Chain.SlotsPerEpoch-1])
	if node.consensus.forkName(cfg.Chain.SlotsPerEpoch-1) != "deneb" ||
		node.consensus.forkName(cfg.Chain.SlotsPerEpoch) != "electra" {
		t.Fatal("Beacon fork names did not transition at imported execution timestamps")
	}
	if prague.Time() != *genesis.Config.PragueTime || prague.RequestsHash() == nil {
		t.Fatal("execution Prague fields did not transition with the Beacon projection")
	}
	assertImportedBeaconJSONSSZ(t, node, "head", "electra")

	account := testWalletAccount(t, node, 0)
	withdrawalData := append(bytes.Repeat([]byte{0xa1}, 48), make([]byte, 8)...)
	binary.BigEndian.PutUint64(withdrawalData[48:], 1234)
	transaction := signExecutionRequestTransaction(
		t, node.cfg, account, 0, addressPointer(params.WithdrawalQueueAddress), big.NewInt(params.GWei), withdrawalData, 500_000,
	)
	if _, err := node.SendTransaction(context.Background(), transaction); err != nil {
		t.Fatal(err)
	}
	nativeHashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	record, exists, err := loadExecutionRequestRecord(node.chain, nativeHashes[0])
	if err != nil || !exists || len(record.NativeRequests) != 1 ||
		len(record.NativeRequests[0]) == 0 || record.NativeRequests[0][0] != executionRequestWithdrawal {
		t.Fatalf("native request record = %#v exists=%v err=%v", record, exists, err)
	}

	osakaHashes, err := node.Mine(context.Background(), cfg.Chain.SlotsPerEpoch-1, true)
	if err != nil {
		t.Fatal(err)
	}
	osaka := node.chain.blockchain.GetBlockByHash(osakaHashes[len(osakaHashes)-1])
	if node.consensus.forkName(2*cfg.Chain.SlotsPerEpoch) != "fulu" ||
		osaka.Time() != *genesis.Config.OsakaTime || osaka.RequestsHash() == nil {
		t.Fatal("execution Osaka fields did not transition with the Beacon projection")
	}
	assertImportedBeaconJSONSSZ(t, node, "head", "fulu")

	firstBlock := node.chain.blockchain.GetBlockByHash(hashes[0])
	state, err := node.chain.blockchain.StateAt(firstBlock.Header())
	if err != nil {
		t.Fatal(err)
	}
	rootIndex := firstBlock.Time()%8191 + 8191
	if state.GetState(params.BeaconRootsAddress, common.BigToHash(new(big.Int).SetUint64(rootIndex))) == (common.Hash{}) {
		t.Fatal("EIP-4788 system call did not run on imported genesis state")
	}
}

func TestExternalGenesisPebbleRestartAndArchiveDoNotRequireSourceFile(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	genesis := externalGenesisForTest(t, cfg, 9876, 0, 0)
	cfg.Chain.GenesisFile = writeExternalGenesis(t, genesis)

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
	archivePath := filepath.Join(t.TempDir(), "state.tar.zst")
	if err := first.DumpState(archivePath); err != nil {
		t.Fatal(err)
	}
	genesisHash := first.chain.genesisHash
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	restartCfg := testConfig()
	restartCfg.Mining.Mode = miningModeManual
	restartCfg.Storage.Engine = "pebble"
	restartCfg.Storage.Path = cfg.Storage.Path
	restartCfg.Chain.NetworkID = 0
	second, err := New(restartCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !second.chain.externalGenesis || second.chain.genesisHash != genesisHash || second.cfg.Chain.ChainID != 9876 ||
		second.cfg.Chain.NetworkID != 9876 || second.chain.blockchain.CurrentBlock().Hash() != hashes[1] {
		t.Fatalf("restart did not restore imported genesis/head: cfg=%#v", second.cfg.Chain)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	withFile, err := New(cfg)
	if err != nil {
		t.Fatalf("matching source file was rejected: %v", err)
	}
	if err := withFile.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*core.Genesis)
		want   string
	}{
		{"header", func(g *core.Genesis) { g.ExtraData = append(g.ExtraData, 0xff) }, "genesis hash mismatch"},
		{"alloc", func(g *core.Genesis) {
			g.Alloc[common.HexToAddress("0x3000000000000000000000000000000000000003")] = types.Account{Balance: big.NewInt(1)}
		}, "genesis hash mismatch"},
		{"ChainConfig", func(g *core.Genesis) { g.Config.ChainID = big.NewInt(9877) }, "ChainConfig"},
	} {
		t.Run("reject changed "+test.name, func(t *testing.T) {
			changed := cloneExternalGenesis(t, genesis)
			test.mutate(changed)
			mismatchCfg := cfg
			mismatchCfg.Chain.GenesisFile = writeExternalGenesis(t, changed)
			if _, err := New(mismatchCfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("changed %s error = %v", test.name, err)
			}
		})
	}

	destination := filepath.Join(t.TempDir(), "loaded")
	if err := LoadState(archivePath, destination); err != nil {
		t.Fatal(err)
	}
	loadedCfg := restartCfg
	loadedCfg.Storage.Path = destination
	loaded, err := New(loadedCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.Close() //nolint:errcheck
	if !loaded.chain.externalGenesis || loaded.chain.genesisHash != genesisHash ||
		loaded.chain.blockchain.CurrentBlock().Hash() != hashes[1] {
		t.Fatal("archive did not retain imported genesis metadata and head")
	}
}

func TestGeneratedGenesisRestartsWithPreImportTimelineMetadata(t *testing.T) {
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
	hashes, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	kv, err := pebble.New(cfg.Storage.Path, 64, 64, "legacy-genesis-metadata", false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := kv.Get(timelineKey)
	if err != nil {
		t.Fatal(err)
	}
	var timeline map[string]any
	if err := json.Unmarshal(encoded, &timeline); err != nil {
		t.Fatal(err)
	}
	delete(timeline, "genesis_hash")
	delete(timeline, "external_genesis")
	encoded, err = json.Marshal(timeline)
	if err != nil {
		t.Fatal(err)
	}
	if err := kv.Put(timelineKey, encoded); err != nil {
		t.Fatal(err)
	}
	if err := kv.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close() //nolint:errcheck
	if restarted.chain.externalGenesis || restarted.chain.blockchain.CurrentBlock().Hash() != hashes[0] {
		t.Fatal("generated database with pre-import timeline metadata did not follow the legacy recovery path")
	}
}

func TestExternalGenesisValidationRejectsIncompatibleFiles(t *testing.T) {
	cfg := testConfig()
	base := externalGenesisForTest(t, cfg, 4242, 1, 2)
	tests := []struct {
		name   string
		mutate func(*core.Genesis)
		want   string
	}{
		{"zero chain ID", func(g *core.Genesis) { g.Config.ChainID = new(big.Int) }, "chainId must be positive"},
		{"wide chain ID", func(g *core.Genesis) { g.Config.ChainID = new(big.Int).Lsh(big.NewInt(1), 65) }, "exceeds uint64"},
		{"timestamp overflow", func(g *core.Genesis) { g.Timestamp = ^uint64(0) }, "timestamp exceeds int64"},
		{"nonzero number", func(g *core.Genesis) { g.Number = 1 }, "block number must be zero"},
		{"zero gas limit", func(g *core.Genesis) { g.GasLimit = 0 }, "gasLimit must be positive"},
		{"nonzero difficulty", func(g *core.Genesis) { g.Difficulty = big.NewInt(1) }, "difficulty must be zero"},
		{"ethash", func(g *core.Genesis) { g.Config.Ethash = new(params.EthashConfig) }, "ethash and clique"},
		{"clique", func(g *core.Genesis) { g.Config.Clique = new(params.CliqueConfig) }, "ethash and clique"},
		{"missing terminal difficulty", func(g *core.Genesis) { g.Config.TerminalTotalDifficulty = nil }, "terminalTotalDifficulty"},
		{"nonzero terminal difficulty", func(g *core.Genesis) { g.Config.TerminalTotalDifficulty = big.NewInt(1) }, "terminalTotalDifficulty"},
		{"London after genesis", func(g *core.Genesis) { g.Config.LondonBlock = big.NewInt(1) }, "londonBlock"},
		{"Cancun after genesis", func(g *core.Genesis) {
			activation := g.Timestamp + uint64(cfg.Chain.SlotsPerEpoch)*uint64(cfg.Chain.SlotDuration.Seconds())
			g.Config.CancunTime, g.Config.PragueTime, g.Config.OsakaTime = &activation, &activation, &activation
		}, "cancun must be active"},
		{"unaligned Prague", func(g *core.Genesis) { activation := g.Timestamp + 1; g.Config.PragueTime = &activation }, "not aligned"},
		{"missing Prague", func(g *core.Genesis) { g.Config.PragueTime = nil }, "activation times are required"},
		{"missing Osaka", func(g *core.Genesis) { g.Config.OsakaTime = nil }, "activation times are required"},
		{"post Osaka fork", func(g *core.Genesis) {
			activation := *g.Config.OsakaTime
			g.Config.AmsterdamTime = &activation
		}, "post-Osaka"},
		{"blob schedule", func(g *core.Genesis) { g.Config.BlobScheduleConfig.Prague.Max++ }, "blobSchedule"},
		{"missing system contract", func(g *core.Genesis) { delete(g.Alloc, params.BeaconRootsAddress) }, "missing EIP-4788"},
		{"modified system contract", func(g *core.Genesis) {
			account := g.Alloc[params.WithdrawalQueueAddress]
			account.Code = []byte{0}
			g.Alloc[params.WithdrawalQueueAddress] = account
		}, "invalid EIP-7002"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			genesis := cloneExternalGenesis(t, base)
			test.mutate(genesis)
			candidate := cfg
			candidate.Chain.GenesisFile = writeExternalGenesis(t, genesis)
			if _, err := ResolveConfig(candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("malformed and trailing JSON", func(t *testing.T) {
		valid, err := json.Marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		for _, contents := range []string{"{", "{}", string(valid) + " {}", string(valid) + " trailing"} {
			path := filepath.Join(t.TempDir(), "genesis.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			candidate := cfg
			candidate.Chain.GenesisFile = path
			if _, err := ResolveConfig(candidate); err == nil {
				t.Fatalf("invalid JSON %q was accepted", contents)
			}
		}
	})
}
