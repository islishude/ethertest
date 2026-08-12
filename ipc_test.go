package ethertest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func ipcTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.IPC.Enabled = true
	if runtime.GOOS == "windows" {
		cfg.IPC.Path = "ethertest-test-" + filepath.Base(t.TempDir())
	} else {
		directory, err := os.MkdirTemp("/tmp", "ethertest-ipc-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(directory) })
		cfg.IPC.Path = filepath.Join(directory, "ethertest.ipc")
	}
	return cfg
}

func TestIPCOnlyRPCBatchAndSubscription(t *testing.T) {
	cfg := ipcTestConfig(t)
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	endpoints := node.Endpoints()
	if endpoints.Execution != "" || endpoints.Beacon != "" || endpoints.IPC != cfg.IPCEndpoint() {
		t.Fatalf("IPC-only endpoints = %#v", endpoints)
	}
	client, err := rpc.DialIPC(t.Context(), endpoints.IPC)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var chainID hexutil.Uint64
	var blockNumber hexutil.Uint64
	batch := []rpc.BatchElem{
		{Method: "eth_chainId", Result: &chainID},
		{Method: "eth_blockNumber", Result: &blockNumber},
	}
	if err := client.BatchCall(batch); err != nil {
		t.Fatal(err)
	}
	for _, element := range batch {
		if element.Error != nil {
			t.Fatal(element.Error)
		}
	}
	if uint64(chainID) != DefaultChainID || blockNumber != 0 {
		t.Fatalf("unexpected IPC batch result: chain=%d block=%d", chainID, blockNumber)
	}
	var capabilities map[string]any
	if err := client.Call(&capabilities, "ethertest_capabilities"); err != nil {
		t.Fatal(err)
	}
	if capabilities["ipc"] != true {
		t.Fatalf("IPC capability is missing: %#v", capabilities)
	}
	var networkConfig map[string]any
	if err := client.Call(&networkConfig, "ethertest_networkConfig"); err != nil {
		t.Fatal(err)
	}
	if networkConfig["ipc"] != endpoints.IPC || networkConfig["el"] != "" || networkConfig["beacon"] != "" {
		t.Fatalf("IPC-only network config = %#v", networkConfig)
	}

	headers := make(chan *types.Header, 1)
	subscription, err := client.Subscribe(context.Background(), "eth", headers, "newHeads")
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Unsubscribe()
	var hashes []string
	if err := client.Call(&hashes, "ethertest_mine", hexutil.Uint64(1)); err != nil {
		t.Fatal(err)
	}
	select {
	case header := <-headers:
		if header.Number.Uint64() != 1 || node.chain.blockchain.CurrentBlock().Number.Uint64() != 1 {
			t.Fatalf("IPC write did not update the canonical chain: header=%d head=%d", header.Number.Uint64(), node.chain.blockchain.CurrentBlock().Number.Uint64())
		}
	case err := <-subscription.Err():
		t.Fatalf("IPC subscription failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for IPC newHeads event")
	}
}

func TestHTTPAndIPCShareChainState(t *testing.T) {
	cfg := ipcTestConfig(t)
	cfg.HTTP.Enabled = true
	cfg.HTTP.Address = "127.0.0.1:0"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	ipcClient, err := rpc.DialIPC(t.Context(), node.Endpoints().IPC)
	if err != nil {
		t.Fatal(err)
	}
	defer ipcClient.Close()
	httpClient, err := rpc.DialHTTP(node.Endpoints().Execution)
	if err != nil {
		t.Fatal(err)
	}
	defer httpClient.Close()

	var hashes []string
	if err := ipcClient.Call(&hashes, "ethertest_mine", hexutil.Uint64(1)); err != nil {
		t.Fatal(err)
	}
	var blockNumber hexutil.Uint64
	if err := httpClient.Call(&blockNumber, "eth_blockNumber"); err != nil {
		t.Fatal(err)
	}
	if blockNumber != 1 {
		t.Fatalf("HTTP observed block %d after IPC mining, want 1", blockNumber)
	}
}

func TestIPCLifecycleAndPermissions(t *testing.T) {
	cfg := ipcTestConfig(t)
	endpoint := cfg.IPCEndpoint()
	startAndStop := func() {
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("IPC socket permissions = %o, want 600", info.Mode().Perm())
			}
		}
		if err := node.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		client, err := rpc.DialIPC(ctx, endpoint)
		if err == nil {
			client.Close()
			t.Fatal("IPC endpoint accepted a connection after shutdown")
		}
		if runtime.GOOS != "windows" {
			_, statErr := os.Stat(endpoint)
			if !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("IPC socket remains after shutdown: %v", statErr)
			}
		}
	}
	startAndStop()
	startAndStop()
}

func TestIPCStartupFailureStopsNode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix filesystem-specific failure")
	}
	cfg := ipcTestConfig(t)
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.IPC.Path = filepath.Join(parent, "ethertest.ipc")
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err == nil {
		t.Fatal("expected IPC startup failure")
	}
	if node.running.Load() {
		t.Fatal("node remained running after IPC startup failure")
	}
}

func TestIPCUnexpectedListenerFailureIsVisible(t *testing.T) {
	var output bytes.Buffer
	cfg := ipcTestConfig(t)
	node, err := New(cfg, WithLogger(slog.New(slog.NewJSONHandler(&output, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	if err := node.ipcListener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-node.stopping:
	case <-time.After(time.Second):
		t.Fatal("node did not stop after an unexpected IPC listener failure")
	}
	if loggedEvents(t, output.String())["ipc_server_failed"] != 1 {
		t.Fatalf("missing IPC server failure event:\n%s", output.String())
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
}
