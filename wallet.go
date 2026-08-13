package ethertest

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"math/big"
	"sync"

	gethaccounts "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

var (
	errUnknownUnlockedAccount       = errors.New("unknown unlocked account")
	errAccountAlreadyManaged        = errors.New("account is already managed")
	errConfiguredAccount            = errors.New("configured account cannot be removed")
	errAuthorizationChainIDRequired = errors.New("authorization chainId is required")
	errAuthorizationChainIDNegative = errors.New("authorization chainId cannot be negative")
	errAuthorizationChainIDOverflow = errors.New("authorization chainId exceeds uint256")
	errAuthorizationChainIDMismatch = errors.New("authorization chainId does not match node")
)

type walletEntry struct {
	account   Account
	protected bool
}

// memoryWallet owns all signing keys for a Node. Configured accounts are
// protected from runtime removal; imported accounts live only until shutdown.
type memoryWallet struct {
	mu      sync.RWMutex
	order   []common.Address
	entries map[common.Address]walletEntry
}

func newMemoryWallet(configured []Account) (*memoryWallet, error) {
	wallet := &memoryWallet{entries: make(map[common.Address]walletEntry, len(configured))}
	for _, account := range configured {
		cloned, err := cloneAccount(account)
		if err != nil {
			return nil, err
		}
		if _, exists := wallet.entries[cloned.Address]; exists {
			return nil, errAccountAlreadyManaged
		}
		wallet.entries[cloned.Address] = walletEntry{account: cloned, protected: true}
		wallet.order = append(wallet.order, cloned.Address)
	}
	return wallet, nil
}

func cloneAccount(account Account) (Account, error) {
	privateKey, err := clonePrivateKey(account.PrivateKey)
	if err != nil {
		return Account{}, err
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey)
	if account.Address != (common.Address{}) && account.Address != address {
		return Account{}, errors.New("private key does not match account address")
	}
	return Account{Address: address, PrivateKey: privateKey, Path: account.Path}, nil
}

func clonePrivateKey(privateKey *ecdsa.PrivateKey) (*ecdsa.PrivateKey, error) {
	if privateKey == nil || privateKey.D == nil || privateKey.Curve != crypto.S256() ||
		privateKey.D.Sign() <= 0 || privateKey.D.Cmp(crypto.S256().Params().N) >= 0 {
		return nil, errors.New("invalid private key")
	}
	cloned, err := crypto.ToECDSA(crypto.FromECDSA(privateKey))
	if err != nil {
		return nil, errors.New("invalid private key")
	}
	return cloned, nil
}

func (wallet *memoryWallet) accounts() []common.Address {
	wallet.mu.RLock()
	defer wallet.mu.RUnlock()
	return append([]common.Address(nil), wallet.order...)
}

func (wallet *memoryWallet) contains(address common.Address) bool {
	wallet.mu.RLock()
	defer wallet.mu.RUnlock()
	_, exists := wallet.entries[address]
	return exists
}

func (wallet *memoryWallet) add(account Account) error {
	cloned, err := cloneAccount(account)
	if err != nil {
		return err
	}
	wallet.mu.Lock()
	defer wallet.mu.Unlock()
	if _, exists := wallet.entries[cloned.Address]; exists {
		return errAccountAlreadyManaged
	}
	wallet.entries[cloned.Address] = walletEntry{account: cloned}
	wallet.order = append(wallet.order, cloned.Address)
	return nil
}

func (wallet *memoryWallet) remove(address common.Address) (bool, error) {
	wallet.mu.Lock()
	defer wallet.mu.Unlock()
	entry, exists := wallet.entries[address]
	if !exists {
		return false, nil
	}
	if entry.protected {
		return false, errConfiguredAccount
	}
	delete(wallet.entries, address)
	for index, current := range wallet.order {
		if current == address {
			wallet.order = append(wallet.order[:index], wallet.order[index+1:]...)
			break
		}
	}
	return true, nil
}

func (wallet *memoryWallet) signText(address common.Address, message []byte) ([]byte, error) {
	return wallet.signHash(address, gethaccounts.TextHash(message))
}

func (wallet *memoryWallet) signHash(address common.Address, hash []byte) ([]byte, error) {
	wallet.mu.RLock()
	defer wallet.mu.RUnlock()
	entry, exists := wallet.entries[address]
	if !exists {
		return nil, errUnknownUnlockedAccount
	}
	return crypto.Sign(hash, entry.account.PrivateKey)
}

func (wallet *memoryWallet) signTransaction(address common.Address, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	wallet.mu.RLock()
	defer wallet.mu.RUnlock()
	entry, exists := wallet.entries[address]
	if !exists {
		return nil, errUnknownUnlockedAccount
	}
	return types.SignTx(tx, types.LatestSignerForChainID(chainID), entry.account.PrivateKey)
}

