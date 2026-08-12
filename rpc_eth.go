package ethertest

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

type accountProof struct {
	Address      common.Address `json:"address"`
	AccountProof []string       `json:"accountProof"`
	Balance      *hexutil.Big   `json:"balance"`
	CodeHash     common.Hash    `json:"codeHash"`
	Nonce        hexutil.Uint64 `json:"nonce"`
	StorageHash  common.Hash    `json:"storageHash"`
	StorageProof []storageProof `json:"storageProof"`
}

type storageProof struct {
	Key   string       `json:"key"`
	Value *hexutil.Big `json:"value"`
	Proof []string     `json:"proof"`
}

func (api *ethAPI) ChainId() hexutil.Uint64 { return hexutil.Uint64(api.node.cfg.Chain.ChainID) }
func (api *ethAPI) BlockNumber() hexutil.Uint64 {
	return hexutil.Uint64(api.node.chain.blockchain.CurrentBlock().Number.Uint64())
}
func (api *ethAPI) Accounts() []common.Address { return api.node.Accounts() }
func (api *ethAPI) Coinbase() common.Address   { return api.node.chain.feeRecipientAddress() }
func (api *ethAPI) Mining() bool               { return api.node.currentMiningMode() != miningModeManual }
func (api *ethAPI) Hashrate() hexutil.Uint64   { return 0 }
func (api *ethAPI) Syncing() bool              { return false }
func (api *ethAPI) GasPrice() *hexutil.Big {
	price := new(big.Int).Add(api.node.chain.blockchain.CurrentBlock().BaseFee, big.NewInt(1_000_000_000))
	return (*hexutil.Big)(price)
}
func (api *ethAPI) MaxPriorityFeePerGas() *hexutil.Big {
	return (*hexutil.Big)(big.NewInt(1_000_000_000))
}

func (api *ethAPI) GetBalance(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	balance := (*hexutil.Big)(state.GetBalance(address).ToBig())
	return balance, state.Error()
}

func (api *ethAPI) GetTransactionCount(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return 0, err
	}
	nonce := hexutil.Uint64(state.GetNonce(address))
	return nonce, state.Error()
}

func (api *ethAPI) GetCode(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	code := state.GetCode(address)
	return code, state.Error()
}

func (api *ethAPI) GetStorageAt(_ context.Context, address common.Address, hexKey string, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	key, err := decodeStorageKey(hexKey)
	if err != nil {
		return nil, &invalidParamsError{message: fmt.Sprintf("%v: %q", err, hexKey)}
	}
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	value := state.GetState(address, key)
	return value[:], state.Error()
}

const maxGetProofKeys = 1024

type orderedProofList []string

func (proof *orderedProofList) Put(_ []byte, value []byte) error {
	*proof = append(*proof, hexutil.Encode(value))
	return nil
}

func (*orderedProofList) Delete([]byte) error { return errors.New("proof deletion is unsupported") }

func (api *ethAPI) GetProof(ctx context.Context, address common.Address, encodedKeys []string, selector rpc.BlockNumberOrHash) (*accountProof, error) {
	if len(encodedKeys) > maxGetProofKeys {
		return nil, &invalidParamsError{message: fmt.Sprintf("too many storage keys requested (max %d, got %d)", maxGetProofKeys, len(encodedKeys))}
	}
	keys := make([]common.Hash, len(encodedKeys))
	keyLengths := make([]int, len(encodedKeys))
	for index, encoded := range encodedKeys {
		var err error
		keys[index], keyLengths[index], err = decodeStorageKeyWithLength(encoded)
		if err != nil {
			return nil, &invalidParamsError{message: fmt.Sprintf("%v: %q", err, encoded)}
		}
	}
	header, statedb, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	accountTrie, err := statedb.Database().OpenTrie(header.Root)
	if err != nil {
		return nil, err
	}
	accountProofNodes := make(orderedProofList, 0)
	if err := accountTrie.Prove(crypto.Keccak256(address.Bytes()), &accountProofNodes); err != nil {
		return nil, err
	}
	storageRoot := statedb.GetStorageRoot(address)
	result := &accountProof{
		Address: address, AccountProof: accountProofNodes,
		Balance:  (*hexutil.Big)(statedb.GetBalance(address).ToBig()),
		CodeHash: statedb.GetCodeHash(address), Nonce: hexutil.Uint64(statedb.GetNonce(address)),
		StorageHash: storageRoot, StorageProof: make([]storageProof, len(keys)),
	}
	var storageTrie state.Trie
	if storageRoot != types.EmptyRootHash && storageRoot != (common.Hash{}) {
		storageTrie, err = statedb.Database().OpenStorageTrie(header.Root, address, storageRoot, nil)
		if err != nil {
			return nil, err
		}
	}
	for index, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outputKey := hexutil.EncodeBig(key.Big())
		if keyLengths[index] == common.HashLength {
			outputKey = hexutil.Encode(key[:])
		}
		proof := make(orderedProofList, 0)
		if storageTrie != nil {
			if err := storageTrie.Prove(crypto.Keccak256(key.Bytes()), &proof); err != nil {
				return nil, err
			}
		}
		value := statedb.GetState(address, key).Big()
		result.StorageProof[index] = storageProof{
			Key: outputKey, Value: (*hexutil.Big)(value), Proof: proof,
		}
	}
	return result, statedb.Error()
}

func (n *Node) resolveHeader(selector rpc.BlockNumberOrHash) (*types.Header, error) {
	if selector.BlockHash != nil {
		header := n.chain.blockchain.GetHeaderByHash(*selector.BlockHash)
		if header == nil {
			return nil, &resourceNotFoundError{message: "header not found"}
		}
		if selector.RequireCanonical && n.chain.blockchain.GetCanonicalHash(header.Number.Uint64()) != header.Hash() {
			return nil, &invalidInputError{message: "header is not canonical"}
		}
		return header, nil
	}
	number := rpc.LatestBlockNumber
	if selector.BlockNumber != nil {
		number = *selector.BlockNumber
	}
	block, err := n.blockByNumber(number)
	if err != nil || block == nil {
		if err != nil {
			return nil, err
		}
		return nil, &resourceNotFoundError{message: "header not found"}
	}
	return block.Header(), nil
}

func (n *Node) resolveState(selector rpc.BlockNumberOrHash) (*types.Header, *state.StateDB, error) {
	if selector.BlockHash == nil && selector.BlockNumber != nil && *selector.BlockNumber == rpc.PendingBlockNumber {
		block, statedb, _ := n.chain.pendingSnapshot()
		if block == nil || statedb == nil {
			return nil, nil, &resourceNotFoundError{message: "pending state is unavailable"}
		}
		return block.Header(), statedb, nil
	}
	header, err := n.resolveHeader(selector)
	if err != nil {
		return nil, nil, err
	}
	statedb, err := n.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, nil, err
	}
	return header, statedb, nil
}
