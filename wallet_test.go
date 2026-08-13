package ethertest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	gethaccounts "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/holiman/uint256"
)

func TestMemoryWalletOrderingOwnershipAndRemoval(t *testing.T) {
	configured, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := newMemoryWallet(configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := wallet.accounts(); !slices.Equal(got, []common.Address{configured[0].Address, configured[1].Address}) {
		t.Fatalf("configured order = %v", got)
	}
	originalScalar := new(big.Int).Set(configured[0].PrivateKey.D)
	configured[0].PrivateKey.D.SetInt64(1)
	owned := testAccountFromWallet(t, wallet, configured[0].Address)
	if owned.PrivateKey.D.Cmp(originalScalar) != 0 {
		t.Fatal("wallet retained the caller-owned private key")
	}
	if removed, err := wallet.remove(configured[0].Address); !errors.Is(err, errConfiguredAccount) || removed {
		t.Fatalf("configured removal = %v, %v", removed, err)
	}

	importedKey := mustGenerateKey(t)
	imported := Account{Address: crypto.PubkeyToAddress(importedKey.PublicKey), PrivateKey: importedKey}
	if err := wallet.add(imported); err != nil {
		t.Fatal(err)
	}
	if err := wallet.add(imported); !errors.Is(err, errAccountAlreadyManaged) {
		t.Fatalf("duplicate import error = %v", err)
	}
	importedKey.D.SetInt64(1)
	owned = testAccountFromWallet(t, wallet, imported.Address)
	if owned.PrivateKey.D.Cmp(big.NewInt(1)) == 0 {
		t.Fatal("imported key was not cloned")
	}
	if removed, err := wallet.remove(common.HexToAddress("0xdead")); err != nil || removed {
		t.Fatalf("unknown removal = %v, %v", removed, err)
	}
	if removed, err := wallet.remove(imported.Address); err != nil || !removed {
		t.Fatalf("imported removal = %v, %v", removed, err)
	}
	if wallet.contains(imported.Address) {
		t.Fatal("removed account remains managed")
	}
}

func TestMemoryWalletConcurrentAccess(t *testing.T) {
	configured, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	wallet, err := newMemoryWallet(configured)
	if err != nil {
		t.Fatal(err)
	}
	key := mustGenerateKey(t)
	account := Account{Address: crypto.PubkeyToAddress(key.PublicKey), PrivateKey: key}
	hash := crypto.Keccak256([]byte("wallet race test"))
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			for range 100 {
				_ = wallet.accounts()
				_ = wallet.contains(account.Address)
				if _, err := wallet.signHash(configured[0].Address, hash); err != nil {
					t.Errorf("configured signing failed: %v", err)
					return
				}
			}
		})
	}
	wait.Go(func() {
		for range 100 {
			if err := wallet.add(account); err != nil {
				t.Errorf("concurrent add failed: %v", err)
				return
			}
			if removed, err := wallet.remove(account.Address); err != nil || !removed {
				t.Errorf("concurrent removal = %v, %v", removed, err)
				return
			}
		}
	})
	wait.Go(func() {
		authorization := types.SetCodeAuthorization{
			ChainID: *uint256.NewInt(1337),
			Address: configured[1].Address,
			Nonce:   1,
		}
		for range 100 {
			signed, err := wallet.signAuthorization(account.Address, authorization)
			if errors.Is(err, errUnknownUnlockedAccount) {
				continue
			}
			if err != nil {
				t.Errorf("concurrent authorization signing failed unexpectedly: %v", err)
				return
			}
			authority, err := signed.Authority()
			if err != nil || authority != account.Address {
				t.Errorf("concurrent authorization authority = %s, %v", authority, err)
				return
			}
		}
	})
	wait.Wait()
}

