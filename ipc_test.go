package ethertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func TestBoundedIPCFramesAdjacentJSONValuesWithoutNewlines(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	go func() {
		_, _ = client.Write([]byte(`{"text":"}"}[1,{"text":"["}]`))
		_ = client.Close()
	}()
	decoder := json.NewDecoder(newBoundedIPCConn(server, 128, 128))
	var first map[string]string
	if err := decoder.Decode(&first); err != nil || first["text"] != "}" {
		t.Fatalf("first adjacent IPC value = %#v, %v", first, err)
	}
	var second []any
	if err := decoder.Decode(&second); err != nil || len(second) != 2 {
		t.Fatalf("second adjacent IPC value = %#v, %v", second, err)
	}
}

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

func TestIPCRequestLimit(t *testing.T) {
	cfg := ipcTestConfig(t)
	cfg.Limits.MaxRequestBytes = 256
	cfg.Limits.MaxResponseBytes = 256
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client, err := rpc.DialIPC(t.Context(), node.Endpoints().IPC)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var chainID hexutil.Uint64
	if err := client.Call(&chainID, "eth_chainId"); err != nil {
		t.Fatal(err)
	}
	var output hexutil.Bytes
	if err := client.Call(&output, "web3_sha3", hexutil.Bytes(make([]byte, 512))); err == nil {
		t.Fatal("oversized IPC request was accepted")
	}
	client.Close()
	client, err = rpc.DialIPC(t.Context(), node.Endpoints().IPC)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var accounts []common.Address
	if err := client.Call(&accounts, "eth_accounts"); err == nil {
		t.Fatal("oversized IPC response was accepted")
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

func TestIPCRefusesFilesAndLiveSocketsButRemovesStaleSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket semantics")
	}
	t.Run("regular file", func(t *testing.T) {
		cfg := ipcTestConfig(t)
		endpoint := cfg.IPCEndpoint()
		if err := os.WriteFile(endpoint, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err == nil {
			t.Fatal("IPC replaced a regular file")
		}
		contents, err := os.ReadFile(endpoint)
		if err != nil || string(contents) != "keep" {
			t.Fatalf("regular file changed: %q %v", contents, err)
		}
	})
	t.Run("live socket", func(t *testing.T) {
		cfg := ipcTestConfig(t)
		endpoint := cfg.IPCEndpoint()
		listener, err := net.Listen("unix", endpoint)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close() //nolint:errcheck
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err == nil {
			t.Fatal("IPC replaced a live socket")
		}
		if _, err := os.Stat(endpoint); err != nil {
			t.Fatalf("live socket was removed: %v", err)
		}
	})
	t.Run("stale socket", func(t *testing.T) {
		cfg := ipcTestConfig(t)
		endpoint := cfg.IPCEndpoint()
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: endpoint, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err != nil {
			t.Fatalf("stale IPC socket was not replaced: %v", err)
		}
		if err := node.Close(); err != nil {
			t.Fatal(err)
		}
	})
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
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := node.Wait(waitCtx); err == nil {
		t.Fatal("Wait did not report unexpected IPC listener failure")
	}
	if loggedEvents(t, output.String())["ipc_server_failed"] != 1 {
		t.Fatalf("missing IPC server failure event:\n%s", output.String())
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
}
