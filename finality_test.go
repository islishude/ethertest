package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type finalityRPCBlock struct {
	Hash   common.Hash    `json:"hash"`
	Number hexutil.Uint64 `json:"number"`
}

type finalityCheckpointEnvelope struct {
	Finalized bool `json:"finalized"`
	Data      struct {
		CurrentJustified struct {
			Epoch string `json:"epoch"`
			Root  string `json:"root"`
		} `json:"current_justified"`
		Finalized struct {
			Epoch string `json:"epoch"`
			Root  string `json:"root"`
		} `json:"finalized"`
	} `json:"data"`
}

func TestSyntheticFinalityPauseResumeAcrossSurfaces(t *testing.T) {
	node := newFinalityTestNode(t)
	ctx := context.Background()
	if _, err := node.Mine(ctx, 6, true); err != nil {
		t.Fatal(err)
	}
	assertFinalityStatus(t, node, false, 6, 6)

	// A short pause/resume and repeated calls are idempotent and do not create
	// revisions when the projected checkpoint does not change.
	revision := node.Revision()
	if err := node.PauseFinality(ctx); err != nil {
		t.Fatal(err)
	}
	if err := node.PauseFinality(ctx); err != nil {
		t.Fatal(err)
	}
	assertFinalityStatus(t, node, true, 6, 6)
	if node.Revision() != revision {
		t.Fatalf("pause changed revision from %d to %d", revision, node.Revision())
	}
	if err := node.ResumeFinality(ctx); err != nil {
		t.Fatal(err)
	}
	if node.Revision() != revision {
		t.Fatalf("short resume changed revision from %d to %d", revision, node.Revision())
	}
	if err := node.PauseFinality(ctx); err != nil {
		t.Fatal(err)
	}

	frozen := node.FinalityStatus()
	if _, err := node.MissSlots(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := node.Mine(ctx, 2, true); err != nil {
		t.Fatal(err)
	}
	assertFinalityStatus(t, node, true, 11, 6)
	assertFinalityELTags(t, node, frozen)
	assertBeaconFinalityCheckpoints(t, node, "head", 6)
	assertBeaconFinalityCheckpoints(t, node, "4", 4)
	assertBeaconBlockFinalized(t, node, "2", true)
	assertBeaconBlockFinalized(t, node, "3", false)

	events, err := node.EventsSince(revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "finalized_checkpoint" {
			t.Fatalf("paused progression published finalized event %#v", event)
		}
	}

	revision = node.Revision()
	if err := node.ResumeFinality(ctx); err != nil {
		t.Fatal(err)
	}
	resumed := node.FinalityStatus()
	assertFinalityStatus(t, node, false, 11, 11)
	assertFinalityELTags(t, node, resumed)
	assertBeaconFinalityCheckpoints(t, node, "head", 11)
	events, err = node.EventsSince(revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "finalized_checkpoint" ||
		events[0].Slot != resumed.FinalizedSlot ||
		events[0].BlockHash != resumed.FinalizedBlockHash ||
		events[0].BlockNumber != resumed.FinalizedBlockNumber {
		t.Fatalf("resume events = %#v, want one latest finalized checkpoint", events)
	}
	messages := node.beaconEventMessages(events[0], map[string]bool{"finalized_checkpoint": true})
	if len(messages) != 1 || messages[0].topic != "finalized_checkpoint" || messages[0].ordinal != 3 {
		t.Fatalf("resume Beacon event messages = %#v", messages)
	}
	message, ok := messages[0].data.(map[string]any)
	if !ok || message["epoch"] != strconv.FormatUint(resumed.FinalizedSlot/node.cfg.Chain.SlotsPerEpoch, 10) ||
		message["block"] == "" || message["state"] == "" {
		t.Fatalf("resume finalized SSE payload = %#v", messages[0].data)
	}
	revision = node.Revision()
	if err := node.ResumeFinality(ctx); err != nil {
		t.Fatal(err)
	}
	if node.Revision() != revision {
		t.Fatalf("repeated resume changed revision from %d to %d", revision, node.Revision())
	}
}

func TestPausedFinalityGuardsBranchesAndClampsCanonicalRewinds(t *testing.T) {
	t.Run("frozen finalized history remains protected", func(t *testing.T) {
		node := newFinalityTestNode(t)
		ctx := context.Background()
		if err := node.CreateBranch(ctx, "stale", 0); err != nil {
			t.Fatal(err)
		}
		if _, err := node.MineBranch(ctx, "stale", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(ctx, 8, true); err != nil {
			t.Fatal(err)
		}
		if err := node.PauseFinality(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := node.MissSlots(ctx, 4); err != nil {
			t.Fatal(err)
		}
		if err := node.SwitchBranch(ctx, "stale"); err == nil || !strings.Contains(err.Error(), "finalized history") {
			t.Fatalf("stale branch switch error = %v", err)
		}
		assertFinalityStatus(t, node, true, 12, 8)
	})

	t.Run("snapshot revert", func(t *testing.T) {
		node := newFinalityTestNode(t)
		ctx := context.Background()
		if _, err := node.Mine(ctx, 3, true); err != nil {
			t.Fatal(err)
		}
		snapshot, err := node.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(ctx, 5, true); err != nil {
			t.Fatal(err)
		}
		if err := node.PauseFinality(ctx); err != nil {
			t.Fatal(err)
		}
		if reverted, err := node.Revert(ctx, snapshot); err != nil || !reverted {
			t.Fatalf("snapshot revert = %v, %v", reverted, err)
		}
		assertFinalityStatus(t, node, true, 3, 3)
	})

	t.Run("checkpoint restore", func(t *testing.T) {
		node := newFinalityTestNode(t)
		ctx := context.Background()
		if _, err := node.Mine(ctx, 4, true); err != nil {
			t.Fatal(err)
		}
		if err := node.Checkpoint(ctx, "early"); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(ctx, 4, true); err != nil {
			t.Fatal(err)
		}
		if err := node.PauseFinality(ctx); err != nil {
			t.Fatal(err)
		}
		if err := node.Restore(ctx, "early"); err != nil {
			t.Fatal(err)
		}
		assertFinalityStatus(t, node, true, 4, 4)
	})

	t.Run("branch switch", func(t *testing.T) {
		node := newFinalityTestNode(t)
		ctx := context.Background()
		if _, err := node.Mine(ctx, 5, true); err != nil {
			t.Fatal(err)
		}
		if err := node.CreateBranch(ctx, "short", 5); err != nil {
			t.Fatal(err)
		}
		if _, err := node.MineBranch(ctx, "short", 1); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(ctx, 3, true); err != nil {
			t.Fatal(err)
		}
		if err := node.PauseFinality(ctx); err != nil {
			t.Fatal(err)
		}
		if err := node.SwitchBranch(ctx, "short"); err != nil {
			t.Fatal(err)
		}
		assertFinalityStatus(t, node, true, 6, 6)
	})
}

func TestPausedFinalityPersistsAcrossPebbleAndStateArchive(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Chain.SlotsPerEpoch = 2
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")

	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Mine(ctx, 6, true); err != nil {
		t.Fatal(err)
	}
	if err := first.PauseFinality(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := first.MissSlots(ctx, 3); err != nil {
		t.Fatal(err)
	}
	assertFinalityStatus(t, first, true, 9, 6)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	assertFinalityStatus(t, second, true, 9, 6)
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Mine(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	assertFinalityStatus(t, second, true, 10, 6)
	archive := filepath.Join(t.TempDir(), "paused-state.tar.zst")
	if err := second.DumpState(archive); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "loaded")
	if err := LoadState(archive, destination); err != nil {
		t.Fatal(err)
	}
	loadedConfig := cfg
	loadedConfig.Storage.Path = destination
	loaded, err := New(loadedConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = loaded.Close() })
	assertFinalityStatus(t, loaded, true, 10, 6)
	if err := loaded.Start(); err != nil {
		t.Fatal(err)
	}
	revision := loaded.Revision()
	if err := loaded.ResumeFinality(ctx); err != nil {
		t.Fatal(err)
	}
	assertFinalityStatus(t, loaded, false, 10, 10)
	events, err := loaded.EventsSince(revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "finalized_checkpoint" {
		t.Fatalf("loaded resume events = %#v", events)
	}
}

func TestFinalityRPCSurfaceAndNamespaceIsolation(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Chain.SlotsPerEpoch = 2
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.PauseFinality(context.Background()); !errors.Is(err, ErrNodeStopped) {
		t.Fatalf("pre-start pause error = %v", err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()

	var capabilities map[string]any
	if err := client.Call(&capabilities, "ethertest_capabilities"); err != nil {
		t.Fatal(err)
	}
	if capabilities["finalityControls"] != true {
		t.Fatalf("finality capability = %#v", capabilities["finalityControls"])
	}

	assertFinalityRPCStatusFields(t, clientFinalityStatus(t, client), false)
	var ok bool
	if err := client.Call(&ok, "ethertest_pauseFinality"); err != nil || !ok {
		t.Fatalf("pause RPC = %v, %v", ok, err)
	}
	assertFinalityRPCStatusFields(t, clientFinalityStatus(t, client), true)
	if _, err := node.Mine(context.Background(), 5, true); err != nil {
		t.Fatal(err)
	}
	status := clientFinalityStatus(t, client)
	if !status.Paused || status.CurrentSlot != 5 || status.FinalitySlot != 0 {
		t.Fatalf("paused RPC status = %#v", status)
	}
	if err := client.Call(&ok, "ethertest_resumeFinality"); err != nil || !ok {
		t.Fatalf("resume RPC = %v, %v", ok, err)
	}
	status = clientFinalityStatus(t, client)
	if status.Paused || status.CurrentSlot != 5 || status.FinalitySlot != 5 {
		t.Fatalf("resumed RPC status = %#v", status)
	}

	for _, method := range []string{
		"ethertest_pauseFinality", "ethertest_resumeFinality", "ethertest_finalityStatus",
	} {
		var result any
		assertRPCErrorCode(t, client.Call(&result, method, true), -32602)
	}
	for _, namespace := range []string{"anvil", "evm"} {
		for _, method := range []string{"pauseFinality", "resumeFinality", "finalityStatus"} {
			var result any
			assertRPCErrorCode(t, client.Call(&result, namespace+"_"+method), -32601)
		}
	}
}

func newFinalityTestNode(t *testing.T) *Node {
	t.Helper()
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Chain.SlotsPerEpoch = 2
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = node.Close() })
	return node
}

func assertFinalityStatus(t *testing.T, node *Node, paused bool, currentSlot, finalitySlot uint64) {
	t.Helper()
	status := node.FinalityStatus()
	want := node.resolveSyntheticFinalityAt(finalitySlot)
	if status.Paused != paused || status.CurrentSlot != currentSlot || status.FinalitySlot != finalitySlot ||
		status.SafeSlot != want.SafeSlot || status.SafeBlockHash != want.Safe.Hash() ||
		status.SafeBlockNumber != want.Safe.NumberU64() || status.FinalizedSlot != want.FinalizedSlot ||
		status.FinalizedBlockHash != want.Finalized.Hash() || status.FinalizedBlockNumber != want.Finalized.NumberU64() ||
		status.ConsensusMode != "synthetic" {
		t.Fatalf("finality status = %#v, want paused=%v current=%d observer=%d resolution=%#v",
			status, paused, currentSlot, finalitySlot, want)
	}
}

func assertFinalityELTags(t *testing.T, node *Node, status FinalityStatus) {
	t.Helper()
	client := node.RPCClient()
	defer client.Close()
	for _, test := range []struct {
		tag    string
		hash   common.Hash
		number uint64
	}{
		{tag: "safe", hash: status.SafeBlockHash, number: status.SafeBlockNumber},
		{tag: "finalized", hash: status.FinalizedBlockHash, number: status.FinalizedBlockNumber},
	} {
		var block finalityRPCBlock
		if err := client.Call(&block, "eth_getBlockByNumber", test.tag, false); err != nil {
			t.Fatal(err)
		}
		if block.Hash != test.hash || uint64(block.Number) != test.number {
			t.Fatalf("%s block = %#v, want %s/%d", test.tag, block, test.hash, test.number)
		}
	}
}

func assertBeaconFinalityCheckpoints(t *testing.T, node *Node, stateID string, observerSlot uint64) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/states/"+stateID+"/finality_checkpoints", nil)
	response := httptest.NewRecorder()
	node.beaconHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("finality checkpoints %s status=%d body=%s", stateID, response.Code, response.Body.String())
	}
	var envelope finalityCheckpointEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	want := node.resolveSyntheticFinalityAt(observerSlot)
	safeRoot, err := node.beaconRoot(want.Safe)
	if err != nil {
		t.Fatal(err)
	}
	finalizedRoot, err := node.beaconRoot(want.Finalized)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Data.CurrentJustified.Epoch != strconv.FormatUint(want.SafeSlot/node.cfg.Chain.SlotsPerEpoch, 10) ||
		envelope.Data.CurrentJustified.Root != common.Hash(safeRoot).Hex() ||
		envelope.Data.Finalized.Epoch != strconv.FormatUint(want.FinalizedSlot/node.cfg.Chain.SlotsPerEpoch, 10) ||
		envelope.Data.Finalized.Root != common.Hash(finalizedRoot).Hex() {
		t.Fatalf("finality checkpoints %s = %#v, want resolution %#v", stateID, envelope.Data, want)
	}
}

func assertBeaconBlockFinalized(t *testing.T, node *Node, blockID string, finalized bool) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/"+blockID, nil)
	response := httptest.NewRecorder()
	node.beaconHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Beacon block %s status=%d body=%s", blockID, response.Code, response.Body.String())
	}
	var envelope struct {
		Finalized bool `json:"finalized"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Finalized != finalized {
		t.Fatalf("Beacon block %s finalized=%v, want %v", blockID, envelope.Finalized, finalized)
	}
}

func clientFinalityStatus(t *testing.T, client interface {
	Call(result any, method string, args ...any) error
}) FinalityStatus {
	t.Helper()
	var status FinalityStatus
	if err := client.Call(&status, "ethertest_finalityStatus"); err != nil {
		t.Fatal(err)
	}
	return status
}

func assertFinalityRPCStatusFields(t *testing.T, status FinalityStatus, paused bool) {
	t.Helper()
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"paused", "current_slot", "finality_slot", "safe_slot", "safe_block_hash", "safe_block_number",
		"finalized_slot", "finalized_block_hash", "finalized_block_number", "consensus_mode",
	}
	if len(fields) != len(want) {
		t.Fatalf("finality status fields = %#v", fields)
	}
	for _, field := range want {
		if _, exists := fields[field]; !exists {
			t.Fatalf("finality status is missing %q: %#v", field, fields)
		}
	}
	if status.Paused != paused || status.ConsensusMode != "synthetic" {
		t.Fatalf("finality RPC status = %#v", status)
	}
}
