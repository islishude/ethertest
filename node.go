package ethertest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

var ErrNodeStopped = errors.New("ethertest node is stopped")

type command struct {
	ctx context.Context
	fn  func(*executionChain) (any, error)
	out chan commandResult
}

type commandResult struct {
	value any
	err   error
}

type Node struct {
	cfg                Config
	chain              *executionChain
	wallet             *memoryWallet
	events             *eventLog
	pendingEvents      *pendingHashLog
	commands           chan command
	stopping           chan struct{}
	done               chan struct{}
	stopSignal         sync.Once
	stopOnce           sync.Once
	running            atomic.Bool
	rpcServer          *rpc.Server
	httpServer         *http.Server
	httpEndpoint       string
	nextSnapshot       uint64
	snapshots          map[uint64]*chainPoint
	checkpoints        map[string]*chainPoint
	branches           map[string]*branch
	consensus          *consensusModel
	logger             *slog.Logger
	startedAt          time.Time
	progress           progressReporter
	miningMu           sync.RWMutex
	miningMode         string
	resumeMiningMode   string
	miningChanged      chan struct{}
	pendingWithdrawals []WithdrawalRequest

	intervalFailure         string
	intervalFailureLoggedAt time.Time
	writeErr                error
	commitHook              func(commitStage) error
}

type chainPoint struct {
	hash    common.Hash
	number  uint64
	slot    uint64
	tainted bool
	used    bool
}

type branch struct {
	name    string
	base    common.Hash
	head    common.Hash
	blocks  []common.Hash
	tainted bool
}

func New(cfg Config, suppliedOptions ...Option) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	options := nodeOptions{logger: discardLogger()}
	for _, apply := range suppliedOptions {
		if apply != nil {
			apply(&options)
		}
	}
	configuredAccounts, err := DeriveAccounts(cfg.Accounts.Mnemonic, cfg.Accounts.Count)
	if err != nil {
		return nil, err
	}
	wallet, err := newMemoryWallet(configuredAccounts)
	if err != nil {
		return nil, err
	}
	configuredAddresses := wallet.accounts()
	chain, err := newExecutionChain(&cfg, configuredAddresses)
	if err != nil {
		return nil, err
	}
	events, err := newEventLog(cfg.Events.Capacity, chain.db)
	if err != nil {
		_ = chain.close()
		return nil, err
	}
	checkpoints, branches, err := loadControlMetadata(chain.db)
	if err != nil {
		_ = chain.close()
		return nil, err
	}
	n := &Node{
		cfg: cfg, chain: chain, wallet: wallet, events: events, pendingEvents: newPendingHashLog(cfg.Events.Capacity),
		commands: make(chan command), stopping: make(chan struct{}), done: make(chan struct{}),
		snapshots: make(map[uint64]*chainPoint), checkpoints: checkpoints,
		branches: branches, logger: options.logger,
		miningMode: cfg.Mining.Mode, miningChanged: make(chan struct{}, 1),
	}
	if cfg.Mining.Mode == "manual" {
		n.resumeMiningMode = "transaction"
	} else {
		n.resumeMiningMode = cfg.Mining.Mode
	}
	consensus, err := newConsensusModel(cfg, configuredAddresses)
	if err != nil {
		_ = chain.close()
		return nil, err
	}
	n.consensus = consensus
	if _, err := n.consensus.ensureProjection(chain, chain.blockchain.Genesis()); err != nil {
		_ = chain.close()
		return nil, err
	}
	if err := validateRuntimeMetadata(chain); err != nil {
		_ = chain.close()
		return nil, fmt.Errorf("validate persisted runtime metadata: %w", err)
	}
	if err := validateControlMetadata(chain, checkpoints, branches); err != nil {
		_ = chain.close()
		return nil, fmt.Errorf("validate persisted control metadata: %w", err)
	}
	if err := n.rebuildPendingView(chain); err != nil {
		_ = chain.close()
		return nil, err
	}
	return n, nil
}

