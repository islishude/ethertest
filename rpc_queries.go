package ethertest

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
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

func (api *ethAPI) GetBlockByNumber(_ context.Context, number rpc.BlockNumber, full bool) (map[string]any, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	result := marshalBlock(block, full, api.node.chain.config)
	if number == rpc.PendingBlockNumber {
		for _, field := range []string{"hash", "nonce", "miner"} {
			result[field] = nil
		}
		if full {
			transactions := block.Transactions()
			items := make([]*rpcTransaction, len(transactions))
			for index := range transactions {
				items[index] = newRPCTransaction(
					transactions[index], common.Hash{}, block.NumberU64(), block.Time(), uint64(index), nil, api.node.chain.config,
				)
			}
			result["transactions"] = items
		}
	}
	return result, nil
}

func (api *ethAPI) GetBlockByHash(_ context.Context, hash common.Hash, full bool) map[string]any {
	block := api.node.chain.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil
	}
	return marshalBlock(block, full, api.node.chain.config)
}

func (api *ethAPI) GetTransactionByHash(_ context.Context, hash common.Hash) *rpcTransaction {
	tx, blockHash, blockNumber, index := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if tx != nil {
		block := api.node.chain.blockchain.GetBlockByHash(blockHash)
		if block == nil {
			return nil
		}
		return newRPCTransaction(tx, blockHash, blockNumber, block.Time(), index, block.BaseFee(), api.node.chain.config)
	}
	if pending := api.node.chain.poolTransaction(hash); pending != nil {
		head := api.node.chain.blockchain.CurrentBlock()
		if block := api.node.chain.pendingBlock(); block != nil {
			head = block.Header()
		}
		return newRPCTransaction(pending, common.Hash{}, head.Number.Uint64(), head.Time, 0, nil, api.node.chain.config)
	}
	return nil
}

func (api *ethAPI) GetTransactionReceipt(_ context.Context, hash common.Hash) (map[string]any, error) {
	return api.transactionReceipt(hash)
}

func (api *ethAPI) transactionReceipt(hash common.Hash) (map[string]any, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if tx == nil {
		return nil, nil
	}
	receipt, _, _, _ := rawdb.ReadCanonicalReceipt(api.node.chain.db, hash, api.node.chain.config)
	if receipt == nil {
		return nil, nil
	}
	return marshalReceipt(receipt, tx, blockHash, blockNumber, index), nil
}

func (n *Node) blockByNumber(number rpc.BlockNumber) (*types.Block, error) {
	head := n.chain.blockchain.CurrentBlock().Number.Uint64()
	switch number {
	case rpc.LatestBlockNumber:
		return n.chain.blockchain.GetBlockByNumber(head), nil
	case rpc.PendingBlockNumber:
		return n.chain.pendingBlock(), nil
	case rpc.EarliestBlockNumber:
		return n.chain.blockchain.GetBlockByNumber(0), nil
	case rpc.SafeBlockNumber:
		return n.resolveSyntheticFinality(n.chain.currentSlot()).Safe, nil
	case rpc.FinalizedBlockNumber:
		return n.resolveSyntheticFinality(n.chain.currentSlot()).Finalized, nil
	default:
		if number < 0 {
			return nil, fmt.Errorf("unsupported block tag %s", number.String())
		}
		return n.chain.blockchain.GetBlockByNumber(uint64(number)), nil
	}
}

func marshalBlock(block *types.Block, full bool, config *params.ChainConfig) map[string]any {
	headerJSON, _ := headerMap(block.Header())
	result := headerJSON
	result["size"] = hexutil.Uint64(block.Size())
	result["uncles"] = []common.Hash{}
	result["withdrawals"] = block.Withdrawals()
	txs := block.Transactions()
	if full {
		items := make([]*rpcTransaction, len(txs))
		for i, tx := range txs {
			items[i] = newRPCTransaction(
				tx, block.Hash(), block.NumberU64(), block.Time(), uint64(i), block.BaseFee(), config,
			)
		}
		result["transactions"] = items
	} else {
		hashes := make([]common.Hash, len(txs))
		for i, tx := range txs {
			hashes[i] = tx.Hash()
		}
		result["transactions"] = hashes
	}
	return result
}

func headerMap(header *types.Header) (map[string]any, error) {
	data, err := header.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(data, &result)
	return result, err
}

