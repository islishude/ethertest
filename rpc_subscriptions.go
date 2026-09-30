package ethertest

import (
	"context"
	"encoding/json"
	"errors"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"
)

// streamSubscription bounds queued notifications independently of the socket
// writer. A gap or a slow reader closes this connection, never the RPC server.
func (api *ethAPI) streamSubscription(ctx context.Context, pump func(<-chan struct{}, func(any) bool) error) (*rpc.Subscription, error) {
	api.node.lifecycleMu.Lock()
	defer api.node.lifecycleMu.Unlock()
	if api.node.closing {
		return nil, ErrNodeStopped
	}
	notifier, ok := rpc.NotifierFromContext(ctx)
	if !ok {
		return nil, errors.New("notifications unsupported")
	}
	client, ok := rpc.ClientFromContext(ctx)
	if !ok {
		return nil, errors.New("subscription connection unavailable")
	}
	if err := api.node.reserveSubscription(); err != nil {
		return nil, err
	}
	subscription := notifier.CreateSubscription()
	queue := make(chan any, int(api.node.cfg.Events.Capacity))
	done := make(chan struct{})
	fail := func(err error) {
		api.node.logger.Warn("subscription connection closed", "event", "subscription_failed", "reason", err.Error())
		client.Close()
	}
	api.node.backgroundTasks.Go(func() {
		defer close(queue)
		err := pump(done, func(value any) bool {
			encoded, err := json.Marshal(value)
			if err != nil {
				fail(err)
				return false
			}
			if len(encoded)+128 > int(api.node.cfg.Limits.MaxResponseBytes) {
				fail(errors.New("subscription response limit exceeded"))
				return false
			}
			select {
			case <-done:
				return false
			case <-api.node.stopping:
				return false
			default:
			}
			select {
			case queue <- value:
				return true
			default:
				fail(errors.New("subscription buffer overflow"))
				return false
			}
		})
		if err != nil {
			fail(err)
		}
	})
	api.node.backgroundTasks.Go(func() {
		defer api.node.releaseSubscription()
		defer close(done)
		// geth cancels the request context after writing the subscription reply
		// and activating its notifier. Wait here to avoid geth's unbounded
		// pre-activation buffer (especially for slow batch responses).
		select {
		case <-ctx.Done():
		case <-subscription.Err():
			return
		case <-api.node.stopping:
			return
		}
		for {
			select {
			case <-subscription.Err():
				return
			case <-api.node.stopping:
				return
			case value, ok := <-queue:
				if !ok {
					return
				}
				if err := notifier.Notify(subscription.ID, value); err != nil {
					return
				}
			}
		}
	})
	return subscription, nil
}

func (api *ethAPI) NewHeads(ctx context.Context) (*rpc.Subscription, error) {
	revision := api.node.Revision()
	return api.streamSubscription(ctx, func(done <-chan struct{}, emit func(any) bool) error {
		for {
			events, changed, err := api.node.events.sinceAndWait(revision)
			if err != nil {
				return err
			}
			for _, event := range events {
				revision = event.Revision
				if !isBlockRevisionEvent(event) || event.Removed {
					continue
				}
				block := api.node.chain.blockchain.GetBlockByHash(event.BlockHash)
				if block == nil {
					return errors.New("subscription block unavailable")
				}
				if !emit(block.Header()) {
					return nil
				}
			}
			select {
			case <-done:
				return nil
			case <-api.node.stopping:
				return nil
			case <-changed:
			}
		}
	})
}

func (api *ethAPI) Logs(ctx context.Context, criteria filters.FilterCriteria) (*rpc.Subscription, error) {
	if err := api.validateInstalledLogCriteria(criteria); err != nil {
		return nil, err
	}
	query := ethereum.FilterQuery(cloneFilterCriteria(criteria))
	revision := api.node.Revision()
	return api.streamSubscription(ctx, func(done <-chan struct{}, emit func(any) bool) error {
		for {
			events, changed, err := api.node.events.sinceAndWait(revision)
			if err != nil {
				return err
			}
			head := api.node.chain.blockchain.CurrentBlock().Number.Uint64()
			for _, event := range events {
				revision = event.Revision
				if !isBlockRevisionEvent(event) {
					continue
				}
				includes, err := api.filterIncludesBlock(query, event, head)
				if err != nil {
					return err
				}
				if !includes {
					continue
				}
				block := api.node.chain.blockchain.GetBlockByHash(event.BlockHash)
				if block == nil {
					return errors.New("subscription block unavailable")
				}
				for _, entry := range filterBlockLogs(api.node.chain.blockchain.GetReceiptsByHash(block.Hash()), query) {
					copy := *entry
					copy.Removed = event.Removed
					if !emit(&copy) {
						return nil
					}
				}
			}
			select {
			case <-done:
				return nil
			case <-api.node.stopping:
				return nil
			case <-changed:
			}
		}
	})
}

func (api *ethAPI) NewPendingTransactions(ctx context.Context, full *bool) (*rpc.Subscription, error) {
	sequence := api.node.pendingEvents.current()
	return api.streamSubscription(ctx, func(done <-chan struct{}, emit func(any) bool) error {
		for {
			events, changed, err := api.node.pendingEvents.sinceAndWait(sequence)
			if err != nil {
				return err
			}
			for _, event := range events {
				sequence = event.Sequence
				var value any = event.Hash
				if full != nil && *full {
					value = event.Transaction
				}
				if !emit(value) {
					return nil
				}
			}
			select {
			case <-done:
				return nil
			case <-api.node.stopping:
				return nil
			case <-changed:
			}
		}
	})
}
