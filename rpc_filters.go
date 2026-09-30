package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"
)

type installedFilterKind uint8

const (
	installedLogFilter installedFilterKind = iota
	installedBlockFilter
	installedPendingTransactionFilter
)

type installedFilter struct {
	kind       installedFilterKind
	criteria   filters.FilterCriteria
	revision   Revision
	pendingSeq uint64
	lastUsed   time.Time
}

func (api *ethAPI) GetLogs(ctx context.Context, criteria filters.FilterCriteria) ([]*types.Log, error) {
	return api.logs(ctx, criteria, nil)
}

func (api *ethAPI) NewFilter(ctx context.Context, criteria filters.FilterCriteria) (rpc.ID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := api.validateInstalledLogCriteria(criteria); err != nil {
		return "", err
	}
	criteria = cloneFilterCriteria(criteria)
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	return api.installFilterLocked(&installedFilter{kind: installedLogFilter, criteria: criteria, revision: api.node.Revision()})
}

func cloneFilterCriteria(criteria filters.FilterCriteria) filters.FilterCriteria {
	query := ethereum.FilterQuery(criteria)
	cloned := ethereum.FilterQuery{
		Addresses: append([]common.Address(nil), query.Addresses...),
		Topics:    make([][]common.Hash, len(query.Topics)),
	}
	if query.BlockHash != nil {
		value := *query.BlockHash
		cloned.BlockHash = &value
	}
	if query.FromBlock != nil {
		cloned.FromBlock = new(big.Int).Set(query.FromBlock)
	}
	if query.ToBlock != nil {
		cloned.ToBlock = new(big.Int).Set(query.ToBlock)
	}
	for index := range query.Topics {
		cloned.Topics[index] = append([]common.Hash(nil), query.Topics[index]...)
	}
	return filters.FilterCriteria(cloned)
}

func (api *ethAPI) NewBlockFilter() (rpc.ID, error) {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	return api.installFilterLocked(&installedFilter{kind: installedBlockFilter, revision: api.node.Revision()})
}

func (api *ethAPI) NewPendingTransactionFilter() (rpc.ID, error) {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	return api.installFilterLocked(&installedFilter{
		kind: installedPendingTransactionFilter, pendingSeq: api.node.pendingEvents.current(),
	})
}

func (api *ethAPI) installFilterLocked(filter *installedFilter) (rpc.ID, error) {
	api.ensureFiltersLocked()
	now := time.Now()
	api.pruneExpiredFiltersLocked(now)
	if len(api.filters) >= api.node.cfg.Limits.MaxFilters {
		return "", newResourceLimitError("installed filter count", uint64(len(api.filters)+1), uint64(api.node.cfg.Limits.MaxFilters))
	}
	id := rpc.NewID()
	filter.lastUsed = now
	api.filters[id] = filter
	return id, nil
}

func (api *ethAPI) ensureFiltersLocked() {
	if api.filters == nil {
		api.filters = make(map[rpc.ID]*installedFilter)
	}
}

func (api *ethAPI) pruneExpiredFiltersLocked(now time.Time) {
	for id, filter := range api.filters {
		if now.Sub(filter.lastUsed) >= api.node.cfg.Limits.FilterTimeout {
			delete(api.filters, id)
		}
	}
}

func (api *ethAPI) startFilterReaper(ctx context.Context) {
	interval := api.node.cfg.Limits.FilterTimeout / 2
	if interval <= 0 {
		interval = api.node.cfg.Limits.FilterTimeout
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	api.node.backgroundTasks.Go(func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				api.filterMu.Lock()
				api.pruneExpiredFiltersLocked(now)
				api.filterMu.Unlock()
			}
		}
	})
}

func (api *ethAPI) GetFilterLogs(ctx context.Context, id rpc.ID) ([]*types.Log, error) {
	api.filterMu.Lock()
	api.pruneExpiredFiltersLocked(time.Now())
	filter := api.filters[id]
	if filter != nil {
		filter.lastUsed = time.Now()
	}
	api.filterMu.Unlock()
	if filter == nil {
		return nil, errors.New("filter not found")
	}
	if filter.kind != installedLogFilter {
		return nil, errors.New("filter is not a log filter")
	}
	return api.logs(ctx, filter.criteria, nil)
}

