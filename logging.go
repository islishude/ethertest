package ethertest

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
)

type Option func(*nodeOptions)

type nodeOptions struct {
	logger *slog.Logger
}

// WithLogger configures structured runtime logging. New is silent when this
// option is omitted, allowing an embedded node to follow its host's policy.
func WithLogger(logger *slog.Logger) Option {
	return func(options *nodeOptions) {
		if logger != nil {
			options.logger = logger
		}
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type progressSummary struct {
	blocks       uint64
	transactions uint64
	firstBlock   uint64
	lastBlock    uint64
	headHash     string
	slot         uint64
}

type progressReporter struct {
	current progressSummary
}

func (reporter *progressReporter) record(block *types.Block, slot uint64) {
	if reporter.current.blocks == 0 {
		reporter.current.firstBlock = block.NumberU64()
	}
	reporter.current.blocks++
	reporter.current.transactions += uint64(len(block.Transactions()))
	reporter.current.lastBlock = block.NumberU64()
	reporter.current.headHash = block.Hash().Hex()
	reporter.current.slot = slot
}

func (reporter *progressReporter) take() (progressSummary, bool) {
	if reporter.current.blocks == 0 {
		return progressSummary{}, false
	}
	summary := reporter.current
	reporter.current = progressSummary{}
	return summary, true
}

func (n *Node) recordAutomaticBlock(block *types.Block, source string) {
	background := context.Background()
	slot := n.chain.slotOf(block)
	if n.logger.Enabled(background, slog.LevelDebug) {
		n.logger.Debug("block mined",
			"event", "block_mined",
			"source", source,
			"block_number", block.NumberU64(),
			"block_hash", block.Hash().Hex(),
			"slot", slot,
			"transactions", len(block.Transactions()),
			"gas_used", block.GasUsed(),
		)
		return
	}
	if !n.logger.Enabled(background, slog.LevelInfo) {
		return
	}
	n.progress.record(block, slot)
}

func (n *Node) flushProgress() {
	summary, ok := n.progress.take()
	if !ok {
		return
	}
	n.logger.Info("chain progressed",
		"event", "chain_progress",
		"blocks", summary.blocks,
		"transactions", summary.transactions,
		"first_block", summary.firstBlock,
		"last_block", summary.lastBlock,
		"head_hash", summary.headHash,
		"slot", summary.slot,
	)
}

func (n *Node) reportIntervalFailure(event, message string, err error) {
	n.reportIntervalFailureAt(event, message, err, time.Now())
}

func (n *Node) reportIntervalFailureAt(event, message string, err error, now time.Time) {
	errorText := err.Error()
	failureKey := event + "\x00" + errorText
	if n.intervalFailure == failureKey && now.Sub(n.intervalFailureLoggedAt) < time.Minute {
		return
	}
	n.intervalFailure = failureKey
	n.intervalFailureLoggedAt = now
	n.logger.Error(message, "event", event, "error", err)
}

func (n *Node) reportIntervalRecovery() {
	if n.intervalFailure == "" {
		return
	}
	n.logger.Info("interval mining recovered", "event", "interval_mining_recovered")
	n.intervalFailure = ""
	n.intervalFailureLoggedAt = time.Time{}
}