func TestSignAuthorizationSemantics(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.ChainID = 1337
	node := startRPCNode(t, cfg)
	authority := node.Accounts()[0]
	target := node.Accounts()[1]
	head := node.chain.blockchain.CurrentBlock().Hash()
	revision := node.Revision()

	signed, err := node.SignAuthorization(authority, AuthorizationRequest{
		ChainID: big.NewInt(1337),
		Address: target,
		Nonce:   42,
	})
	if err != nil {
		t.Fatal(err)
	}
	if signed.ChainID.ToBig().Cmp(big.NewInt(1337)) != 0 || signed.Address != target || signed.Nonce != 42 || signed.V > 1 {
		t.Fatalf("signed authorization = %#v", signed)
	}
	recovered, err := signed.Authority()
	if err != nil || recovered != authority {
		t.Fatalf("authorization authority = %s, %v", recovered, err)
	}
	if node.chain.blockchain.CurrentBlock().Hash() != head || node.Revision() != revision {
		t.Fatal("authorization signing changed canonical state or revision")
	}

	replayable, err := node.SignAuthorization(authority, AuthorizationRequest{
		ChainID: new(big.Int),
		Address: common.Address{},
		Nonce:   math.MaxUint64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayable.ChainID.IsZero() || replayable.Address != (common.Address{}) || replayable.Nonce != math.MaxUint64 {
		t.Fatalf("replayable clearing authorization = %#v", replayable)
	}
	if recovered, err := replayable.Authority(); err != nil || recovered != authority {
		t.Fatalf("replayable authorization authority = %s, %v", recovered, err)
	}

	invalid := []struct {
		name    string
		chainID *big.Int
		want    error
	}{
		{name: "missing", want: errAuthorizationChainIDRequired},
		{name: "negative", chainID: big.NewInt(-1), want: errAuthorizationChainIDNegative},
		{name: "overflow", chainID: new(big.Int).Lsh(big.NewInt(1), 256), want: errAuthorizationChainIDOverflow},
		{name: "mismatch", chainID: big.NewInt(1), want: errAuthorizationChainIDMismatch},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, err := node.SignAuthorization(authority, AuthorizationRequest{ChainID: test.chainID, Address: target})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	if _, err := node.SignAuthorization(common.HexToAddress("0xdead"), AuthorizationRequest{ChainID: big.NewInt(1337), Address: target}); !errors.Is(err, errUnknownUnlockedAccount) {
		t.Fatalf("unknown authority error = %v", err)
	}

	key := mustGenerateKey(t)
	imported := crypto.PubkeyToAddress(key.PublicKey)
	if _, err := node.ImportAccount(context.Background(), key, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := node.SignAuthorization(imported, AuthorizationRequest{ChainID: big.NewInt(1337), Address: target}); err != nil {
		t.Fatalf("runtime authorization signing failed: %v", err)
	}
	if removed, err := node.RemoveAccount(context.Background(), imported); err != nil || !removed {
		t.Fatalf("runtime removal = %v, %v", removed, err)
	}
	if _, err := node.SignAuthorization(imported, AuthorizationRequest{ChainID: big.NewInt(1337), Address: target}); !errors.Is(err, errUnknownUnlockedAccount) {
		t.Fatalf("removed authority error = %v", err)
	}
}

func TestSignAuthorizationRPC(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.ChainID = 1337
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	authority := node.Accounts()[0]
	target := node.Accounts()[1]
	args := map[string]any{
		"chainId": "0x539",
		"address": target,
		"nonce":   "0x2a",
	}

	var raw json.RawMessage
	if err := client.Call(&raw, "ethertest_signAuthorization", authority, args); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{"address", "chainId", "nonce", "r", "s", "yParity"}
	if len(fields) != len(wantFields) {
		t.Fatalf("authorization fields = %v", fields)
	}
	for _, name := range wantFields {
		if _, exists := fields[name]; !exists {
			t.Fatalf("authorization is missing %q: %s", name, raw)
		}
	}
	var signed types.SetCodeAuthorization
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	if signed.Address != target || signed.Nonce != 42 || signed.ChainID.ToBig().Cmp(big.NewInt(1337)) != 0 {
		t.Fatalf("RPC authorization = %#v", signed)
	}
	if recovered, err := signed.Authority(); err != nil || recovered != authority {
		t.Fatalf("RPC authorization authority = %s, %v", recovered, err)
	}
	clearingArgs := map[string]any{
		"chainId": "0x0",
		"address": common.Address{},
		"nonce":   "0xffffffffffffffff",
	}
	if err := client.Call(&signed, "ethertest_signAuthorization", authority, clearingArgs); err != nil {
		t.Fatal(err)
	}
	if !signed.ChainID.IsZero() || signed.Address != (common.Address{}) || signed.Nonce != math.MaxUint64 {
		t.Fatalf("RPC clearing authorization = %#v", signed)
	}

	var capabilities map[string]any
	if err := client.Call(&capabilities, "ethertest_capabilities"); err != nil {
		t.Fatal(err)
	}
	if capabilities["authorizationSigning"] != true {
		t.Fatalf("authorization signing capability = %#v", capabilities["authorizationSigning"])
	}

	invalid := []map[string]any{
		{"address": target, "nonce": "0x0"},
		{"chainId": "0x539", "nonce": "0x0"},
		{"chainId": "0x539", "address": target},
		{"chainId": "0x1", "address": target, "nonce": "0x0"},
		{"chainId": "0x1" + strings.Repeat("0", 64), "address": target, "nonce": "0x0"},
		{"chainId": "0x539", "address": target, "nonce": "0x00"},
	}
	for _, input := range invalid {
		assertRPCErrorCode(t, client.Call(&raw, "ethertest_signAuthorization", authority, input), -32602)
	}
	assertRPCErrorCode(t, client.Call(&raw, "ethertest_signAuthorization", authority, args, true), -32602)
	if err := client.Call(&raw, "ethertest_signAuthorization", common.HexToAddress("0xdead"), args); !strings.Contains(errorString(err), errUnknownUnlockedAccount.Error()) {
		t.Fatalf("unknown RPC authority error = %v", err)
	}
	for _, method := range []string{"eth_signAuthorization", "anvil_signAuthorization", "evm_signAuthorization"} {
		assertRPCErrorCode(t, client.Call(&raw, method, authority, args), -32601)
	}
}

func TestImportAccountLifecycleAndPersistenceBoundary(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = miningModeManual
	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "chain")
	key := mustGenerateKey(t)
	address := crypto.PubkeyToAddress(key.PublicKey)
	balance := big.NewInt(424242)

	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	result, err := first.ImportAccount(context.Background(), key, balance)
	if err != nil {
		t.Fatal(err)
	}
	if result.Address != address || result.ControlBlockHash == nil {
		t.Fatalf("import result = %#v", result)
	}
	if !first.SafetyStatus().SessionTainted {
		t.Fatal("funded import did not taint the session")
	}
	state, err := first.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.GetBalance(address).ToBig(); got.Cmp(balance) != 0 {
		t.Fatalf("imported balance = %s", got)
	}
	if _, err := first.ImportAccount(context.Background(), key, big.NewInt(1)); !errors.Is(err, errAccountAlreadyManaged) {
		t.Fatalf("duplicate import error = %v", err)
	}
	if got := first.chain.blockchain.CurrentBlock().Number.Uint64(); got != 1 {
		t.Fatalf("duplicate import changed head to %d", got)
	}
	state, err = first.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.GetBalance(address).ToBig(); got.Cmp(balance) != 0 {
		t.Fatalf("duplicate import changed balance to %s", got)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(second.Accounts(), address) {
		t.Fatal("runtime account survived restart")
	}
	state, err = second.chain.blockchain.State()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.GetBalance(address).ToBig(); got.Cmp(balance) != 0 {
		t.Fatalf("persisted balance = %s", got)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestImportAccountValidationAndSnapshotIndependence(t *testing.T) {
	node := startRPCNode(t, testConfig())
	if _, err := node.ImportAccount(context.Background(), nil, nil); err == nil {
		t.Fatal("nil private key accepted")
	}
	key := mustGenerateKey(t)
	address := crypto.PubkeyToAddress(key.PublicKey)
	if _, err := node.ImportAccount(context.Background(), key, big.NewInt(-1)); err == nil {
		t.Fatal("negative balance accepted")
	}
	overflow := new(big.Int).Lsh(big.NewInt(1), 256)
	if _, err := node.ImportAccount(context.Background(), key, overflow); err == nil {
		t.Fatal("uint256 overflow accepted")
	}
	snapshot, err := node.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := node.ImportAccount(context.Background(), key, nil)
	if err != nil || result.ControlBlockHash != nil {
		t.Fatalf("unfunded import = %#v, %v", result, err)
	}
	if got := node.chain.blockchain.CurrentBlock().Number.Uint64(); got != 0 {
		t.Fatalf("unfunded import changed head to %d", got)
	}
	if ok, err := node.Revert(context.Background(), snapshot); err != nil || !ok {
		t.Fatalf("snapshot revert = %v, %v", ok, err)
	}
	if !node.wallet.contains(address) {
		t.Fatal("snapshot revert removed runtime wallet membership")
	}
	if removed, err := node.RemoveAccount(context.Background(), node.Accounts()[0]); !errors.Is(err, errConfiguredAccount) || removed {
		t.Fatalf("configured removal = %v, %v", removed, err)
	}
	if removed, err := node.RemoveAccount(context.Background(), address); err != nil || !removed {
		t.Fatalf("runtime removal = %v, %v", removed, err)
	}
	if removed, err := node.RemoveAccount(context.Background(), address); err != nil || removed {
		t.Fatalf("second removal = %v, %v", removed, err)
	}
}

func TestWalletManagementRPCAndImportedSigning(t *testing.T) {
	cfg := testConfig()
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	key := mustGenerateKey(t)
	encodedKey := hexutil.Encode(crypto.FromECDSA(key))
	address := crypto.PubkeyToAddress(key.PublicKey)
	balance := (*hexutil.Big)(new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18)))

	var result ImportAccountResult
	if err := client.Call(&result, "ethertest_importAccount", encodedKey, balance); err != nil {
		t.Fatal(err)
	}
	if result.Address != address || result.ControlBlockHash == nil {
		t.Fatalf("RPC import result = %#v", result)
	}
	var rpcBalance hexutil.Big
	if err := client.Call(&rpcBalance, "eth_getBalance", address, "latest"); err != nil || (*big.Int)(&rpcBalance).Cmp((*big.Int)(balance)) != 0 {
		t.Fatalf("RPC balance = %s, %v", (*big.Int)(&rpcBalance), err)
	}
	var listed []common.Address
	if err := client.Call(&listed, "eth_accounts"); err != nil || !slices.Contains(listed, address) {
		t.Fatalf("eth_accounts = %v, %v", listed, err)
	}
	if err := client.Call(&listed, "personal_listAccounts"); err != nil || !slices.Contains(listed, address) {
		t.Fatalf("personal_listAccounts = %v, %v", listed, err)
	}
	var unlocked bool
	if err := client.Call(&unlocked, "personal_unlockAccount", address, ""); err != nil || !unlocked {
		t.Fatalf("personal_unlockAccount = %v, %v", unlocked, err)
	}

	message := hexutil.Bytes("runtime signer")
	var signature hexutil.Bytes
	if err := client.Call(&signature, "eth_sign", address, message); err != nil {
		t.Fatal(err)
	}
	recovery := append([]byte(nil), signature...)
	recovery[crypto.RecoveryIDOffset] -= 27
	publicKey, err := crypto.SigToPub(gethaccounts.TextHash(message), recovery)
	if err != nil || crypto.PubkeyToAddress(*publicKey) != address {
		t.Fatalf("imported eth_sign recovery failed: %v", err)
	}
	typedPayload := json.RawMessage(`{"types":{"EIP712Domain":[{"name":"chainId","type":"uint256"}],"Message":[{"name":"value","type":"string"}]},"primaryType":"Message","domain":{"chainId":1337},"message":{"value":"runtime"}}`)
	if err := client.Call(&signature, "eth_signTypedData_v4", address, typedPayload); err != nil {
		t.Fatalf("imported typed-data signing failed: %v", err)
	}
	var authorization types.SetCodeAuthorization
	authorizationArgs := map[string]any{
		"chainId": hexutil.EncodeUint64(cfg.Chain.ChainID),
		"address": node.Accounts()[0],
		"nonce":   "0x0",
	}
	if err := client.Call(&authorization, "ethertest_signAuthorization", address, authorizationArgs); err != nil {
		t.Fatalf("imported authorization signing failed: %v", err)
	}
	if recovered, err := authorization.Authority(); err != nil || recovered != address {
		t.Fatalf("imported authorization authority = %s, %v", recovered, err)
	}

	txArgs := map[string]any{
		"type": "0x2", "from": address, "to": node.Accounts()[0], "nonce": "0x0", "gas": "0x5208",
		"maxFeePerGas": "0xb2d05e00", "maxPriorityFeePerGas": "0x3b9aca00", "value": "0x1",
		"chainId": hexutil.Uint64(cfg.Chain.ChainID),
	}
	var raw hexutil.Bytes
	if err := client.Call(&raw, "eth_signTransaction", txArgs); err != nil {
		t.Fatal(err)
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(raw); err != nil {
		t.Fatal(err)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(new(big.Int).SetUint64(cfg.Chain.ChainID)), &signed)
	if err != nil || sender != address {
		t.Fatalf("signed sender = %s, %v", sender, err)
	}
	var hash common.Hash
	if err := client.Call(&hash, "eth_sendTransaction", txArgs); err != nil || hash == (common.Hash{}) {
		t.Fatalf("eth_sendTransaction = %s, %v", hash, err)
	}

	if err := client.Call(&result, "ethertest_importAccount", encodedKey); !strings.Contains(errorString(err), errAccountAlreadyManaged.Error()) {
		t.Fatalf("duplicate RPC import error = %v", err)
	}
	for _, invalid := range []string{"11", "0x11", "0x" + strings.Repeat("00", 32)} {
		if err := client.Call(&result, "ethertest_importAccount", invalid); err == nil || strings.Contains(err.Error(), invalid) {
			t.Fatalf("invalid private key error = %v", err)
		}
	}
	var removed bool
	if err := client.Call(&removed, "ethertest_removeAccount", node.Accounts()[0]); !strings.Contains(errorString(err), errConfiguredAccount.Error()) {
		t.Fatalf("configured RPC removal error = %v", err)
	}
	if err := client.Call(&removed, "ethertest_removeAccount", address); err != nil || !removed {
		t.Fatalf("runtime RPC removal = %v, %v", removed, err)
	}
	if err := client.Call(&listed, "personal_listAccounts"); err != nil || slices.Contains(listed, address) {
		t.Fatalf("accounts after removal = %v, %v", listed, err)
	}
	if err := client.Call(&unlocked, "personal_unlockAccount", address, ""); err != nil || unlocked {
		t.Fatalf("unlock after removal = %v, %v", unlocked, err)
	}
	if err := client.Call(&rpcBalance, "eth_getBalance", address, "latest"); err != nil || (*big.Int)(&rpcBalance).Sign() <= 0 {
		t.Fatalf("state after wallet removal = %s, %v", (*big.Int)(&rpcBalance), err)
	}
	if err := client.Call(&signature, "eth_sign", address, message); !strings.Contains(errorString(err), errUnknownUnlockedAccount.Error()) {
		t.Fatalf("removed signer error = %v", err)
	}
	if err := client.Call(&authorization, "ethertest_signAuthorization", address, authorizationArgs); !strings.Contains(errorString(err), errUnknownUnlockedAccount.Error()) {
		t.Fatalf("removed authorization signer error = %v", err)
	}
	for _, method := range []string{"anvil_importAccount", "evm_importAccount", "personal_importAccount"} {
		var ignored json.RawMessage
		assertRPCErrorCode(t, client.Call(&ignored, method), -32601)
	}
}

func TestSignTypedDataV4Compatibility(t *testing.T) {
	cfg := testConfig()
	cfg.Chain.ChainID = 1
	node := startRPCNode(t, cfg)
	client := node.RPCClient()
	defer client.Close()
	address := node.Accounts()[0]
	payload := json.RawMessage(`{
		"types": {
			"EIP712Domain": [{"name":"name","type":"string"},{"name":"version","type":"string"},{"name":"chainId","type":"uint256"},{"name":"verifyingContract","type":"address"}],
			"Person": [{"name":"name","type":"string"},{"name":"wallet","type":"address"}],
			"Mail": [{"name":"from","type":"Person"},{"name":"to","type":"Person"},{"name":"contents","type":"string"}]
		},
		"primaryType": "Mail",
		"domain": {"name":"Ether Mail","version":"1","chainId":1,"verifyingContract":"0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"},
		"message": {"from":{"name":"Cow","wallet":"0xCD2a3d9F938E13CD947Ec05AbC7FE734Df8DD826"},"to":{"name":"Bob","wallet":"0xbBbBBBBbbBBBbbbBbbBbbbbBBbBbbbbBbBbbBBbB"},"contents":"Hello, Bob!"}
	}`)
	typedData, err := decodeTypedData(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		t.Fatal(err)
	}
	if got := common.BytesToHash(digest); got != common.HexToHash("0xbe609aee343fb3c4b28e1df9e632fca64fcfaede20f02e86244efddf30957bd2") {
		t.Fatalf("official EIP-712 digest = %s", got)
	}

	var objectSignature hexutil.Bytes
	if err := client.Call(&objectSignature, "eth_signTypedData_v4", address, payload); err != nil {
		t.Fatal(err)
	}
	var stringSignature hexutil.Bytes
	if err := client.Call(&stringSignature, "eth_signTypedData_v4", address, string(payload)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(objectSignature, stringSignature) || len(objectSignature) != crypto.SignatureLength || objectSignature[crypto.RecoveryIDOffset] < 27 {
		t.Fatalf("typed signatures differ or are malformed: %x / %x", objectSignature, stringSignature)
	}
	recovery := append([]byte(nil), objectSignature...)
	recovery[crypto.RecoveryIDOffset] -= 27
	publicKey, err := crypto.SigToPub(digest, recovery)
	if err != nil || crypto.PubkeyToAddress(*publicKey) != address {
		t.Fatalf("typed signature recovery failed: %v", err)
	}

	mismatch := strings.Replace(string(payload), `"chainId":1`, `"chainId":2`, 1)
	if err := client.Call(&objectSignature, "eth_signTypedData_v4", address, mismatch); !strings.Contains(errorString(err), "chainId does not match") {
		t.Fatalf("mismatched chainId error = %v", err)
	}
	withoutChainID := json.RawMessage(`{"types":{"EIP712Domain":[{"name":"name","type":"string"}],"Message":[{"name":"value","type":"string"}]},"primaryType":"Message","domain":{"name":"ethertest"},"message":{"value":"ok"}}`)
	if err := client.Call(&objectSignature, "eth_signTypedData_v4", address, withoutChainID); err != nil {
		t.Fatalf("chainId-free typed data failed: %v", err)
	}
	if err := client.Call(&objectSignature, "eth_signTypedData_v4", common.HexToAddress("0xdead"), payload); !strings.Contains(errorString(err), errUnknownUnlockedAccount.Error()) {
		t.Fatalf("unknown typed-data signer error = %v", err)
	}
	if err := client.Call(&objectSignature, "eth_signTypedData_v4", address, "{"); !strings.Contains(errorString(err), "invalid typed data") {
		t.Fatalf("invalid typed data error = %v", err)
	}
	var ignored json.RawMessage
	assertRPCErrorCode(t, client.Call(&ignored, "eth_signTypedData"), -32601)
	assertRPCErrorCode(t, client.Call(&ignored, "eth_signTypedDataV4"), -32601)
}

func TestRuntimeWalletSecretsStayOutOfLogsAndArchives(t *testing.T) {
	var logs bytes.Buffer
	cfg := testConfig()
	node, err := New(cfg, WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	key := mustGenerateKey(t)
	rawKey := crypto.FromECDSA(key)
	if _, err := node.ImportAccount(context.Background(), key, big.NewInt(424242)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), hexutil.Encode(rawKey)) || strings.Contains(logs.String(), "424242") {
		t.Fatalf("runtime account secret or balance leaked to logs: %s", logs.String())
	}
	path := filepath.Join(t.TempDir(), "state.tar.zst")
	if err := node.DumpState(path); err != nil {
		t.Fatal(err)
	}
	manifest, database, err := readArchive(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Secrets || bytes.Contains(database, rawKey) {
		t.Fatal("runtime private key leaked to state archive")
	}
}

func mustGenerateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
