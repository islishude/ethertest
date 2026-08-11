package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/big"
	"testing"
	"time"

	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

func TestWithdrawalQueueUpdatesPendingAndConsumesOnce(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	firstAddress := common.HexToAddress("0x1000000000000000000000000000000000000001")
	secondAddress := common.HexToAddress("0x2000000000000000000000000000000000000002")
	if err := node.AddWithdrawal(context.Background(), WithdrawalRequest{
		ValidatorIndex: 7, Address: firstAddress, Amount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := node.AddWithdrawal(context.Background(), WithdrawalRequest{
		ValidatorIndex: 8, Address: secondAddress, Amount: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if head := node.chain.blockchain.CurrentBlock().Number.Uint64(); head != 0 {
		t.Fatalf("add withdrawal mined block %d", head)
	}
	pending, pendingState, _ := node.chain.pendingSnapshot()
	assertWithdrawals(t, pending, []WithdrawalRequest{
		{ValidatorIndex: 7, Address: firstAddress, Amount: 2},
		{ValidatorIndex: 8, Address: secondAddress, Amount: 3},
	}, 0)
	wantPendingBalance := new(big.Int).Mul(big.NewInt(2), big.NewInt(params.GWei))
	if balance := pendingState.GetBalance(firstAddress).ToBig(); balance.Cmp(wantPendingBalance) != 0 {
		t.Fatalf("pending withdrawal balance = %s, want %s", balance, wantPendingBalance)
	}
	latestState, err := node.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if balance := latestState.GetBalance(firstAddress); !balance.IsZero() {
		t.Fatalf("latest balance changed before mining: %s", balance)
	}

	hashes, err := node.Mine(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByHash(hashes[0])
	assertWithdrawals(t, block, []WithdrawalRequest{
		{ValidatorIndex: 7, Address: firstAddress, Amount: 2},
		{ValidatorIndex: 8, Address: secondAddress, Amount: 3},
	}, 0)
	state, err := node.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if balance := state.GetBalance(firstAddress).ToBig(); balance.Cmp(wantPendingBalance) != 0 {
		t.Fatalf("canonical withdrawal balance = %s, want %s", balance, wantPendingBalance)
	}

	signed, err := node.consensus.signedBlock(node.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	if signed.electra == nil || signed.electra.Message == nil || signed.electra.Message.Body == nil ||
		signed.electra.Message.Body.ExecutionPayload == nil || len(signed.electra.Message.Body.ExecutionPayload.Withdrawals) != 2 {
		t.Fatal("Beacon projection is missing execution withdrawals")
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
	if len(jsonBlock.Message.Body.ExecutionPayload.Withdrawals) != 2 ||
		len(sszBlock.Message.Body.ExecutionPayload.Withdrawals) != 2 ||
		jsonBlock.Message.Body.ExecutionPayload.Withdrawals[1].Index != sszBlock.Message.Body.ExecutionPayload.Withdrawals[1].Index {
		t.Fatal("Beacon JSON and SSZ withdrawals diverged")
	}

	pending = node.chain.pendingBlock()
	if withdrawals := pending.Withdrawals(); withdrawals == nil || len(withdrawals) != 0 {
		t.Fatalf("next pending withdrawals = %#v, want empty non-nil list", withdrawals)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	if err := node.AddWithdrawal(context.Background(), WithdrawalRequest{
		ValidatorIndex: 9, Address: firstAddress, Amount: 4,
	}); err != nil {
		t.Fatal(err)
	}
	assertWithdrawals(t, node.chain.pendingBlock(), []WithdrawalRequest{
		{ValidatorIndex: 9, Address: firstAddress, Amount: 4},
	}, 2)
}

func TestAddWithdrawalRPCValidationAndNamespace(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	client := node.RPCClient()
	defer client.Close()

	address := common.HexToAddress("0x3000000000000000000000000000000000000003")
	valid := map[string]any{"validatorIndex": "0x0", "address": address, "amount": "0x1"}
	for _, invalid := range []map[string]any{
		{"index": "0x0", "validatorIndex": "0x0", "address": address, "amount": "0x1"},
		{"address": address, "amount": "0x1"},
		{"validatorIndex": "0x0", "amount": "0x1"},
		{"validatorIndex": "0x0", "address": address},
		{"validatorIndex": "0x0", "address": address, "amount": "0x0"},
	} {
		var result bool
		assertRPCErrorCode(t, client.Call(&result, "ethertest_addWithdrawal", invalid), -32602)
	}
	for index := range maxWithdrawalsPerBlock {
		args := maps.Clone(valid)
		args["validatorIndex"] = "0x" + new(big.Int).SetInt64(int64(index)).Text(16)
		var result bool
		if err := client.Call(&result, "ethertest_addWithdrawal", args); err != nil || !result {
			t.Fatalf("add withdrawal %d = %v, %v", index, result, err)
		}
	}
	if head := node.chain.blockchain.CurrentBlock().Number.Uint64(); head != 0 {
		t.Fatalf("transaction mining mode mined queued withdrawals at block %d", head)
	}
	var result bool
	assertRPCErrorCode(t, client.Call(&result, "ethertest_addWithdrawal", valid), -32602)
	if len(node.pendingWithdrawals) != maxWithdrawalsPerBlock || len(node.chain.pendingBlock().Withdrawals()) != maxWithdrawalsPerBlock {
		t.Fatal("queue limit failure changed the accepted withdrawals")
	}
	assertRPCErrorCode(t, client.Call(&result, "anvil_addWithdrawal", valid), -32601)
	assertRPCErrorCode(t, client.Call(&result, "evm_addWithdrawal", valid), -32601)

	if err := node.AddWithdrawal(context.Background(), WithdrawalRequest{Address: address}); !errors.Is(err, ErrWithdrawalAmountZero) {
		t.Fatalf("zero amount Go API error = %v", err)
	}
}

func TestWithdrawalQueueReorgControlAndCommitBoundaries(t *testing.T) {
	t.Run("reorg recomputes unconsumed index", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = "manual"
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
		request := WithdrawalRequest{ValidatorIndex: 1, Address: common.Address{1}, Amount: 1}
		if err := node.AddWithdrawal(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if _, err := node.Mine(context.Background(), 1, true); err != nil {
			t.Fatal(err)
		}
		if err := node.AddWithdrawal(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		assertWithdrawals(t, node.chain.pendingBlock(), []WithdrawalRequest{request}, 1)
		if err := node.SwitchBranch(context.Background(), "genesis"); err != nil {
			t.Fatal(err)
		}
		assertWithdrawals(t, node.chain.pendingBlock(), []WithdrawalRequest{request}, 0)
	})

	t.Run("control block consumes queue", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = "manual"
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err != nil {
			t.Fatal(err)
		}
		defer node.Close() //nolint:errcheck
		request := WithdrawalRequest{ValidatorIndex: 2, Address: common.Address{2}, Amount: 2}
		if err := node.AddWithdrawal(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		hash, err := node.ApplyControl(context.Background(), ControlChanges{
			common.Address{3}: {Nonce: new(uint64)},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertWithdrawals(t, node.chain.blockchain.GetBlockByHash(hash), []WithdrawalRequest{request}, 0)
		if valid, err := node.VerifyControlRecord(context.Background(), hash); err != nil || !valid {
			t.Fatalf("control record with withdrawal = %v, %v", valid, err)
		}
		if len(node.pendingWithdrawals) != 0 || len(node.chain.pendingBlock().Withdrawals()) != 0 {
			t.Fatal("control block did not consume withdrawal queue")
		}
	})

	t.Run("failed commit retains queue", func(t *testing.T) {
		cfg := testConfig()
		cfg.Mining.Mode = "manual"
		node, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := node.Start(); err != nil {
			t.Fatal(err)
		}
		defer node.Close() //nolint:errcheck
		request := WithdrawalRequest{ValidatorIndex: 3, Address: common.Address{3}, Amount: 3}
		if err := node.AddWithdrawal(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		node.commitHook = func(stage commitStage) error {
			if stage == commitStagePrepared {
				return errors.New("injected failure")
			}
			return nil
		}
		if _, err := node.Mine(context.Background(), 1, true); err == nil {
			t.Fatal("expected injected commit failure")
		}
		if len(node.pendingWithdrawals) != 1 {
			t.Fatal("failed commit consumed withdrawal queue")
		}
	})
}

func TestIntervalMinerTreatsWithdrawalAsPendingWork(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "interval"
	cfg.Mining.Interval = 20 * time.Millisecond
	cfg.Mining.AutoMineEmpty = false
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	request := WithdrawalRequest{ValidatorIndex: 4, Address: common.Address{4}, Amount: 4}
	if err := node.AddWithdrawal(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitForHead(t, node, 1)
	assertWithdrawals(t, node.chain.blockchain.GetBlockByNumber(1), []WithdrawalRequest{request}, 0)
}

func assertWithdrawals(t *testing.T, block interface{ Withdrawals() types.Withdrawals }, requests []WithdrawalRequest, firstIndex uint64) {
	t.Helper()
	withdrawals := block.Withdrawals()
	if len(withdrawals) != len(requests) {
		t.Fatalf("withdrawals = %#v, want %d entries", withdrawals, len(requests))
	}
	for index, request := range requests {
		withdrawal := withdrawals[index]
		if withdrawal.Index != firstIndex+uint64(index) || withdrawal.Validator != request.ValidatorIndex ||
			withdrawal.Address != request.Address || withdrawal.Amount != request.Amount {
			t.Fatalf("withdrawal %d = %#v, want index=%d request=%#v", index, withdrawal, firstIndex+uint64(index), request)
		}
	}
}
