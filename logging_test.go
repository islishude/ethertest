package ethertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestNodeLoggingIsOptIn(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)

	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("embedded node used the process default logger: %s", output.String())
	}
}

func TestNodeEmitsKeyLifecycleAndControlEventsWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := node.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if reverted, err := node.Revert(context.Background(), snapshot); err != nil || !reverted {
		t.Fatalf("revert: ok=%v err=%v", reverted, err)
	}
	if err := node.Checkpoint(context.Background(), "stable"); err != nil {
		t.Fatal(err)
	}
	if err := node.CreateBranch(context.Background(), "alternative", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := node.MineBranch(context.Background(), "alternative", 1); err != nil {
		t.Fatal(err)
	}
	if err := node.SwitchBranch(context.Background(), "alternative"); err != nil {
		t.Fatal(err)
	}
	secretValue := new(big.Int)
	secretValue.SetString("987654321987654321", 10)
	if _, err := node.ApplyControl(context.Background(), ControlChanges{
		common.HexToAddress("0x1234"): {Balance: secretValue},
	}); err != nil {
		t.Fatal(err)
	}
	if err := node.DumpState(filepath.Join(t.TempDir(), "state.tar.zst")); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}

	events := loggedEvents(t, output.String())
	for _, event := range []string{
		"node_started",
		"blocks_mined",
		"chain_reorganized",
		"snapshot_reverted",
		"checkpoint_created",
		"branch_created",
		"branch_blocks_mined",
		"branch_switched",
		"control_block_applied",
		"state_archive_written",
		"node_stopping",
		"node_stopped",
	} {
		if events[event] == 0 {
			t.Errorf("missing %s in logs:\n%s", event, output.String())
		}
	}
	if events["snapshot_created"] != 0 {
		t.Fatal("debug snapshot event was emitted at info level")
	}
	for _, secret := range []string{
		DefaultMnemonic,
		"ac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
		secretValue.String(),
	} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("logs contain secret value %q", secret)
		}
	}
}

func TestProgressReporterCoalescesAndSuppressesEmptyWindows(t *testing.T) {
	reporter := new(progressReporter)
	if _, ok := reporter.take(); ok {
		t.Fatal("empty reporter produced a summary")
	}
	first := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(7)})
	second := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(8)})
	reporter.record(first, 9)
	reporter.record(second, 11)
	summary, ok := reporter.take()
	if !ok {
		t.Fatal("missing progress summary")
	}
	if summary.blocks != 2 || summary.firstBlock != 7 || summary.lastBlock != 8 ||
		summary.headHash != second.Hash().Hex() || summary.slot != 11 {
		t.Fatalf("unexpected summary %#v", summary)
	}
	if _, ok := reporter.take(); ok {
		t.Fatal("reporter did not reset after take")
	}
}

func TestDebugBlockLoggingReplacesInfoProgress(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := testConfig()
	node, err := New(cfg, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(context.Background(), signedLoggingTestTransfer(t)); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	events := loggedEvents(t, output.String())
	if events["transaction_accepted"] != 1 || events["block_mined"] != 1 {
		t.Fatalf("missing debug transaction/block events: %v", events)
	}
	if events["chain_progress"] != 0 {
		t.Fatalf("debug logging duplicated aggregate progress: %v", events)
	}
}

func TestInfoAutomaticMiningIsCoalesced(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo}))
	node, err := New(testConfig(), WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := node.SendTransaction(context.Background(), signedLoggingTestTransfer(t)); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	events := loggedEvents(t, output.String())
	if events["chain_progress"] != 1 || events["block_mined"] != 0 ||
		events["transaction_accepted"] != 0 {
		t.Fatalf("automatic mining was not coalesced at info level: %v", events)
	}
}

func TestIntervalFailureLoggingIsBoundedAndReportsRecovery(t *testing.T) {
	var output bytes.Buffer
	node := &Node{logger: slog.New(slog.NewJSONHandler(&output, nil))}
	start := time.Unix(1_800_000_000, 0)
	failure := errors.New("temporary failure")
	node.reportIntervalFailureAt("interval_mining_failed", "interval mining failed", failure, start)
	node.reportIntervalFailureAt("interval_mining_failed", "interval mining failed", failure, start.Add(30*time.Second))
	node.reportIntervalFailureAt("interval_mining_failed", "interval mining failed", failure, start.Add(time.Minute))
	node.reportIntervalRecovery()
	node.reportIntervalRecovery()

	events := loggedEvents(t, output.String())
	if events["interval_mining_failed"] != 2 || events["interval_mining_recovered"] != 1 {
		t.Fatalf("unexpected bounded failure events: %v", events)
	}
}

func signedLoggingTestTransfer(t *testing.T) *types.Transaction {
	t.Helper()
	accounts, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	chainID := big.NewInt(int64(DefaultChainID))
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: 0,
		GasTipCap: big.NewInt(1), GasFeeCap: big.NewInt(2_000_000_000),
		Gas: 21_000, To: &accounts[1].Address, Value: big.NewInt(1),
	})
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(chainID), accounts[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func loggedEvents(t *testing.T, output string) map[string]int {
	t.Helper()
	events := make(map[string]int)
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSON log %q: %v", line, err)
		}
		event, _ := record["event"].(string)
		events[event]++
	}
	return events
}