func (api *ethAPI) GetFilterChanges(ctx context.Context, id rpc.ID) (any, error) {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	now := time.Now()
	api.pruneExpiredFiltersLocked(now)
	filter := api.filters[id]
	if filter == nil {
		return nil, errors.New("filter not found")
	}
	filter.lastUsed = now
	if filter.kind == installedPendingTransactionFilter {
		events, err := api.node.pendingEvents.since(filter.pendingSeq)
		if errors.Is(err, ErrEventGap) {
			filter.pendingSeq = api.node.pendingEvents.current()
			return nil, &invalidInputError{message: "pending transaction filter history is no longer available"}
		}
		if err != nil {
			return nil, err
		}
		result := make([]common.Hash, 0, len(events))
		nextSequence := filter.pendingSeq
		for _, event := range events {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nextSequence = event.Sequence
			result = append(result, event.Hash)
		}
		if err := api.checkHashResultLimits("pending transaction filter", len(result)); err != nil {
			return nil, err
		}
		filter.pendingSeq = nextSequence
		return result, nil
	}
	events, err := api.node.EventsSince(filter.revision)
	if errors.Is(err, ErrEventGap) {
		filter.revision = api.node.Revision()
		return nil, &invalidInputError{message: "filter history is no longer available"}
	}
	if err != nil {
		return nil, err
	}
	if filter.kind == installedBlockFilter {
		result := make([]common.Hash, 0, len(events))
		nextRevision := filter.revision
		for _, event := range events {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nextRevision = event.Revision
			if isBlockRevisionEvent(event) && !event.Removed {
				result = append(result, event.BlockHash)
			}
		}
		if err := api.checkHashResultLimits("block filter", len(result)); err != nil {
			return nil, err
		}
		filter.revision = nextRevision
		return result, nil
	}
	result := make([]*types.Log, 0)
	encodedSize := uint64(2)
	if encodedSize > uint64(api.node.cfg.Limits.MaxResponseBytes) {
		return nil, newResourceLimitError("log response bytes", encodedSize, uint64(api.node.cfg.Limits.MaxResponseBytes))
	}
	query := ethereum.FilterQuery(filter.criteria)
	head := api.node.chain.blockchain.CurrentBlock().Number.Uint64()
	nextRevision := filter.revision
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		nextRevision = event.Revision
		if !isBlockRevisionEvent(event) {
			continue
		}
		includes, err := api.filterIncludesBlock(query, event, head)
		if err != nil {
			return nil, err
		}
		if !includes {
			continue
		}
		block := api.node.chain.blockchain.GetBlockByHash(event.BlockHash)
		if block == nil {
			continue
		}
		for _, entry := range filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query) {
			copy := *entry
			copy.Removed = event.Removed
			if err := api.appendLimitedLogs(&result, &encodedSize, []*types.Log{&copy}); err != nil {
				return nil, err
			}
		}
	}
	filter.revision = nextRevision
	return result, nil
}

func (api *ethAPI) UninstallFilter(id rpc.ID) bool {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	api.pruneExpiredFiltersLocked(time.Now())
	if api.filters[id] == nil {
		return false
	}
	delete(api.filters, id)
	return true
}

func isBlockRevisionEvent(event Event) bool {
	return event.Type == "block" || event.Type == "control_block"
}

func (api *ethAPI) filterIncludesBlock(query ethereum.FilterQuery, event Event, head uint64) (bool, error) {
	if query.BlockHash != nil {
		return *query.BlockHash == event.BlockHash, nil
	}
	if query.FromBlock != nil {
		from, err := api.resolveLogBlockNumber(query.FromBlock, rpc.LatestBlockNumber, head)
		if err != nil {
			return false, err
		}
		if event.BlockNumber < from {
			return false, nil
		}
	}
	if query.ToBlock != nil {
		to, err := api.resolveLogBlockNumber(query.ToBlock, rpc.LatestBlockNumber, head)
		if err != nil {
			return false, err
		}
		if event.BlockNumber > to {
			return false, nil
		}
	}
	return true, nil
}

func (api *ethAPI) logs(ctx context.Context, criteria filters.FilterCriteria, fromOverride *uint64) ([]*types.Log, error) {
	if api.node.cfg.Limits.MaxResponseBytes < 2 {
		return nil, newResourceLimitError("log response bytes", 2, uint64(api.node.cfg.Limits.MaxResponseBytes))
	}
	query := ethereum.FilterQuery(criteria)
	if query.BlockHash != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		block := api.node.chain.blockchain.GetBlockByHash(*query.BlockHash)
		if block == nil {
			return []*types.Log{}, nil
		}
		result := make([]*types.Log, 0)
		encodedSize := uint64(2)
		if err := api.appendLimitedLogs(
			&result, &encodedSize, filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query),
		); err != nil {
			return nil, err
		}
		return result, nil
	}
	head := api.node.chain.blockchain.CurrentBlock().Number.Uint64()
	from, err := api.resolveLogBlockNumber(query.FromBlock, rpc.LatestBlockNumber, head)
	if err != nil {
		return nil, err
	}
	if fromOverride != nil && *fromOverride > from {
		from = *fromOverride
	}
	to, err := api.resolveLogBlockNumber(query.ToBlock, rpc.LatestBlockNumber, head)
	if err != nil {
		return nil, err
	}
	if from > to {
		return nil, &invalidInputError{message: "invalid block range: fromBlock exceeds toBlock"}
	}
	if to > head {
		to = head
	}
	if from > head {
		return []*types.Log{}, nil
	}
	width := to - from
	if width >= api.node.cfg.Limits.MaxLogBlocks {
		requested := width
		if requested != ^uint64(0) {
			requested++
		}
		return nil, newResourceLimitError("log block range", requested, api.node.cfg.Limits.MaxLogBlocks)
	}
	result := make([]*types.Log, 0)
	encodedSize := uint64(2)
	for number := from; ; number++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		block := api.node.chain.blockchain.GetBlockByNumber(number)
		if block != nil {
			if err := api.appendLimitedLogs(
				&result, &encodedSize, filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query),
			); err != nil {
				return nil, err
			}
		}
		if number == to {
			break
		}
	}
	return result, nil
}

