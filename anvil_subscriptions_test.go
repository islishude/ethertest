package ethertest

import (
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

func receiveCompat[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("notification timeout")
		var zero T
		return zero
	}
}

func TestAnvilSubscriptions(t *testing.T) {
	for _, transport := range []string{"inproc", "ws", "ipc"} {
		t.Run(transport, func(t *testing.T) {
			cfg := testConfig()
			cfg.Mining.Mode = miningModeManual
			if transport == "ipc" {
				cfg = ipcTestConfig(t)
			}
			if transport == "ws" {
				cfg.HTTP.Enabled = true
				cfg.HTTP.Address = "127.0.0.1:0"
			}
			node := compatNode(t, cfg)
			client := node.RPCClient()
			if transport != "inproc" {
				client.Close()
				var err error
				if transport == "ws" {
					client, err = rpc.DialWebsocket(t.Context(), strings.Replace(node.Endpoints().Execution, "http://", "ws://", 1), "")
				} else {
					client, err = rpc.DialIPC(t.Context(), node.Endpoints().IPC)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			defer client.Close()
			account := testWalletAccount(t, node, 0)
			contract := common.HexToAddress("0x1234")
			code := []byte{0x60, 0, 0x60, 0, 0xa0, 0x00} // LOG0(empty)
			if _, err := node.ApplyControl(t.Context(), ControlChanges{contract: {Code: &code}}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := node.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			logs := make(chan *types.Log, 8)
			hashes := make(chan common.Hash, 8)
			transactions := make(chan *rpcTransaction, 8)
			sub, err := client.EthSubscribe(t.Context(), logs, "logs", map[string]any{"address": contract})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Unsubscribe()
			pending, err := client.EthSubscribe(t.Context(), hashes, "newPendingTransactions")
			if err != nil {
				t.Fatal(err)
			}
			defer pending.Unsubscribe()
			full, err := client.EthSubscribe(t.Context(), transactions, "newPendingTransactions", true)
			if err != nil {
				t.Fatal(err)
			}
			defer full.Unsubscribe()
			tx := signedDynamicTransaction(t, cfg, account, 0, contract, big.NewInt(0), nil)
			if _, err := node.SendTransaction(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			if err := node.DropAllTransactions(t.Context()); err != nil {
				t.Fatal(err)
			}
			if receiveCompat(t, hashes) != tx.Hash() {
				t.Fatal("pending hash")
			}
			result := receiveCompat(t, transactions)
			if result.Hash != tx.Hash() || result.From != account.Address || result.BlockHash != nil || result.BlockNumber != nil || result.TransactionIndex != nil {
				t.Fatal("pending transaction snapshot")
			}
			// The same accepted transaction can be submitted again after removal.
			if _, err := node.SendTransaction(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			if _, err := node.Mine(t.Context(), 1, false); err != nil {
				t.Fatal(err)
			}
			first := receiveCompat(t, logs)
			if first.Removed || first.TxHash != tx.Hash() {
				t.Fatal("canonical log")
			}
			if ok, err := node.Revert(t.Context(), snapshot); err != nil || !ok {
				t.Fatal("revert", err)
			}
			if _, err := node.SendTransaction(t.Context(), tx); err != nil {
				t.Fatal(err)
			}
			if _, err := node.Mine(t.Context(), 1, false); err != nil {
				t.Fatal(err)
			}
			removed := receiveCompat(t, logs)
			added := receiveCompat(t, logs)
			if !removed.Removed || removed.BlockHash != first.BlockHash || added.Removed {
				t.Fatal("reorg notification ordering")
			}
		})
	}
}

// A batch does not activate its notifier until every call completes. With a
// one-element queue, a burst in that batch must disconnect rather than collect
// unbounded notifications inside geth's notifier.
func TestSubscriptionOverflowClosesOnlyAffectedConnection(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Events.Capacity = 1
	node := compatNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	healthy := node.RPCClient()
	defer healthy.Close()
	watch := make(chan *types.Header, 64)
	watched, watchErr := client.EthSubscribe(t.Context(), watch, "newHeads")
	if watchErr != nil {
		t.Fatal(watchErr)
	}
	defer watched.Unsubscribe()
	var subID string
	var blocks []common.Hash
	batch := []rpc.BatchElem{
		{Method: "eth_subscribe", Args: []any{"newHeads"}, Result: &subID},
		{Method: "ethertest_mine", Args: []any{"0x10"}, Result: &blocks},
	}
	err := client.BatchCallContext(t.Context(), batch)
	for _, elem := range batch {
		if elem.Error != nil {
			t.Logf("batch %s: %v", elem.Method, elem.Error)
		}
	}
	// Calls may transparently reconnect; the existing subscription must end.
	_ = err
	select {
	case <-watched.Err():
	case <-time.After(3 * time.Second):
		t.Fatal("overflow connection remained open")
	}

	compatCall(t, healthy, "eth_chainId")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		node.subscriptionMu.Lock()
		active := node.activeSubscriptions
		node.subscriptionMu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("subscription capacity leaked")
}

func TestPendingEventGapAndImmutableTransaction(t *testing.T) {
	log := newPendingHashLog(1)
	log.record(common.HexToHash("0x1"), json.RawMessage(`{"blockHash":null}`))
	payload := json.RawMessage(`{"blockHash":null}`)
	log.record(common.HexToHash("0x2"), payload)
	payload[0] = '!'
	if _, _, err := log.sinceAndWait(0); err != ErrEventGap {
		t.Fatal("missing pending gap", err)
	}
	events, _, err := log.sinceAndWait(1)
	if err != nil || len(events) != 1 {
		t.Fatal(err)
	}
	data, err := json.Marshal(events[0].Transaction)
	if err != nil || !strings.Contains(string(data), `"blockHash":null`) {
		t.Fatal("pending shape", err)
	}
}