func (n *Node) Snapshot(ctx context.Context) (uint64, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		head := chain.blockchain.CurrentBlock()
		n.nextSnapshot++
		chain.mu.RLock()
		pointSlot := chain.slot
		tainted := chain.blockSafety[head.Hash()].Tainted
		chain.mu.RUnlock()
		n.snapshots[n.nextSnapshot] = &chainPoint{
			hash: head.Hash(), number: head.Number.Uint64(), slot: pointSlot, tainted: tainted,
		}
		n.logger.Debug("snapshot created",
			"event", "snapshot_created",
			"snapshot_id", n.nextSnapshot,
			"block_number", head.Number.Uint64(),
			"block_hash", head.Hash().Hex(),
		)
		return n.nextSnapshot, nil
	})
	if err != nil {
		return 0, err
	}
	return value.(uint64), nil
}

func (n *Node) Revert(ctx context.Context, id uint64) (bool, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		point := n.snapshots[id]
		if point == nil || point.used {
			return false, nil
		}
		target := chain.blockchain.GetBlock(point.hash, point.number)
		if target == nil {
			return false, errors.New("snapshot block is unavailable")
		}
		if err := n.switchCanonical(chain, target, point.slot); err != nil {
			return false, err
		}
		point.used = true
		n.logger.Info("snapshot reverted",
			"event", "snapshot_reverted",
			"snapshot_id", id,
			"block_number", target.NumberU64(),
			"block_hash", target.Hash().Hex(),
		)
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}

func (n *Node) Checkpoint(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("checkpoint name is required")
	}
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		head := chain.blockchain.CurrentBlock()
		chain.mu.RLock()
		point := &chainPoint{
			hash: head.Hash(), number: head.Number.Uint64(), slot: chain.slot,
			tainted: chain.blockSafety[head.Hash()].Tainted,
		}
		chain.mu.RUnlock()
		if err := persistCheckpoint(chain.db, name, point); err != nil {
			return nil, err
		}
		n.checkpoints[name] = point
		n.logger.Info("checkpoint created",
			"event", "checkpoint_created",
			"name", name,
			"block_number", point.number,
			"block_hash", point.hash.Hex(),
		)
		return nil, nil
	})
	return err
}

func (n *Node) Restore(ctx context.Context, name string) error {
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		point := n.checkpoints[name]
		if point == nil {
			return nil, fmt.Errorf("checkpoint %q not found", name)
		}
		target := chain.blockchain.GetBlock(point.hash, point.number)
		if target == nil {
			return nil, errors.New("checkpoint block is unavailable")
		}
		if err := n.switchCanonical(chain, target, point.slot); err != nil {
			return nil, err
		}
		n.logger.Info("checkpoint restored",
			"event", "checkpoint_restored",
			"name", name,
			"block_number", target.NumberU64(),
			"block_hash", target.Hash().Hex(),
		)
		return nil, nil
	})
	return err
}

// Start starts the single-writer controller. Network listeners are started by
// Serve; embedded users may use the in-process methods without opening ports.
func (n *Node) Start() error {
	if !n.running.CompareAndSwap(false, true) {
		return errors.New("node already started")
	}
	go n.run()
	if err := n.startServers(); err != nil {
		n.stopSignal.Do(func() { close(n.stopping) })
		<-n.done
		n.running.Store(false)
		_ = n.chain.close()
		return err
	}
	n.startedAt = time.Now()
	head := n.chain.blockchain.CurrentBlock()
	endpoints := n.Endpoints()
	n.logger.Info("node started",
		"event", "node_started",
		"version", Version,
		"chain_id", n.cfg.Chain.ChainID,
		"fork", "osaka/fulu",
		"head_number", head.Number.Uint64(),
		"head_hash", head.Hash().Hex(),
		"slot", n.chain.currentSlot(),
		"mining_mode", n.currentMiningMode(),
		"storage_engine", n.cfg.Storage.Engine,
		"restored", head.Number.Uint64() != 0,
		"execution_endpoint", endpoints.Execution,
		"beacon_endpoint", endpoints.Beacon,
		"synthetic_finality", true,
	)
	if n.cfg.HTTP.Enabled && n.cfg.HTTP.AllowUnsafeExternal && !isLoopbackAddress(n.cfg.HTTP.Address) {
		n.logger.Warn("HTTP listener permits non-loopback binding",
			"event", "unsafe_external_listener",
			"address", n.cfg.HTTP.Address,
		)
	}
	return nil
}