func (api *ethAPI) validateInstalledLogCriteria(criteria filters.FilterCriteria) error {
	query := ethereum.FilterQuery(criteria)
	if query.BlockHash != nil {
		return nil
	}
	head := api.node.chain.blockchain.CurrentBlock().Number.Uint64()
	var from, to *uint64
	if query.FromBlock != nil {
		value, err := api.resolveLogBlockNumber(query.FromBlock, rpc.LatestBlockNumber, head)
		if err != nil {
			return err
		}
		from = &value
	}
	if query.ToBlock != nil {
		value, err := api.resolveLogBlockNumber(query.ToBlock, rpc.LatestBlockNumber, head)
		if err != nil {
			return err
		}
		to = &value
	}
	if from != nil && to != nil && *from > *to {
		return &invalidInputError{message: "invalid block range: fromBlock exceeds toBlock"}
	}
	return nil
}

func (api *ethAPI) resolveLogBlockNumber(value *big.Int, fallback rpc.BlockNumber, head uint64) (uint64, error) {
	number := fallback
	if value != nil {
		if value.Sign() >= 0 {
			if value.BitLen() > 64 {
				return 0, &invalidInputError{message: "log range block number exceeds uint64"}
			}
			return value.Uint64(), nil
		}
		if !value.IsInt64() {
			return 0, &invalidInputError{message: "unsupported log range block tag"}
		}
		number = rpc.BlockNumber(value.Int64())
	}
	switch number {
	case rpc.LatestBlockNumber:
		return head, nil
	case rpc.EarliestBlockNumber:
		return 0, nil
	case rpc.SafeBlockNumber, rpc.FinalizedBlockNumber:
		block, err := api.node.blockByNumber(number)
		if err != nil {
			return 0, err
		}
		if block == nil {
			return 0, &resourceNotFoundError{message: "log range block not found"}
		}
		return block.NumberU64(), nil
	case rpc.PendingBlockNumber:
		return 0, &invalidInputError{message: "pending logs are not supported"}
	default:
		if number < 0 {
			return 0, &invalidInputError{message: "unsupported log range block tag"}
		}
		return uint64(number), nil
	}
}

func (api *ethAPI) appendLimitedLogs(result *[]*types.Log, encodedSize *uint64, logs []*types.Log) error {
	for _, entry := range logs {
		requested := len(*result) + 1
		if requested > api.node.cfg.Limits.MaxLogResults {
			return newResourceLimitError("log result count", uint64(requested), uint64(api.node.cfg.Limits.MaxLogResults))
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		additional := uint64(len(encoded))
		if len(*result) != 0 {
			additional++
		}
		limit := uint64(api.node.cfg.Limits.MaxResponseBytes)
		if *encodedSize > limit || additional > limit-*encodedSize {
			return newResourceLimitError(
				"log response bytes", *encodedSize+additional, uint64(api.node.cfg.Limits.MaxResponseBytes),
			)
		}
		*encodedSize += additional
		*result = append(*result, entry)
	}
	return nil
}

func (api *ethAPI) checkHashResultLimits(resource string, count int) error {
	if count > api.node.cfg.Limits.MaxLogResults {
		return newResourceLimitError(resource+" result count", uint64(count), uint64(api.node.cfg.Limits.MaxLogResults))
	}
	size := uint64(2)
	if count != 0 {
		size += uint64(count)*68 + uint64(count-1)
	}
	if size > uint64(api.node.cfg.Limits.MaxResponseBytes) {
		return newResourceLimitError(resource+" response bytes", size, uint64(api.node.cfg.Limits.MaxResponseBytes))
	}
	return nil
}

func filterBlockLogs(receipts types.Receipts, query ethereum.FilterQuery) []*types.Log {
	result := make([]*types.Log, 0)
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			if !matchesAddress(log.Address, query.Addresses) || !matchesTopics(log.Topics, query.Topics) {
				continue
			}
			result = append(result, log)
		}
	}
	return result
}

func matchesAddress(address common.Address, addresses []common.Address) bool {
	if len(addresses) == 0 {
		return true
	}
	return slices.Contains(addresses, address)
}

func matchesTopics(logTopics []common.Hash, filterTopics [][]common.Hash) bool {
	if len(filterTopics) > len(logTopics) {
		return false
	}
	for index, alternatives := range filterTopics {
		if len(alternatives) == 0 {
			continue
		}
		matched := false
		for _, candidate := range alternatives {
			matched = matched || candidate == logTopics[index]
		}
		if !matched {
			return false
		}
	}
	return true
}
