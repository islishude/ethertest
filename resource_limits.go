package ethertest

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrResourceLimit is returned when a request exceeds a configured local-node
// work or response bound.
var ErrResourceLimit = errors.New("resource limit exceeded")

type resourceLimitError struct {
	resource  string
	limit     uint64
	requested uint64
}

func (err *resourceLimitError) Error() string {
	return fmt.Sprintf("%s: %v (requested %d, limit %d)", err.resource, ErrResourceLimit, err.requested, err.limit)
}

func (err *resourceLimitError) Unwrap() error  { return ErrResourceLimit }
func (err *resourceLimitError) ErrorCode() int { return -38026 }

func newResourceLimitError(resource string, requested, limit uint64) error {
	return &resourceLimitError{resource: resource, requested: requested, limit: limit}
}

type rpcTimeoutError struct{ message string }

func (err *rpcTimeoutError) Error() string  { return err.message }
func (err *rpcTimeoutError) ErrorCode() int { return -32016 }
func (err *rpcTimeoutError) Unwrap() error  { return context.DeadlineExceeded }

func (n *Node) withRPCTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= n.cfg.Limits.TraceTimeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, n.cfg.Limits.TraceTimeout)
}

func (n *Node) checkResponseBytes(size int) error {
	if int64(size) > n.cfg.Limits.MaxResponseBytes {
		return newResourceLimitError("response bytes", uint64(size), uint64(n.cfg.Limits.MaxResponseBytes))
	}
	return nil
}

func (n *Node) checkFixedListResponse(resource string, count, elementBytes uint64) error {
	requested := uint64(2)
	if count != 0 {
		if count > (^uint64(0)-1)/(elementBytes+1) {
			requested = ^uint64(0)
		} else {
			requested = 1 + count*(elementBytes+1)
		}
	}
	if requested > uint64(n.cfg.Limits.MaxResponseBytes) {
		return newResourceLimitError(resource+" response bytes", requested, uint64(n.cfg.Limits.MaxResponseBytes))
	}
	return nil
}

func (n *Node) reserveSubscription() error {
	n.subscriptionMu.Lock()
	defer n.subscriptionMu.Unlock()
	if n.activeSubscriptions >= n.cfg.Limits.MaxSubscriptions {
		return newResourceLimitError(
			"active subscription count", uint64(n.activeSubscriptions+1), uint64(n.cfg.Limits.MaxSubscriptions),
		)
	}
	n.activeSubscriptions++
	return nil
}

func (n *Node) releaseSubscription() {
	n.subscriptionMu.Lock()
	if n.activeSubscriptions > 0 {
		n.activeSubscriptions--
	}
	n.subscriptionMu.Unlock()
}