func (n *Node) run() {
	defer close(n.done)
	var ticker *time.Ticker
	var ticks <-chan time.Time
	resetMiningTicker := func() {
		if ticker != nil {
			ticker.Stop()
			ticker, ticks = nil, nil
		}
		if n.currentMiningMode() == "interval" {
			ticker = time.NewTicker(n.cfg.Mining.Interval)
			ticks = ticker.C
		}
	}
	resetMiningTicker()
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	var progressTicker *time.Ticker
	var progressTicks <-chan time.Time
	background := context.Background()
	if n.logger.Enabled(background, slog.LevelInfo) && !n.logger.Enabled(background, slog.LevelDebug) {
		progressTicker = time.NewTicker(n.cfg.Log.ProgressInterval)
		progressTicks = progressTicker.C
		defer progressTicker.Stop()
	}
	for {
		select {
		case request := <-n.commands:
			select {
			case <-request.ctx.Done():
				request.out <- commandResult{err: request.ctx.Err()}
			default:
				value, err := request.fn(n.chain)
				request.out <- commandResult{value: value, err: err}
			}
		case <-n.stopping:
			n.flushProgress()
			return
		case <-progressTicks:
			n.flushProgress()
		case <-n.miningChanged:
			resetMiningTicker()
		case <-ticks:
			if n.currentMiningMode() != "interval" {
				continue
			}
			if n.chain.pendingCount() == 0 && len(n.pendingWithdrawals) == 0 && !n.cfg.Mining.AutoMineEmpty {
				continue
			}
			block, _, err := n.mineExecutionBlock(n.chain, false)
			if err != nil {
				n.reportIntervalFailure("interval_mining_failed", "interval mining failed", err)
				continue
			}
			n.reportIntervalRecovery()
			n.recordAutomaticBlock(block, "interval")
		}
	}
}

func (n *Node) execute(ctx context.Context, fn func(*executionChain) (any, error)) (any, error) {
	if !n.running.Load() {
		return nil, ErrNodeStopped
	}
	result := make(chan commandResult, 1)
	request := command{ctx: ctx, fn: fn, out: result}
	select {
	case n.commands <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.stopping:
		return nil, ErrNodeStopped
	}
	select {
	case response := <-result:
		return response.value, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (n *Node) SendTransaction(ctx context.Context, tx *types.Transaction) (common.Hash, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		if err := chain.addTransaction(tx); err != nil {
			return common.Hash{}, err
		}
		if err := n.rebuildPendingView(chain); err != nil {
			return common.Hash{}, err
		}
		n.pendingEvents.record(tx.Hash())
		n.logger.Debug("transaction accepted",
			"event", "transaction_accepted",
			"transaction_hash", tx.Hash().Hex(),
			"transaction_type", tx.Type(),
			"nonce", tx.Nonce(),
		)
		if n.currentMiningMode() == "transaction" {
			block, _, err := n.mineExecutionBlock(chain, false)
			if err != nil {
				return common.Hash{}, err
			}
			n.recordAutomaticBlock(block, "transaction")
		}
		return tx.Hash(), nil
	})
	if err != nil {
		return common.Hash{}, err
	}
	return value.(common.Hash), nil
}

func (n *Node) currentMiningMode() string {
	n.miningMu.RLock()
	defer n.miningMu.RUnlock()
	return n.miningMode
}

func (n *Node) setMiningMode(mode string) {
	n.miningMu.Lock()
	n.miningMode = mode
	if mode != "manual" {
		n.resumeMiningMode = mode
	}
	n.miningMu.Unlock()
	select {
	case n.miningChanged <- struct{}{}:
	default:
	}
}

func (n *Node) resumeMode() string {
	n.miningMu.RLock()
	defer n.miningMu.RUnlock()
	return n.resumeMiningMode
}

