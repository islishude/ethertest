package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

func signedDynamicTransaction(t *testing.T, cfg Config, account Account, nonce uint64, to common.Address, value *big.Int, data []byte) *types.Transaction {
	t.Helper()
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: new(big.Int).SetUint64(cfg.Chain.ChainID), Nonce: nonce,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		Gas: 150_000, To: &to, Value: new(big.Int).Set(value), Data: append([]byte(nil), data...),
	}), types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.Chain.ChainID)), account.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func TestExecutionBlocksReferencePersistedParentBeaconProjection(t *testing.T) {
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

	genesis := node.chain.blockchain.Genesis()
	projection, err := node.consensus.ensureProjection(node.chain, genesis)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.MissSlots(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	hashes, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	if block.BeaconRoot() == nil || *block.BeaconRoot() != common.Hash(projection.Root) {
		t.Fatalf("parent beacon root = %v, want %s", block.BeaconRoot(), common.Hash(projection.Root))
	}
	if *block.BeaconRoot() == genesis.Hash() {
		t.Fatal("execution block incorrectly references the parent execution hash")
	}
	state, err := node.chain.blockchain.StateAt(block.Header())
	if err != nil {
		t.Fatal(err)
	}
	rootIndex := block.Time()%8191 + 8191
	stored := state.GetState(params.BeaconRootsAddress, common.BigToHash(new(big.Int).SetUint64(rootIndex)))
	if stored != common.Hash(projection.Root) {
		t.Fatalf("Beacon roots contract stored %s, want %s", stored, common.Hash(projection.Root))
	}
	replayState, err := node.chain.blockchain.StateAt(genesis.Header())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.NewStateProcessor(node.chain.blockchain).Process(
		context.Background(), block, replayState, nil, vm.Config{}, nil,
	); err != nil {
		t.Fatal(err)
	}
	if replayed := replayState.IntermediateRoot(true); replayed != block.Root() {
		t.Fatalf("clean execution replay root = %s, want %s", replayed, block.Root())
	}

	if err := node.CreateBranch(context.Background(), "side", block.NumberU64()); err != nil {
		t.Fatal(err)
	}
	branchHashes, err := node.MineBranch(context.Background(), "side", 1)
	if err != nil {
		t.Fatal(err)
	}
	branchBlock := node.chain.blockchain.GetBlockByHash(branchHashes[0])
	parentProjection, err := node.consensus.ensureProjection(node.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	if branchBlock.BeaconRoot() == nil || *branchBlock.BeaconRoot() != common.Hash(parentProjection.Root) {
		t.Fatalf("branch parent beacon root = %v, want %s", branchBlock.BeaconRoot(), common.Hash(parentProjection.Root))
	}
}

func TestControlLineageAndArchiveSafetyArePermanent(t *testing.T) {
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

	if err := node.CreateBranch(context.Background(), "clean", 0); err != nil {
		t.Fatal(err)
	}
	cleanHashes, err := node.MineBranch(context.Background(), "clean", 1)
	if err != nil {
		t.Fatal(err)
	}
	address := node.Accounts()[0]
	balance := big.NewInt(123)
	controlHash, err := node.ApplyControl(context.Background(), ControlChanges{address: {Balance: balance}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := node.VerifyControlRecord(context.Background(), controlHash); err != nil || !ok {
		t.Fatalf("VerifyControlRecord = %v, %v", ok, err)
	}
	descendants, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, hash := range []common.Hash{controlHash, descendants[0]} {
		safety, err := node.BlockSafety(hash)
		if err != nil || !safety.Tainted || safety.FirstUnsafeBlock == nil || *safety.FirstUnsafeBlock != controlHash {
			t.Fatalf("block safety for %s = %#v, %v", hash, safety, err)
		}
	}
	cleanSafety, err := node.BlockSafety(cleanHashes[0])
	if err != nil || cleanSafety.Tainted {
		t.Fatalf("clean branch safety = %#v, %v", cleanSafety, err)
	}

	control := node.chain.blockchain.GetBlockByHash(controlHash)
	parent := node.chain.blockchain.GetBlockByHash(control.ParentHash())
	parentState, err := node.chain.blockchain.StateAt(parent.Header())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.NewStateProcessor(node.chain.blockchain).Process(
		context.Background(), control, parentState, nil, vm.Config{}, nil,
	); err != nil {
		t.Fatal(err)
	}
	if replayed := parentState.IntermediateRoot(true); replayed == control.Root() {
		t.Fatal("unsafe control block unexpectedly matches a standard state transition")
	}

	if err := node.Checkpoint(context.Background(), "unsafe"); err != nil {
		t.Fatal(err)
	}
	if point := node.checkpoints["unsafe"]; point == nil || !point.tainted || point.slot != 2 {
		t.Fatalf("unsafe checkpoint metadata = %#v", point)
	}
	if err := node.SwitchBranch(context.Background(), "clean"); err != nil {
		t.Fatal(err)
	}
	status := node.SafetyStatus()
	if !status.SessionTainted || status.HeadTainted || status.FirstUnsafeBlock == nil || *status.FirstUnsafeBlock != controlHash || status.ConsensusMode != "synthetic" {
		t.Fatalf("safety after clean reorg = %#v", status)
	}
	client := node.RPCClient()
	defer client.Close()
	var rpcStatus SafetyStatus
	if err := client.Call(&rpcStatus, "ethertest_safetyStatus"); err != nil || !rpcStatus.SessionTainted || rpcStatus.HeadTainted {
		t.Fatalf("ethertest_safetyStatus = %#v, %v", rpcStatus, err)
	}
	var rpcBlockSafety BlockSafety
	if err := client.Call(&rpcBlockSafety, "ethertest_blockSafety", controlHash); err != nil || !rpcBlockSafety.Tainted {
		t.Fatalf("ethertest_blockSafety = %#v, %v", rpcBlockSafety, err)
	}
	archive := filepath.Join(t.TempDir(), "state.tar.zst")
	if err := node.DumpState(archive); err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectState(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Tainted || manifest.HeadTainted || !manifest.TimelineComplete || manifest.ConsensusMode != "synthetic" || len(manifest.TaintReasons) == 0 {
		t.Fatalf("unsafe archive manifest = %#v", manifest)
	}
}

func TestSafetyQueriesFailClosedWhenMetadataIsUnavailable(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	head := node.chain.blockchain.CurrentBlock().Hash()
	node.chain.mu.Lock()
	original := node.chain.blockSafety[head]
	delete(node.chain.blockSafety, head)
	node.chain.mu.Unlock()
	defer func() {
		node.chain.mu.Lock()
		node.chain.blockSafety[head] = original
		node.chain.mu.Unlock()
	}()

	status := node.SafetyStatus()
	if !status.SessionTainted || !status.HeadTainted || status.FirstUnsafeBlock == nil || *status.FirstUnsafeBlock != head ||
		!strings.Contains(strings.Join(status.Reasons, ","), taintSafetyMetadataUnavailable) {
		t.Fatalf("missing safety status was not conservative: %#v", status)
	}
	safety, err := node.BlockSafety(head)
	if err != nil || !safety.Tainted || safety.FirstUnsafeBlock == nil || *safety.FirstUnsafeBlock != head ||
		!strings.Contains(strings.Join(safety.Reasons, ","), taintSafetyMetadataUnavailable) {
		t.Fatalf("missing block safety was not conservative: %#v, %v", safety, err)
	}
}

func TestPebbleRetainsMissedTailAndCheckpointSlot(t *testing.T) {
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
	if _, err := first.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if _, err := first.MissSlots(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if err := first.Checkpoint(context.Background(), "tail"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.chain.currentSlot() != 5 {
		t.Fatalf("restart slot = %d, want 5", second.chain.currentSlot())
	}
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	defer second.Close() //nolint:errcheck
	if err := second.Restore(context.Background(), "tail"); err != nil {
		t.Fatal(err)
	}
	if second.chain.currentSlot() != 4 || second.chain.blockchain.CurrentBlock().Number.Uint64() != 1 {
		t.Fatalf("restored slot/head = %d/%d, want 4/1", second.chain.currentSlot(), second.chain.blockchain.CurrentBlock().Number.Uint64())
	}
}

func TestPebbleReusesAutomaticallyResolvedGenesisTime(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.GenesisTime = 0
	cfg.Mining.Mode = miningModeManual
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	storedGenesisTime := first.cfg.Chain.GenesisTime
	if storedGenesisTime == 0 {
		t.Fatal("new Pebble chain did not resolve an automatic genesis time")
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	hashes, err := first.Mine(context.Background(), 1, true)
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
	if second.cfg.Chain.GenesisTime != storedGenesisTime || second.chain.blockchain.CurrentBlock().Hash() != hashes[0] {
		t.Fatalf("restart genesis/head = %d/%s, want %d/%s",
			second.cfg.Chain.GenesisTime, second.chain.blockchain.CurrentBlock().Hash(), storedGenesisTime, hashes[0])
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	mismatch := cfg
	mismatch.Chain.GenesisTime = storedGenesisTime + 1
	if _, err := New(mismatch); err == nil || !strings.Contains(err.Error(), "does not match stored genesis time") {
		t.Fatalf("configured genesis mismatch error = %v", err)
	}
}

func TestSameSlotBranchNeverOverwritesCanonicalIndex(t *testing.T) {
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
	canonical, err := first.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CreateBranch(context.Background(), "same-slot", 0); err != nil {
		t.Fatal(err)
	}
	side, err := first.MineBranch(context.Background(), "same-slot", 1)
	if err != nil {
		t.Fatal(err)
	}
	if side[0] == canonical[0] {
		t.Fatal("branch did not produce a distinct same-slot block")
	}
	if got, err := first.beaconBlockID("1"); err != nil || got.Hash() != canonical[0] {
		t.Fatalf("canonical slot after branch mining = %v, %v", got, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.beaconBlockID("1"); err != nil || got.Hash() != canonical[0] {
		t.Fatalf("canonical slot after restart = %v, %v", got, err)
	}
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	defer second.Close() //nolint:errcheck
	if err := second.SwitchBranch(context.Background(), "same-slot"); err != nil {
		t.Fatal(err)
	}
	if got, err := second.beaconBlockID("1"); err != nil || got.Hash() != side[0] {
		t.Fatalf("canonical slot after switch = %v, %v", got, err)
	}
}

func TestBranchSwitchCannotReplaceSyntheticFinalizedHistory(t *testing.T) {
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
	if err := node.CreateBranch(context.Background(), "stale", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := node.MineBranch(context.Background(), "stale", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 2*cfg.Chain.SlotsPerEpoch+4, true); err != nil {
		t.Fatal(err)
	}
	finalized := node.resolveSyntheticFinality(node.chain.currentSlot()).Finalized
	if finalized.NumberU64() == 0 {
		t.Fatal("test did not advance synthetic finality")
	}
	if err := node.SwitchBranch(context.Background(), "stale"); err == nil || !strings.Contains(err.Error(), "finalized history") {
		t.Fatalf("stale branch switch error = %v", err)
	}
	if err := node.CreateBranch(context.Background(), "at-finalized", finalized.NumberU64()); err != nil {
		t.Fatalf("branching from the retained finalized block should be allowed: %v", err)
	}
}

func TestRecoveryJournalBoundaries(t *testing.T) {
	for _, test := range []struct {
		name         string
		stage        commitStage
		wantHead     uint64
		wantRevision Revision
	}{
		{name: "prepared", stage: commitStagePrepared, wantHead: 0, wantRevision: 0},
		{name: "execution", stage: commitStageExecution, wantHead: 1, wantRevision: 1},
		{name: "auxiliary", stage: commitStageAuxiliary, wantHead: 1, wantRevision: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			first.commitHook = func(stage commitStage) error {
				if stage == test.stage {
					return errors.New("injected crash boundary")
				}
				return nil
			}
			if _, err := first.Mine(context.Background(), 1, true); err == nil {
				t.Fatal("expected injected failure")
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
			if second.Revision() != test.wantRevision {
				t.Fatalf("recovered revision = %d, want %d", second.Revision(), test.wantRevision)
			}
			events, err := second.EventsSince(0)
			if err != nil || Revision(len(events)) != test.wantRevision {
				t.Fatalf("recovered events = %#v, %v", events, err)
			}
		})
	}
}

func TestRecoveryJournalMismatchFailsClosed(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	operation := preparedOperation{
		Kind: "head", OldHead: common.HexToHash("0x01"), NewHead: common.HexToHash("0x02"),
	}
	if err := writePreparedOperation(node.chain.db, operation); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "head mismatch") {
		t.Fatalf("mismatched journal error = %v", err)
	}
}

func TestOldInPlaceStateWithoutMetadataIsRejected(t *testing.T) {
	cfg := testConfig()
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	kv, err := pebble.New(cfg.Storage.Path, 64, 64, "metadata-test", false)
	if err != nil {
		t.Fatal(err)
	}
	db := rawdb.NewDatabase(kv)
	if err := db.Delete(stateSchemaKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "current in-place metadata format") {
		t.Fatalf("old state rejection = %v", err)
	}
}

func TestPersistedMetadataCorruptionFailsClosed(t *testing.T) {
	t.Run("projection", func(t *testing.T) {
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
		db := openTestPebbleDatabase(t, cfg.Storage.Path)
		if err := db.Delete(projectionKey(hashes[0])); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "missing or inconsistent Beacon projection") {
			t.Fatalf("projection corruption error = %v", err)
		}
	})

	t.Run("checkpoint", func(t *testing.T) {
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
		if _, err := node.Mine(context.Background(), 1, true); err != nil {
			t.Fatal(err)
		}
		if err := node.Checkpoint(context.Background(), "stable"); err != nil {
			t.Fatal(err)
		}
		point := *node.checkpoints["stable"]
		if err := node.Close(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(storedChainPoint{
			Hash: point.hash, Number: point.number, Slot: point.slot - 1, Tainted: point.tainted,
		})
		if err != nil {
			t.Fatal(err)
		}
		db := openTestPebbleDatabase(t, cfg.Storage.Path)
		if err := db.Put(appendKey(checkpointNamespace, "stable"), encoded); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "checkpoint \"stable\" has inconsistent slot metadata") {
			t.Fatalf("checkpoint corruption error = %v", err)
		}
	})

	t.Run("branch", func(t *testing.T) {
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
		if err := node.CreateBranch(context.Background(), "alternate", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := node.MineBranch(context.Background(), "alternate", 1); err != nil {
			t.Fatal(err)
		}
		item := *node.branches["alternate"]
		if err := node.Close(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(storedBranch{
			Name: item.name, Base: item.base, Head: item.base,
			Blocks: item.blocks, Tainted: item.tainted,
		})
		if err != nil {
			t.Fatal(err)
		}
		db := openTestPebbleDatabase(t, cfg.Storage.Path)
		if err := db.Put(appendKey(branchNamespace, "alternate"), encoded); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "branch \"alternate\" head does not match its final block") {
			t.Fatalf("branch corruption error = %v", err)
		}
	})
}

func openTestPebbleDatabase(t *testing.T, path string) ethdb.Database {
	t.Helper()
	kv, err := pebble.New(path, 64, 64, "metadata-test", false)
	if err != nil {
		t.Fatal(err)
	}
	return rawdb.NewDatabase(kv)
}

func TestPendingCumulativeBalanceAndInvalidFrontierIsolation(t *testing.T) {
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
	accounts := testWalletAccounts(t, node)
	if err := node.CreateBranch(context.Background(), "funded", 0); err != nil {
		t.Fatal(err)
	}
	balance := mustBalance(t, cfg.Accounts.Balance)
	largeValue := new(big.Int).Mul(balance, big.NewInt(3))
	largeValue.Div(largeValue, big.NewInt(5))
	first := signedDynamicTransaction(t, cfg, accounts[0], 0, accounts[2].Address, largeValue, nil)
	second := signedDynamicTransaction(t, cfg, accounts[0], 1, accounts[2].Address, largeValue, nil)
	if _, err := node.SendTransaction(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(context.Background(), second); err == nil || !errors.Is(err, core.ErrInsufficientFunds) {
		t.Fatalf("cumulative overspend error = %v", err)
	}
	other := signedDynamicTransaction(t, cfg, accounts[1], 0, accounts[2].Address, big.NewInt(1), nil)
	if _, err := node.SendTransaction(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	zero := new(big.Int)
	if _, err := node.ApplyControl(context.Background(), ControlChanges{accounts[0].Address: {Balance: zero}}); err != nil {
		t.Fatal(err)
	}
	pending, queued := (&txpoolAPI{node: node}).poolCounts()
	if pending != 1 || queued != 1 {
		t.Fatalf("pending classification = %d/%d, want 1/1", pending, queued)
	}
	hashes, err := node.Mine(context.Background(), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	mined := node.chain.blockchain.GetBlockByHash(hashes[0])
	if len(mined.Transactions()) != 1 || mined.Transactions()[0].Hash() != other.Hash() {
		t.Fatalf("mined transactions = %#v, want only %s", mined.Transactions(), other.Hash())
	}
	if _, queued = (&txpoolAPI{node: node}).poolCounts(); queued != 1 {
		t.Fatalf("queued count after mining = %d, want 1", queued)
	}
	if err := node.SwitchBranch(context.Background(), "funded"); err != nil {
		t.Fatal(err)
	}
	if pending, queued = (&txpoolAPI{node: node}).poolCounts(); pending != 1 || queued != 0 {
		t.Fatalf("classification after funded reorg = %d/%d, want 1/0", pending, queued)
	}
}

func TestPendingReplacementAndNonceGapClassification(t *testing.T) {
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
	recipient := node.Accounts()[1]
	sign := func(nonce uint64, tip int64) *types.Transaction {
		t.Helper()
		tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
			ChainID: new(big.Int).SetUint64(cfg.Chain.ChainID), Nonce: nonce,
			GasTipCap: big.NewInt(tip), GasFeeCap: new(big.Int).Mul(big.NewInt(tip), big.NewInt(4)),
			Gas: 21_000, To: &recipient, Value: big.NewInt(1),
		}), types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.Chain.ChainID)), account.PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}
	gap := sign(1, 1_000_000_000)
	if _, err := node.SendTransaction(context.Background(), gap); err != nil {
		t.Fatal(err)
	}
	if pending, queued := (&txpoolAPI{node: node}).poolCounts(); pending != 0 || queued != 1 {
		t.Fatalf("nonce gap classification = %d/%d, want 0/1", pending, queued)
	}
	first := sign(0, 1_000_000_000)
	if _, err := node.SendTransaction(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if pending, queued := (&txpoolAPI{node: node}).poolCounts(); pending != 2 || queued != 0 {
		t.Fatalf("filled gap classification = %d/%d, want 2/0", pending, queued)
	}
	underpriced := sign(0, 1_050_000_000)
	if _, err := node.SendTransaction(context.Background(), underpriced); err == nil || !strings.Contains(err.Error(), "underpriced") {
		t.Fatalf("underpriced replacement error = %v", err)
	}
	replacement := sign(0, 1_100_000_000)
	if _, err := node.SendTransaction(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if pending, queued := (&txpoolAPI{node: node}).poolCounts(); pending != 2 || queued != 0 {
		t.Fatalf("replacement classification = %d/%d, want 2/0", pending, queued)
	}
	if executable, _ := node.chain.pendingClassification(first.Hash()); executable {
		t.Fatal("replaced transaction remained executable")
	}
	if executable, _ := node.chain.pendingClassification(replacement.Hash()); !executable {
		t.Fatal("replacement transaction is not executable")
	}
}

func TestPendingRPCsShareFrozenCandidateState(t *testing.T) {
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
	contract := crypto.CreateAddress(account.Address, 0)
	// Store 0x2a in slot zero, then deploy one STOP byte as runtime code.
	initCode := common.FromHex("0x602a6000556001601160003960016000f300")
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: new(big.Int).SetUint64(cfg.Chain.ChainID), Nonce: 0,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		Gas: 150_000, Value: big.NewInt(123), Data: initCode,
	})
	creation, err := types.SignTx(unsigned, types.LatestSignerForChainID(unsigned.ChainId()), account.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(context.Background(), creation); err != nil {
		t.Fatal(err)
	}

	client := node.RPCClient()
	defer client.Close()
	var latestBalance, pendingBalance hexutil.Big
	if err := client.Call(&latestBalance, "eth_getBalance", contract, "latest"); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&pendingBalance, "eth_getBalance", contract, "pending"); err != nil {
		t.Fatal(err)
	}
	if (*big.Int)(&latestBalance).Sign() != 0 || (*big.Int)(&pendingBalance).Cmp(big.NewInt(123)) != 0 {
		t.Fatalf("latest/pending balances = %s/%s", (*big.Int)(&latestBalance), (*big.Int)(&pendingBalance))
	}
	var latestNonce, pendingNonce hexutil.Uint64
	if err := client.Call(&latestNonce, "eth_getTransactionCount", account.Address, "latest"); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(&pendingNonce, "eth_getTransactionCount", account.Address, "pending"); err != nil {
		t.Fatal(err)
	}
	if latestNonce != 0 || pendingNonce != 1 {
		t.Fatalf("latest/pending nonces = %d/%d", latestNonce, pendingNonce)
	}
	var code hexutil.Bytes
	if err := client.Call(&code, "eth_getCode", contract, "pending"); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(hexutil.Encode(code), "0x00") {
		t.Fatalf("pending code = %x", []byte(code))
	}
	var storage hexutil.Bytes
	if err := client.Call(&storage, "eth_getStorageAt", contract, common.Hash{}, "pending"); err != nil {
		t.Fatal(err)
	}
	if common.BytesToHash(storage) != common.BigToHash(big.NewInt(42)) {
		t.Fatalf("pending storage = %x", []byte(storage))
	}
	var callResult hexutil.Bytes
	if err := client.Call(&callResult, "eth_call", map[string]any{"to": contract}, "pending"); err != nil || len(callResult) != 0 {
		t.Fatalf("pending call = %x, %v", []byte(callResult), err)
	}
	var estimate hexutil.Uint64
	if err := client.Call(&estimate, "eth_estimateGas", map[string]any{"to": contract}, "pending"); err != nil || estimate < 21_000 {
		t.Fatalf("pending estimate = %d, %v", estimate, err)
	}
	var proof accountProof
	if err := client.Call(&proof, "eth_getProof", contract, []common.Hash{{}}, "pending"); err != nil {
		t.Fatal(err)
	}
	if (*big.Int)(proof.Balance).Cmp(big.NewInt(123)) != 0 || len(proof.StorageProof) != 1 ||
		(*big.Int)(proof.StorageProof[0].Value).Cmp(big.NewInt(42)) != 0 {
		t.Fatalf("pending proof = %#v", proof)
	}
	var pendingBlock struct {
		Hash         *common.Hash      `json:"hash"`
		Nonce        *types.BlockNonce `json:"nonce"`
		Miner        *common.Address   `json:"miner"`
		StateRoot    common.Hash       `json:"stateRoot"`
		Transactions []common.Hash     `json:"transactions"`
	}
	if err := client.Call(&pendingBlock, "eth_getBlockByNumber", "pending", false); err != nil {
		t.Fatal(err)
	}
	block, state, receipts := node.chain.pendingSnapshot()
	if block == nil || state == nil || pendingBlock.Hash != nil || pendingBlock.Nonce != nil || pendingBlock.Miner != nil || pendingBlock.StateRoot != block.Root() ||
		len(pendingBlock.Transactions) != 1 || pendingBlock.Transactions[0] != creation.Hash() || len(receipts) != 1 ||
		state.GetState(contract, common.Hash{}) != common.BigToHash(big.NewInt(42)) {
		t.Fatalf("pending candidate mismatch: rpc=%#v block=%v receipts=%d", pendingBlock, block, len(receipts))
	}
}

func TestEIP1898ErrorsAcrossStateQueries(t *testing.T) {
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
	if err := node.CreateBranch(context.Background(), "side", 0); err != nil {
		t.Fatal(err)
	}
	hashes, err := node.MineBranch(context.Background(), "side", 1)
	if err != nil {
		t.Fatal(err)
	}
	client := node.RPCClient()
	defer client.Close()
	address := node.Accounts()[0]
	nonCanonical := map[string]any{"blockHash": hashes[0], "requireCanonical": true}
	var balance hexutil.Big
	err = client.Call(&balance, "eth_getBalance", address, nonCanonical)
	assertRPCErrorCode(t, err, -32000)
	missing := map[string]any{"blockHash": common.HexToHash("0xdeadbeef")}
	tests := []struct {
		method string
		args   []any
	}{
		{method: "eth_getBalance", args: []any{address}},
		{method: "eth_getTransactionCount", args: []any{address}},
		{method: "eth_getCode", args: []any{address}},
		{method: "eth_getStorageAt", args: []any{address, common.Hash{}}},
		{method: "eth_call", args: []any{map[string]any{"to": address}}},
		{method: "eth_estimateGas", args: []any{map[string]any{"to": address}}},
		{method: "eth_getProof", args: []any{address, []common.Hash{}}},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			args := append(append([]any(nil), test.args...), missing)
			var result any
			assertRPCErrorCode(t, client.Call(&result, test.method, args...), -32001)
		})
	}
}

func assertRPCErrorCode(t *testing.T, err error, code int) {
	t.Helper()
	var rpcErr rpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.ErrorCode() != code {
		t.Fatalf("RPC error = %T %v, want code %d", err, err, code)
	}
}

func TestMinerStopStartOwnsRuntimeState(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "interval"
	cfg.Mining.Interval = 20 * time.Millisecond
	cfg.Mining.AutoMineEmpty = true
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	waitForHead(t, node, 1)
	client := node.RPCClient()
	defer client.Close()
	var stopped bool
	if err := client.Call(&stopped, "miner_stop"); err != nil || !stopped {
		t.Fatalf("miner_stop = %v, %v", stopped, err)
	}
	stoppedAt := node.chain.blockchain.CurrentBlock().Number.Uint64()
	time.Sleep(80 * time.Millisecond)
	if head := node.chain.blockchain.CurrentBlock().Number.Uint64(); head != stoppedAt {
		t.Fatalf("interval miner advanced after stop: %d -> %d", stoppedAt, head)
	}
	var mining bool
	if err := client.Call(&mining, "eth_mining"); err != nil || mining {
		t.Fatalf("eth_mining after stop = %v, %v", mining, err)
	}
	var started bool
	if err := client.Call(&started, "miner_start"); err != nil || !started {
		t.Fatalf("miner_start = %v, %v", started, err)
	}
	waitForHead(t, node, stoppedAt+1)
	if err := client.Call(&mining, "eth_mining"); err != nil || !mining {
		t.Fatalf("eth_mining after start = %v, %v", mining, err)
	}
}

func TestMinerStartFromInitialManualModeUsesTransactionAutomining(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := (&minerAPI{node: node}).Start(context.Background(), nil); ok || !errors.Is(err, ErrNodeStopped) {
		t.Fatalf("pre-start miner_start = %v, %v", ok, err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()
	var started bool
	if err := client.Call(&started, "miner_start"); err != nil || !started {
		t.Fatalf("miner_start = %v, %v", started, err)
	}
	tx := signedDynamicTransaction(t, cfg, testWalletAccount(t, node, 0), 0, node.Accounts()[1], big.NewInt(1), nil)
	if _, err := node.SendTransaction(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if head := node.chain.blockchain.CurrentBlock().Number.Uint64(); head != 1 {
		t.Fatalf("transaction automining head = %d, want 1", head)
	}
}

func waitForHead(t *testing.T, node *Node, minimum uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if node.chain.blockchain.CurrentBlock().Number.Uint64() >= minimum {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("head did not reach %d", minimum)
}

func TestTraceTransactionReplaysPreExecutionSystemCalls(t *testing.T) {
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
	targetTime := uint64(cfg.Chain.GenesisTime) + uint64(cfg.Chain.SlotDuration/time.Second)
	beaconInput := common.LeftPadBytes(new(big.Int).SetUint64(targetTime).Bytes(), 32)
	historyInput := make([]byte, 32)
	beaconTx := signedDynamicTransaction(t, cfg, account, 0, params.BeaconRootsAddress, new(big.Int), beaconInput)
	historyTx := signedDynamicTransaction(t, cfg, account, 1, params.HistoryStorageAddress, new(big.Int), historyInput)
	if _, err := node.SendTransaction(context.Background(), beaconTx); err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(context.Background(), historyTx); err != nil {
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
	parentProjection, err := node.consensus.ensureProjection(node.chain, node.chain.blockchain.Genesis())
	if err != nil {
		t.Fatal(err)
	}
	client := node.RPCClient()
	defer client.Close()
	for _, test := range []struct {
		hash common.Hash
		want common.Hash
	}{
		{hash: beaconTx.Hash(), want: common.Hash(parentProjection.Root)},
		{hash: historyTx.Hash(), want: node.chain.blockchain.Genesis().Hash()},
	} {
		var raw json.RawMessage
		if err := client.Call(&raw, "debug_traceTransaction", test.hash, map[string]any{"tracer": "callTracer"}); err != nil {
			t.Fatal(err)
		}
		var trace struct {
			Output hexutil.Bytes `json:"output"`
			Error  string        `json:"error"`
		}
		if err := json.Unmarshal(raw, &trace); err != nil {
			t.Fatal(err)
		}
		if trace.Error != "" || common.BytesToHash(trace.Output) != test.want {
			t.Fatalf("trace %s output=%x error=%q, want %s", test.hash, []byte(trace.Output), trace.Error, test.want)
		}
	}
	var blockTraces []traceResult
	if err := client.Call(&blockTraces, "debug_traceBlockByHash", block.Hash(), map[string]any{"tracer": "callTracer"}); err != nil {
		t.Fatal(err)
	}
	if len(blockTraces) != 2 {
		t.Fatalf("block trace length = %d, want 2", len(blockTraces))
	}
	for index, want := range []common.Hash{common.Hash(parentProjection.Root), node.chain.blockchain.Genesis().Hash()} {
		var trace struct {
			Output hexutil.Bytes `json:"output"`
			Error  string        `json:"error"`
		}
		if err := json.Unmarshal(blockTraces[index].Result, &trace); err != nil {
			t.Fatal(err)
		}
		if trace.Error != "" || common.BytesToHash(trace.Output) != want {
			t.Fatalf("block trace %d output=%x error=%q, want %s", index, []byte(trace.Output), trace.Error, want)
		}
	}
}