func newRPCTransaction(
	tx *types.Transaction,
	blockHash common.Hash,
	blockNumber uint64,
	blockTime uint64,
	index uint64,
	baseFee *big.Int,
	config *params.ChainConfig,
) *rpcTransaction {
	signer := types.MakeSigner(config, new(big.Int).SetUint64(blockNumber), blockTime)
	from, _ := types.Sender(signer, tx)
	v, r, s := tx.RawSignatureValues()
	result := &rpcTransaction{
		Type:     hexutil.Uint64(tx.Type()),
		From:     from,
		Gas:      hexutil.Uint64(tx.Gas()),
		GasPrice: (*hexutil.Big)(tx.GasPrice()),
		Hash:     tx.Hash(),
		Input:    hexutil.Bytes(tx.Data()),
		Nonce:    hexutil.Uint64(tx.Nonce()),
		To:       tx.To(),
		Value:    (*hexutil.Big)(tx.Value()),
		V:        (*hexutil.Big)(v),
		R:        (*hexutil.Big)(r),
		S:        (*hexutil.Big)(s),
	}
	if blockHash != (common.Hash{}) {
		result.BlockHash = &blockHash
		result.BlockNumber = (*hexutil.Big)(new(big.Int).SetUint64(blockNumber))
		result.BlockTimestamp = (*hexutil.Uint64)(&blockTime)
		result.TransactionIndex = (*hexutil.Uint64)(&index)
	}

	switch tx.Type() {
	case types.LegacyTxType:
		if chainID := tx.ChainId(); chainID.Sign() != 0 {
			result.ChainID = (*hexutil.Big)(chainID)
		}
	case types.AccessListTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
	case types.DynamicFeeTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		if baseFee != nil && blockHash != (common.Hash{}) {
			result.GasPrice = (*hexutil.Big)(effectiveGasPrice(tx, baseFee))
		} else {
			result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
		}
	case types.BlobTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		if baseFee != nil && blockHash != (common.Hash{}) {
			result.GasPrice = (*hexutil.Big)(effectiveGasPrice(tx, baseFee))
		} else {
			result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
		}
		result.MaxFeePerBlobGas = (*hexutil.Big)(tx.BlobGasFeeCap())
		result.BlobVersionedHashes = tx.BlobHashes()
	case types.SetCodeTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		if baseFee != nil && blockHash != (common.Hash{}) {
			result.GasPrice = (*hexutil.Big)(effectiveGasPrice(tx, baseFee))
		} else {
			result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
		}
		result.AuthorizationList = tx.SetCodeAuthorizations()
	}
	return result
}

func effectiveGasPrice(tx *types.Transaction, baseFee *big.Int) *big.Int {
	price := tx.GasTipCap()
	price.Add(price, baseFee)
	if tx.GasFeeCapIntCmp(price) < 0 {
		return tx.GasFeeCap()
	}
	return price
}

func marshalReceipt(receipt *types.Receipt, tx *types.Transaction, blockHash common.Hash, blockNumber, index uint64) map[string]any {
	from, _ := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	logs := receipt.Logs
	if logs == nil {
		logs = []*types.Log{}
	}
	result := map[string]any{
		"blockHash":         blockHash,
		"blockNumber":       hexutil.Uint64(blockNumber),
		"transactionHash":   tx.Hash(),
		"transactionIndex":  hexutil.Uint64(index),
		"from":              from,
		"to":                tx.To(),
		"gasUsed":           hexutil.Uint64(receipt.GasUsed),
		"cumulativeGasUsed": hexutil.Uint64(receipt.CumulativeGasUsed),
		"contractAddress":   nil,
		"logs":              logs,
		"logsBloom":         receipt.Bloom,
		"type":              hexutil.Uint64(tx.Type()),
		"effectiveGasPrice": (*hexutil.Big)(receipt.EffectiveGasPrice),
	}
	if len(receipt.PostState) > 0 {
		result["root"] = hexutil.Bytes(receipt.PostState)
	} else {
		result["status"] = hexutil.Uint64(receipt.Status)
	}
	if receipt.ContractAddress != (common.Address{}) {
		result["contractAddress"] = receipt.ContractAddress
	}
	if tx.Type() == types.BlobTxType {
		result["blobGasUsed"] = hexutil.Uint64(receipt.BlobGasUsed)
		result["blobGasPrice"] = (*hexutil.Big)(receipt.BlobGasPrice)
	}
	return result
}