func (wallet *memoryWallet) signAuthorization(address common.Address, authorization types.SetCodeAuthorization) (types.SetCodeAuthorization, error) {
	wallet.mu.RLock()
	defer wallet.mu.RUnlock()
	entry, exists := wallet.entries[address]
	if !exists {
		return types.SetCodeAuthorization{}, errUnknownUnlockedAccount
	}
	return types.SignSetCode(entry.account.PrivateKey, authorization)
}

// AuthorizationRequest is the exact EIP-7702 tuple to sign. Address is the
// delegation target, including the zero address used to clear a delegation.
// Callers are responsible for selecting the nonce; signing does not read or
// mutate execution state.
type AuthorizationRequest struct {
	ChainID *big.Int
	Address common.Address
	Nonce   uint64
}

// SignAuthorization signs an EIP-7702 authorization with a configured or
// runtime-imported account. ChainID must be the node chain ID or zero, the
// replayable cross-chain value allowed by EIP-7702.
func (n *Node) SignAuthorization(authority common.Address, request AuthorizationRequest) (types.SetCodeAuthorization, error) {
	if request.ChainID == nil {
		return types.SetCodeAuthorization{}, errAuthorizationChainIDRequired
	}
	chainID := new(big.Int).Set(request.ChainID)
	if chainID.Sign() < 0 {
		return types.SetCodeAuthorization{}, errAuthorizationChainIDNegative
	}
	encodedChainID, overflow := uint256.FromBig(chainID)
	if overflow {
		return types.SetCodeAuthorization{}, errAuthorizationChainIDOverflow
	}
	if chainID.Sign() != 0 && chainID.Cmp(n.chain.config.ChainID) != 0 {
		return types.SetCodeAuthorization{}, errAuthorizationChainIDMismatch
	}
	return n.wallet.signAuthorization(authority, types.SetCodeAuthorization{
		ChainID: *encodedChainID,
		Address: request.Address,
		Nonce:   request.Nonce,
	})
}

// ImportAccountResult describes both the new signer and the optional unsafe
// control block created to set its initial balance.
type ImportAccountResult struct {
	Address          common.Address `json:"address"`
	ControlBlockHash *common.Hash   `json:"controlBlockHash"`
}

// ImportAccount adds a runtime-only signing key. When balance is non-nil, its
// absolute value is applied through the normal unsafe control-block path.
func (n *Node) ImportAccount(ctx context.Context, privateKey *ecdsa.PrivateKey, balance *big.Int) (ImportAccountResult, error) {
	key, err := clonePrivateKey(privateKey)
	if err != nil {
		return ImportAccountResult{}, err
	}
	address := crypto.PubkeyToAddress(key.PublicKey)
	var requestedBalance *big.Int
	if balance != nil {
		requestedBalance = new(big.Int).Set(balance)
		if requestedBalance.Sign() < 0 {
			return ImportAccountResult{}, errors.New("account balance cannot be negative")
		}
		if _, overflow := uint256.FromBig(requestedBalance); overflow {
			return ImportAccountResult{}, errors.New("account balance exceeds uint256")
		}
	}
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		if n.wallet.contains(address) {
			return ImportAccountResult{}, errAccountAlreadyManaged
		}
		result := ImportAccountResult{Address: address}
		if requestedBalance != nil {
			hash, applyErr := n.applyControl(chain, ControlChanges{address: {Balance: requestedBalance}})
			if applyErr != nil {
				return ImportAccountResult{}, applyErr
			}
			result.ControlBlockHash = &hash
		}
		if err := n.wallet.add(Account{Address: address, PrivateKey: key}); err != nil {
			return ImportAccountResult{}, err
		}
		attributes := []any{"event", "account_imported", "address", address.Hex()}
		if result.ControlBlockHash != nil {
			attributes = append(attributes, "control_block_hash", result.ControlBlockHash.Hex())
		}
		n.logger.Info("runtime account imported", attributes...)
		return result, nil
	})
	if err != nil {
		return ImportAccountResult{}, err
	}
	return value.(ImportAccountResult), nil
}

// RemoveAccount removes only a runtime-imported signer. It does not modify the
// corresponding execution account state.
func (n *Node) RemoveAccount(ctx context.Context, address common.Address) (bool, error) {
	value, err := n.execute(ctx, func(_ *executionChain) (any, error) {
		removed, removeErr := n.wallet.remove(address)
		if removeErr != nil {
			return false, removeErr
		}
		if removed {
			n.logger.Info("runtime account removed", "event", "account_removed", "address", address.Hex())
		}
		return removed, nil
	})
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}
