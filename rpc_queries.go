package ethertest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

const maxGetStorageSlots = 1024

type invalidParamsError struct{ message string }

func (err *invalidParamsError) Error() string  { return err.message }
func (err *invalidParamsError) ErrorCode() int { return -32602 }

type clientLimitExceededError struct{ message string }

func (err *clientLimitExceededError) Error() string  { return err.message }
func (err *clientLimitExceededError) ErrorCode() int { return -38026 }

func decodeStorageKey(input string) (common.Hash, error) {
	key, _, err := decodeStorageKeyWithLength(input)
	return key, err
}

func decodeStorageKeyWithLength(input string) (common.Hash, int, error) {
	key := input
	if strings.HasPrefix(key, "0x") || strings.HasPrefix(key, "0X") {
		key = key[2:]
	}
	if len(key)&1 != 0 {
		key = "0" + key
	}
	if len(key) > 2*common.HashLength {
		return common.Hash{}, 0, errors.New("storage key too long (want at most 32 bytes)")
	}
	decoded, err := hex.DecodeString(key)
	if err != nil {
		return common.Hash{}, 0, errors.New("invalid hex in storage key")
	}
	return common.BytesToHash(decoded), len(decoded), nil
}

// BlobBaseFee returns the blob base fee of the current canonical head. A nil
// value before Cancun is encoded as JSON null, matching geth's RPC behavior.
func (api *ethAPI) BlobBaseFee() *hexutil.Big {
	head := api.node.chain.blockchain.CurrentBlock()
	if head.ExcessBlobGas == nil {
		return nil
	}
	return (*hexutil.Big)(eip4844.CalcBlobFee(api.node.chain.config, head))
}

func (api *ethAPI) GetStorageValues(_ context.Context, requests map[common.Address][]string, selector rpc.BlockNumberOrHash) (map[common.Address][]hexutil.Bytes, error) {
	var total int
	for _, keys := range requests {
		total += len(keys)
		if total > maxGetStorageSlots {
			return nil, &clientLimitExceededError{message: fmt.Sprintf("too many slots (max %d)", maxGetStorageSlots)}
		}
	}
	if total == 0 {
		return nil, &invalidParamsError{message: "empty request"}
	}
	_, statedb, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	result := make(map[common.Address][]hexutil.Bytes, len(requests))
	for address, encodedKeys := range requests {
		keys := make([]common.Hash, len(encodedKeys))
		for index, encoded := range encodedKeys {
			key, err := decodeStorageKey(encoded)
			if err != nil {
				return nil, &invalidParamsError{message: fmt.Sprintf("invalid storage key for %s at index %d: %v", address, index, err)}
			}
			keys[index] = key
		}
		values := make([]hexutil.Bytes, len(keys))
		for index, key := range keys {
			value := statedb.GetState(address, key)
			values[index] = append(hexutil.Bytes(nil), value[:]...)
		}
		result[address] = values
	}
	return result, statedb.Error()
}

func (api *ethAPI) GetBlockReceipts(_ context.Context, selector rpc.BlockNumberOrHash) ([]map[string]any, error) {
	block, receipts, err := api.node.blockAndReceipts(selector)
	if err != nil || block == nil {
		return nil, err
	}
	transactions := block.Transactions()
	if len(receipts) != len(transactions) {
		return nil, fmt.Errorf("receipts length mismatch: %d vs %d", len(receipts), len(transactions))
	}
	result := make([]map[string]any, len(receipts))
	for index, receipt := range receipts {
		result[index] = marshalReceipt(receipt, transactions[index], block.Hash(), block.NumberU64(), uint64(index))
	}
	return result, nil
}

func (api *ethAPI) GetBlockTransactionCountByNumber(_ context.Context, number rpc.BlockNumber) (*hexutil.Uint, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	count := hexutil.Uint(len(block.Transactions()))
	return &count, nil
}

func (api *ethAPI) GetBlockTransactionCountByHash(_ context.Context, hash common.Hash) (*hexutil.Uint, error) {
	block := api.node.chain.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil, nil
	}
	count := hexutil.Uint(len(block.Transactions()))
	return &count, nil
}

