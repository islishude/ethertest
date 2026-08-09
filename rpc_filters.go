package ethertest

import (
	"context"
	"errors"
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
}

func (api *ethAPI) GetLogs(_ context.Context, criteria filters.FilterCriteria) ([]*types.Log, error) {
	return api.logs(criteria, nil)
}

func (api *ethAPI) NewFilter(criteria filters.FilterCriteria) rpc.ID {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	api.ensureFiltersLocked()
	id := rpc.NewID()
	api.filters[id] = &installedFilter{kind: installedLogFilter, criteria: criteria, revision: api.node.Revision()}
	return id
}

func (api *ethAPI) NewBlockFilter() rpc.ID {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	api.ensureFiltersLocked()
	id := rpc.NewID()
	api.filters[id] = &installedFilter{kind: installedBlockFilter, revision: api.node.Revision()}
	return id
}

func (api *ethAPI) NewPendingTransactionFilter() rpc.ID {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	api.ensureFiltersLocked()
	id := rpc.NewID()
	api.filters[id] = &installedFilter{
		kind: installedPendingTransactionFilter, pendingSeq: api.node.pendingEvents.current(),
	}
	return id
}

func (api *ethAPI) ensureFiltersLocked() {
	if api.filters == nil {
		api.filters = make(map[rpc.ID]*installedFilter)
	}
}

func (api *ethAPI) GetFilterLogs(id rpc.ID) ([]*types.Log, error) {
	api.filterMu.Lock()
	filter := api.filters[id]
	api.filterMu.Unlock()
	if filter == nil {
		return nil, errors.New("filter not found")
	}
	if filter.kind != installedLogFilter {
		return nil, errors.New("filter is not a log filter")
	}
	return api.logs(filter.criteria, nil)
}

func (api *ethAPI) GetFilterChanges(id rpc.ID) (any, error) {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	filter := api.filters[id]
	if filter == nil {
		return nil, errors.New("filter not found")
	}
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
		for _, event := range events {
			filter.pendingSeq = event.Sequence
			result = append(result, event.Hash)
		}
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
		for _, event := range events {
			filter.revision = event.Revision
			if isBlockRevisionEvent(event) && !event.Removed {
				result = append(result, event.BlockHash)
			}
		}
		return result, nil
	}
	result := make([]*types.Log, 0)
	query := ethereum.FilterQuery(filter.criteria)
	for _, event := range events {
		filter.revision = event.Revision
		if !isBlockRevisionEvent(event) || !filterIncludesBlock(query, event) {
			continue
		}
		block := api.node.chain.blockchain.GetBlockByHash(event.BlockHash)
		if block == nil {
			continue
		}
		for _, entry := range filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query) {
			copy := *entry
			copy.Removed = event.Removed
			result = append(result, &copy)
		}
	}
	return result, nil
}

func (api *ethAPI) UninstallFilter(id rpc.ID) bool {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	if api.filters[id] == nil {
		return false
	}
	delete(api.filters, id)
	return true
}

func isBlockRevisionEvent(event Event) bool {
	return event.Type == "block" || event.Type == "control_block"
}

func filterIncludesBlock(query ethereum.FilterQuery, event Event) bool {
	if query.BlockHash != nil {
		return *query.BlockHash == event.BlockHash
	}
	if query.FromBlock != nil && query.FromBlock.Sign() >= 0 && event.BlockNumber < query.FromBlock.Uint64() {
		return false
	}
	if query.ToBlock != nil && query.ToBlock.Sign() >= 0 && event.BlockNumber > query.ToBlock.Uint64() {
		return false
	}
	return true
}

func (api *ethAPI) NewHeads(ctx context.Context) (*rpc.Subscription, error) {
	notifier, ok := rpc.NotifierFromContext(ctx)
	if !ok {
		return nil, errors.New("notifications unsupported")
	}
	subscription := notifier.CreateSubscription()
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		revision := api.node.Revision()
		for {
			select {
			case <-subscription.Err():
				return
			case <-api.node.stopping:
				return
			case <-ticker.C:
				events, err := api.node.EventsSince(revision)
				if err != nil {
					revision = api.node.Revision()
					continue
				}
				for _, event := range events {
					revision = event.Revision
					if !isBlockRevisionEvent(event) || event.Removed {
						continue
					}
					block := api.node.chain.blockchain.GetBlockByHash(event.BlockHash)
					if block != nil {
						_ = notifier.Notify(subscription.ID, block.Header())
					}
				}
			}
		}
	}()
	return subscription, nil
}

func (api *ethAPI) logs(criteria filters.FilterCriteria, fromOverride *uint64) ([]*types.Log, error) {
	query := ethereum.FilterQuery(criteria)
	if query.BlockHash != nil {
		block := api.node.chain.blockchain.GetBlockByHash(*query.BlockHash)
		if block == nil {
			return []*types.Log{}, nil
		}
		return filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query), nil
	}
	head := api.node.chain.blockchain.CurrentBlock().Number.Uint64()
	from, to := uint64(0), head
	if query.FromBlock != nil {
		if query.FromBlock.Sign() >= 0 {
			from = query.FromBlock.Uint64()
		} else {
			from = head
		}
	}
	if fromOverride != nil && *fromOverride > from {
		from = *fromOverride
	}
	if query.ToBlock != nil {
		if query.ToBlock.Sign() >= 0 {
			to = query.ToBlock.Uint64()
		}
	}
	if to > head {
		to = head
	}
	if from > to {
		return []*types.Log{}, nil
	}
	result := make([]*types.Log, 0)
	for number := from; number <= to; number++ {
		block := api.node.chain.blockchain.GetBlockByNumber(number)
		if block == nil {
			continue
		}
		result = append(result, filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query)...)
	}
	return result, nil
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
