package ethertest

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

func (api *txpoolAPI) Status() map[string]hexutil.Uint {
	pending, queued := api.poolCounts()
	return map[string]hexutil.Uint{"pending": hexutil.Uint(pending), "queued": hexutil.Uint(queued)}
}

func (api *txpoolAPI) Content() map[string]any {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	pending := make(map[string]map[string]any)
	queued := make(map[string]map[string]any)
	for address, transactions := range api.node.chain.pending {
		for nonce, transaction := range transactions {
			target := api.poolTarget(transaction, pending, queued)
			if target[address.Hex()] == nil {
				target[address.Hex()] = make(map[string]any)
			}
			target[address.Hex()][strconv.FormatUint(nonce, 10)] = api.poolRPCTransaction(transaction)
		}
	}
	return map[string]any{"pending": pending, "queued": queued}
}

func (api *txpoolAPI) ContentFrom(address common.Address) map[string]map[string]any {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	pending := make(map[string]any)
	queued := make(map[string]any)
	for nonce, transaction := range api.node.chain.pending[address] {
		target := queued
		if api.node.chain.pendingView != nil {
			if _, executable := api.node.chain.pendingView.executable[transaction.Hash()]; executable {
				target = pending
			}
		}
		target[strconv.FormatUint(nonce, 10)] = api.poolRPCTransaction(transaction)
	}
	return map[string]map[string]any{"pending": pending, "queued": queued}
}

func (api *txpoolAPI) poolTarget(transaction *types.Transaction, pending, queued map[string]map[string]any) map[string]map[string]any {
	if api.node.chain.pendingView != nil {
		if _, executable := api.node.chain.pendingView.executable[transaction.Hash()]; executable {
			return pending
		}
	}
	return queued
}

func (api *txpoolAPI) poolRPCTransaction(transaction *types.Transaction) *rpcTransaction {
	head := api.node.chain.blockchain.CurrentBlock()
	if api.node.chain.pendingView != nil && api.node.chain.pendingView.block != nil {
		head = api.node.chain.pendingView.block.Header()
	}
	return newRPCTransaction(transaction, common.Hash{}, head.Number.Uint64(), head.Time, 0, nil, api.node.chain.config)
}

func (api *txpoolAPI) poolCounts() (pending, queued int) {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	if api.node.chain.pendingView == nil {
		for _, transactions := range api.node.chain.pending {
			queued += len(transactions)
		}
		return 0, queued
	}
	pending = len(api.node.chain.pendingView.executable)
	queued = len(api.node.chain.pendingView.queued)
	return pending, queued
}

// Inspect uses exactly the same executable/queued classification as Content.
func (api *txpoolAPI) Inspect() map[string]map[string]map[string]string {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	result := map[string]map[string]map[string]string{"pending": {}, "queued": {}}
	for address, transactions := range api.node.chain.pending {
		for nonce, tx := range transactions {
			kind := "queued"
			if api.node.chain.pendingView != nil {
				if _, ok := api.node.chain.pendingView.executable[tx.Hash()]; ok {
					kind = "pending"
				}
			}
			if result[kind][strings.ToLower(address.Hex())] == nil {
				result[kind][strings.ToLower(address.Hex())] = make(map[string]string)
			}
			to := "contract creation"
			if tx.To() != nil {
				to = strings.ToLower(tx.To().Hex())
			}
			result[kind][strings.ToLower(address.Hex())][strconv.FormatUint(nonce, 10)] = fmt.Sprintf("%s: %s wei + %d gas × %s wei", to, tx.Value(), tx.Gas(), tx.GasPrice())
		}
	}
	return result
}