func (api *ethAPI) GetTransactionByBlockNumberAndIndex(_ context.Context, number rpc.BlockNumber, index hexutil.Uint) (*rpcTransaction, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	pending := number == rpc.PendingBlockNumber
	return rpcTransactionAt(block, uint64(index), pending, api.node.chain.config), nil
}

func (api *ethAPI) GetTransactionByBlockHashAndIndex(_ context.Context, hash common.Hash, index hexutil.Uint) (*rpcTransaction, error) {
	block := api.node.chain.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil, nil
	}
	return rpcTransactionAt(block, uint64(index), false, api.node.chain.config), nil
}

func rpcTransactionAt(block *types.Block, index uint64, pending bool, config *params.ChainConfig) *rpcTransaction {
	transactions := block.Transactions()
	if index >= uint64(len(transactions)) {
		return nil
	}
	if pending {
		return newRPCTransaction(transactions[index], common.Hash{}, block.NumberU64(), block.Time(), index, nil, config)
	}
	return newRPCTransaction(transactions[index], block.Hash(), block.NumberU64(), block.Time(), index, block.BaseFee(), config)
}

func (api *debugAPI) GetRawHeader(_ context.Context, number rpc.BlockNumber) (hexutil.Bytes, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	return rlp.EncodeToBytes(block.Header())
}

func (api *debugAPI) GetRawBlock(_ context.Context, number rpc.BlockNumber) (hexutil.Bytes, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	return rlp.EncodeToBytes(block)
}

func (api *debugAPI) GetRawReceipts(_ context.Context, number rpc.BlockNumber) ([]hexutil.Bytes, error) {
	selector := rpc.BlockNumberOrHashWithNumber(number)
	block, receipts, err := api.node.blockAndReceipts(selector)
	if err != nil || block == nil {
		return nil, err
	}
	if len(receipts) != len(block.Transactions()) {
		return nil, fmt.Errorf("receipts length mismatch: %d vs %d", len(receipts), len(block.Transactions()))
	}
	result := make([]hexutil.Bytes, len(receipts))
	for index, receipt := range receipts {
		encoded, err := receipt.MarshalBinary()
		if err != nil {
			return nil, err
		}
		result[index] = encoded
	}
	return result, nil
}

func (api *debugAPI) GetRawTransaction(_ context.Context, hash common.Hash) (hexutil.Bytes, error) {
	transaction, _, _, _ := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if transaction == nil {
		transaction = api.node.chain.poolTransaction(hash)
	}
	if transaction == nil {
		return nil, nil
	}
	return transaction.MarshalBinary()
}

func (n *Node) blockAndReceipts(selector rpc.BlockNumberOrHash) (*types.Block, types.Receipts, error) {
	if selector.BlockHash == nil && selector.BlockNumber != nil && *selector.BlockNumber == rpc.PendingBlockNumber {
		block, _, receipts := n.chain.pendingSnapshot()
		if block == nil {
			return nil, nil, nil
		}
		return block, receipts, nil
	}
	block, err := n.blockByNumberOrHash(selector)
	if err != nil || block == nil {
		return nil, nil, err
	}
	receipts := n.chain.blockchain.GetReceiptsByHash(block.Hash())
	if receipts == nil && len(block.Transactions()) != 0 {
		return nil, nil, errors.New("receipts are unavailable")
	}
	if receipts == nil {
		receipts = types.Receipts{}
	}
	return block, receipts, nil
}

func (n *Node) blockByNumberOrHash(selector rpc.BlockNumberOrHash) (*types.Block, error) {
	if selector.BlockHash != nil {
		block := n.chain.blockchain.GetBlockByHash(*selector.BlockHash)
		if block == nil {
			return nil, nil
		}
		if selector.RequireCanonical && n.chain.blockchain.GetCanonicalHash(block.NumberU64()) != block.Hash() {
			return nil, &invalidInputError{message: "block is not canonical"}
		}
		return block, nil
	}
	number := rpc.LatestBlockNumber
	if selector.BlockNumber != nil {
		number = *selector.BlockNumber
	}
	return n.blockByNumber(number)
}

func (c *executionChain) poolTransaction(hash common.Hash) *types.Transaction {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, transactions := range c.pending {
		for _, transaction := range transactions {
			if transaction.Hash() == hash {
				return transaction
			}
		}
	}
	return nil
}