func (n *Node) Mine(ctx context.Context, count uint64, empty bool) ([]common.Hash, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		hashes := make([]common.Hash, 0, count)
		var firstNumber uint64
		var lastNumber uint64
		var transactions uint64
		for range count {
			block, _, err := n.mineExecutionBlock(chain, empty)
			if err != nil {
				return nil, err
			}
			hashes = append(hashes, block.Hash())
			if len(hashes) == 1 {
				firstNumber = block.NumberU64()
			}
			lastNumber = block.NumberU64()
			transactions += uint64(len(block.Transactions()))
		}
		if len(hashes) != 0 {
			n.logger.Info("blocks mined",
				"event", "blocks_mined",
				"source", "manual",
				"blocks", len(hashes),
				"transactions", transactions,
				"first_block", firstNumber,
				"last_block", lastNumber,
				"head_hash", hashes[len(hashes)-1].Hex(),
				"slot", chain.currentSlot(),
				"empty", empty,
			)
		}
		return hashes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]common.Hash), nil
}

func (n *Node) mineExecutionBlock(chain *executionChain, empty bool) (*types.Block, types.Receipts, error) {
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	projection, err := n.consensus.ensureProjection(chain, parent)
	if err != nil {
		return nil, nil, err
	}
	block, receipts, targetSlot, err := chain.buildBlock(
		uint64(n.cfg.Chain.SlotDuration/time.Second), empty, common.Hash(projection.Root), n.pendingWithdrawals,
	)
	if err != nil {
		return nil, nil, err
	}
	projectionPut, err := n.consensus.projectionPut(chain, block)
	if err != nil {
		return nil, nil, err
	}
	chain.mu.RLock()
	parentSafety := chain.blockSafety[parent.Hash()]
	timeline := chain.timeline()
	chain.mu.RUnlock()
	timeline.CurrentSlot = targetSlot
	timeline.LastProcessedSlot = targetSlot
	safety := blockSafetyForChild(parentSafety, block.Hash(), "")
	timelineMutation, err := timelinePut(timeline)
	if err != nil {
		return nil, nil, err
	}
	safetyMutation, err := blockSafetyPut(block.Hash(), safety)
	if err != nil {
		return nil, nil, err
	}
	operation := preparedOperation{
		Kind: "head", OldHead: parent.Hash(), NewHead: block.Hash(),
		TargetNumber: block.NumberU64(), DiscardTargetOnCancel: true,
		Puts: []journalKV{
			timelineMutation, blockSlotPut(block.Hash(), targetSlot),
			canonicalSlotPut(targetSlot, block.Hash()), safetyMutation, projectionPut,
		},
	}
	events := []Event{{Type: "block", Slot: targetSlot, BlockHash: block.Hash(), BlockNumber: block.NumberU64()}}
	if finalized := n.finalizedEventBetween(timeline.CurrentSlot-1, targetSlot); finalized != nil {
		events = append(events, *finalized)
	}
	if err := n.commitPrepared(chain, operation, events, func() error {
		_, insertErr := chain.blockchain.InsertChain(types.Blocks{block})
		return insertErr
	}, func() {
		chain.applyCanonicalBlock(block, targetSlot)
		chain.mu.Lock()
		chain.blockSafety[block.Hash()] = safety
		chain.mu.Unlock()
	}); err != nil {
		return nil, nil, err
	}
	n.pendingWithdrawals = nil
	if err := n.rebuildPendingView(chain); err != nil {
		n.writeErr = err
		return nil, nil, fmt.Errorf("block committed but pending view rebuild failed: %w", err)
	}
	return block, receipts, nil
}

func (n *Node) rebuildPendingView(chain *executionChain) error {
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	projection, err := n.consensus.ensureProjection(chain, parent)
	if err != nil {
		return err
	}
	block, receipts, _, err := chain.buildBlock(
		uint64(n.cfg.Chain.SlotDuration/time.Second), false, common.Hash(projection.Root), n.pendingWithdrawals,
	)
	if err != nil {
		return err
	}
	statedb, err := chain.blockchain.StateAt(block.Header())
	if err != nil {
		return err
	}
	chain.setPendingView(block, statedb, receipts)
	return nil
}

