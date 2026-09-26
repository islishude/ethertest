package ethertest

import (
	"context"
	"encoding/json"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

func (api *debugAPI) GetRawBlockAccessList(_ context.Context, selector rpc.BlockNumberOrHash) (*hexutil.Bytes, error) {
	block, err := api.node.blockByNumberOrHash(selector)
	if err != nil || block == nil {
		return nil, err
	}
	if block.Header().BlockAccessListHash == nil {
		return nil, nil
	}
	raw, err := blockAccessListBytes(api.node.chain, block)
	if err != nil {
		return nil, err
	}
	if int64(len(raw))*2+4 > api.node.cfg.Limits.MaxResponseBytes {
		return nil, newResourceLimitError("block access list response bytes", uint64(len(raw))*2+4, uint64(api.node.cfg.Limits.MaxResponseBytes))
	}
	result := hexutil.Bytes(raw)
	return &result, nil
}

func (api *ethAPI) GetBlockAccessList(ctx context.Context, selector rpc.BlockNumberOrHash) ([]map[string]any, error) {
	raw, err := (&debugAPI{node: api.node}).GetRawBlockAccessList(ctx, selector)
	if err != nil || raw == nil {
		return nil, err
	}
	var list bal.BlockAccessList
	if err := rlp.DecodeBytes(*raw, &list); err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0, len(list))
	for _, account := range list {
		storage := make([]map[string]any, 0, len(account.StorageChanges))
		reads := make([]common.Hash, 0, len(account.StorageReads))
		balances := make([]map[string]any, 0, len(account.BalanceChanges))
		nonces := make([]map[string]any, 0, len(account.NonceChanges))
		codes := make([]map[string]any, 0, len(account.CodeChanges))
		for _, slot := range account.StorageChanges {
			changes := make([]map[string]any, 0, len(slot.SlotChanges))
			for _, v := range slot.SlotChanges {
				changes = append(changes, map[string]any{"index": hexutil.Uint64(v.BlockAccessIndex), "value": common.Hash(v.PostValue.Bytes32())})
			}
			storage = append(storage, map[string]any{"key": common.Hash(slot.Slot.Bytes32()), "changes": changes})
		}
		for _, key := range account.StorageReads {
			reads = append(reads, common.Hash(key.Bytes32()))
		}
		for _, v := range account.BalanceChanges {
			balances = append(balances, map[string]any{"index": hexutil.Uint64(v.BlockAccessIndex), "value": (*hexutil.U256)(v.PostBalance)})
		}
		for _, v := range account.NonceChanges {
			nonces = append(nonces, map[string]any{"index": hexutil.Uint64(v.BlockAccessIndex), "value": hexutil.Uint64(v.PostNonce)})
		}
		for _, v := range account.CodeChanges {
			codes = append(codes, map[string]any{"index": hexutil.Uint64(v.BlockAccessIndex), "value": hexutil.Bytes(v.NewCode)})
		}
		result = append(result, map[string]any{"address": account.Address, "storageChanges": storage, "storageReads": reads, "balanceChanges": balances, "nonceChanges": nonces, "codeChanges": codes})
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > api.node.cfg.Limits.MaxResponseBytes {
		return nil, newResourceLimitError("block access list response bytes", uint64(len(encoded)), uint64(api.node.cfg.Limits.MaxResponseBytes))
	}
	return result, nil
}