func (n *Node) MissSlots(ctx context.Context, count uint64) ([]uint64, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		slots := make([]uint64, 0, count)
		chain.mu.RLock()
		start := chain.slot
		timeline := chain.timeline()
		chain.mu.RUnlock()
		events := make([]Event, 0, count)
		for offset := uint64(1); offset <= count; offset++ {
			slot := start + offset
			slots = append(slots, slot)
			events = append(events, Event{Type: "missed_slot", Slot: slot})
		}
		if count != 0 {
			timeline.CurrentSlot = start + count
			timeline.LastProcessedSlot = start + count
			mutation, err := timelinePut(timeline)
			if err != nil {
				return nil, err
			}
			if finalized := n.finalizedEventBetween(start, start+count); finalized != nil {
				events = append(events, *finalized)
			}
			if err := n.commitAuxiliary(chain, []journalKV{mutation}, nil, events, func() {
				chain.mu.Lock()
				chain.slot = start + count
				chain.lastProcessedSlot = start + count
				chain.mu.Unlock()
			}); err != nil {
				return nil, err
			}
			if err := n.rebuildPendingView(chain); err != nil {
				return nil, err
			}
		}
		if len(slots) != 0 {
			n.logger.Info("slots missed",
				"event", "slots_missed",
				"count", len(slots),
				"first_slot", slots[0],
				"last_slot", slots[len(slots)-1],
			)
		}
		return slots, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]uint64), nil
}

func (n *Node) Revision() Revision {
	return n.events.current()
}

func (n *Node) EventsSince(revision Revision) ([]Event, error) {
	return n.events.since(revision)
}

func (n *Node) Accounts() []common.Address {
	return n.wallet.accounts()
}

func (n *Node) Close() error {
	var err error
	n.stopOnce.Do(func() {
		wasRunning := n.running.Swap(false)
		if wasRunning {
			n.logger.Info("node stopping", "event", "node_stopping")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if n.httpServer != nil {
			if shutdownErr := n.httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
				err = shutdownErr
				n.logger.Error("HTTP server shutdown failed",
					"event", "http_shutdown_failed",
					"error", shutdownErr,
				)
			}
		}
		if n.rpcServer != nil {
			n.rpcServer.Stop()
		}
		if wasRunning {
			n.stopSignal.Do(func() { close(n.stopping) })
			<-n.done
		}
		head := n.chain.blockchain.CurrentBlock()
		revision := n.Revision()
		if n.cfg.DumpState != "" {
			if dumpErr := n.dumpState(n.cfg.DumpState); dumpErr != nil {
				if err == nil {
					err = dumpErr
				}
				n.logger.Error("state archive failed", "event", "state_archive_failed", "error", dumpErr)
			}
		}
		if closeErr := n.chain.close(); closeErr != nil {
			if err == nil {
				err = closeErr
			}
			n.logger.Error("chain storage close failed", "event", "storage_close_failed", "error", closeErr)
		}
		if wasRunning {
			n.logger.Info("node stopped",
				"event", "node_stopped",
				"head_number", head.Number.Uint64(),
				"head_hash", head.Hash().Hex(),
				"revision", revision,
				"uptime", time.Since(n.startedAt).Round(time.Millisecond).String(),
				"clean", err == nil,
			)
		}
	})
	return err
}

func (n *Node) RPCClient() *rpc.Client {
	if n.rpcServer == nil {
		return nil
	}
	return rpc.DialInProc(n.rpcServer)
}

type Endpoints struct {
	Execution string `json:"execution"`
	Beacon    string `json:"beacon"`
}

func (n *Node) Endpoints() Endpoints {
	endpoints := Endpoints{Execution: n.httpEndpoint}
	if n.cfg.Beacon.Enabled && n.httpEndpoint != "" {
		endpoints.Beacon = n.httpEndpoint
	}
	return endpoints
}
