package ethertest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/rpc"
)

var ErrNodeStopped = errors.New("ethertest node is stopped")

type nodeLifecycle uint8

const (
	nodeLifecycleNew nodeLifecycle = iota
	nodeLifecycleStarting
	nodeLifecycleRunning
	nodeLifecycleStopping
	nodeLifecycleStopped
)

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
	cfg                      Config
	chain                    *executionChain
	wallet                   *memoryWallet
	events                   *eventLog
	pendingEvents            *pendingHashLog
	commands                 chan command
	stopping                 chan struct{}
	done                     chan struct{}
	stopSignal               sync.Once
	doneSignal               sync.Once
	running                  atomic.Bool
	lifecycleMu              sync.Mutex
	lifecycle                nodeLifecycle
	closing                  bool
	controllerStarted        bool
	terminalErr              error
	closeErr                 error
	closeDone                chan struct{}
	rootCtx                  context.Context
	rootCancel               context.CancelFunc
	rpcServer                *rpc.Server
	httpServer               *http.Server
	httpListener             net.Listener
	httpEndpoint             string
	ipcServer                *rpc.Server
	ipcListener              net.Listener
	ipcEndpoint              string
	ipcStopping              atomic.Bool
	ipcServeDone             chan struct{}
	nextSnapshot             uint64
	snapshots                map[uint64]*chainPoint
	checkpoints              map[string]*chainPoint
	branches                 map[string]*branch
	consensus                *consensusModel
	logger                   *slog.Logger
	startedAt                time.Time
	progress                 progressReporter
	handlerMu                sync.Mutex
	handlersClosing          bool
	httpHandlers             sync.WaitGroup
	ipcHandlers              sync.WaitGroup
	backgroundTasks          sync.WaitGroup
	subscriptionMu           sync.Mutex
	activeSubscriptions      int
	miningMu                 sync.RWMutex
	miningMode               string
	resumeMiningMode         string
	miningChanged            chan struct{}
	pendingWithdrawals       []WithdrawalRequest
	pendingExecutionRequests executionRequestQueue

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
	tainted bool
}

func New(cfg Config, suppliedOptions ...Option) (*Node, error) {
	resolved, genesis, err := resolveConfiguredGenesis(cfg)
	if err != nil {
		return nil, err
	}
	cfg = resolved
	cfg.HTTP.CORS = append([]string(nil), cfg.HTTP.CORS...)
	if cfg.Storage.Engine == "pebble" {
		fresh, existed, err := freshStoragePath(cfg.Storage.Path)
		if err != nil {
			return nil, err
		}
		if fresh {
			return newStagedPebbleNode(cfg, genesis, existed, suppliedOptions...)
		}
		if err := validateExistingPebbleStorage(cfg.Storage.Path); err != nil {
			return nil, err
		}
	}
	return newNode(cfg, genesis, suppliedOptions...)
}

func newNode(cfg Config, genesis *core.Genesis, suppliedOptions ...Option) (*Node, error) {
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
	chain, err := newExecutionChain(&cfg, configuredAddresses, genesis)
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
	executionRequests, err := loadExecutionRequestQueue(chain)
	if err != nil {
		_ = chain.close()
		return nil, err
	}
	rootCtx, rootCancel := context.WithCancel(context.Background())
	n := &Node{
		cfg: cfg, chain: chain, wallet: wallet, events: events, pendingEvents: newPendingHashLog(cfg.Events.Capacity),
		commands: make(chan command), stopping: make(chan struct{}), done: make(chan struct{}),
		closeDone: make(chan struct{}), rootCtx: rootCtx, rootCancel: rootCancel,
		snapshots: make(map[uint64]*chainPoint), checkpoints: checkpoints,
		branches: branches, logger: options.logger,
		miningMode: cfg.Mining.Mode, miningChanged: make(chan struct{}, 1),
		pendingExecutionRequests: executionRequests,
	}
	if cfg.Mining.Mode == miningModeManual {
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
	if err := validateExecutionRequestMetadata(n, chain); err != nil {
		_ = chain.close()
		return nil, fmt.Errorf("validate persisted execution request metadata: %w", err)
	}
	if err := initializeBeaconRootIndex(n.consensus, chain); err != nil {
		_ = chain.close()
		return nil, err
	}
	if err := validateControlMetadata(chain, checkpoints, branches); err != nil {
		_ = chain.close()
		return nil, fmt.Errorf("validate persisted control metadata: %w", err)
	}
	if err := n.rebuildPendingView(context.Background(), chain); err != nil {
		_ = chain.close()
		return nil, err
	}
	return n, nil
}

func freshStoragePath(path string) (fresh, existed bool, err error) {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if len(entries) == 0 {
		return true, true, nil
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "marker.manifest.") {
			return false, true, nil
		}
	}
	return false, true, errors.New("nonempty storage.path is not a Pebble database")
}

func validateExistingPebbleStorage(path string) error {
	kv, err := pebble.New(path, 64, 64, "ethertest-preflight", true)
	if err != nil {
		return fmt.Errorf("open existing Pebble storage read-only: %w", err)
	}
	db := rawdb.NewDatabase(kv)
	_, validateErr := readPersistedGenesisMetadata(db)
	closeErr := db.Close()
	if validateErr != nil {
		validateErr = fmt.Errorf("validate existing ethertest storage: %w", validateErr)
	}
	return errors.Join(validateErr, closeErr)
}

func newStagedPebbleNode(cfg Config, genesis *core.Genesis, _ bool, suppliedOptions ...Option) (*Node, error) {
	parent := filepath.Dir(cfg.Storage.Path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(cfg.Storage.Path)+".init-*")
	if err != nil {
		return nil, err
	}
	target, err := inspectEmptyDirectoryTarget(cfg.Storage.Path)
	if err != nil {
		return nil, err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(staging)
		}
	}()
	stagedConfig := cfg
	stagedConfig.Storage.Path = staging
	stagedConfig.DumpState = ""
	staged, err := newNode(stagedConfig, genesis, suppliedOptions...)
	if err != nil {
		return nil, err
	}
	if err := staged.Close(); err != nil {
		return nil, err
	}
	rollback, err := installStagedDirectory(staging, cfg.Storage.Path, target)
	if err != nil {
		return nil, err
	}
	node, err := newNode(cfg, genesis, suppliedOptions...)
	if err != nil {
		return nil, errors.Join(err, rollback())
	}
	installed = true
	return node, nil
}

func (n *Node) Snapshot(ctx context.Context) (uint64, error) {
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		head := chain.blockchain.CurrentBlock()
		if n.nextSnapshot == ^uint64(0) {
			return nil, errors.New("snapshot ID overflows uint64")
		}
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
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		point := n.snapshots[id]
		if point == nil || point.used {
			return false, nil
		}
		target := chain.blockchain.GetBlock(point.hash, point.number)
		if target == nil {
			return false, errors.New("snapshot block is unavailable")
		}
		if err := n.switchCanonical(ctx, chain, target, point.slot); err != nil {
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
	_, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		head := chain.blockchain.CurrentBlock()
		chain.mu.RLock()
		point := &chainPoint{
			hash: head.Hash(), number: head.Number.Uint64(), slot: chain.slot,
			tainted: chain.blockSafety[head.Hash()].Tainted,
		}
		chain.mu.RUnlock()
		mutation, err := checkpointPut(name, point)
		if err != nil {
			return nil, err
		}
		if err := n.commitAuxiliary(chain, []journalKV{mutation}, nil, nil, func() {
			n.checkpoints[name] = point
		}); err != nil {
			return nil, err
		}
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
	_, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		point := n.checkpoints[name]
		if point == nil {
			return nil, fmt.Errorf("checkpoint %q not found", name)
		}
		target := chain.blockchain.GetBlock(point.hash, point.number)
		if target == nil {
			return nil, errors.New("checkpoint block is unavailable")
		}
		if err := n.switchCanonical(ctx, chain, target, point.slot); err != nil {
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
	n.lifecycleMu.Lock()
	defer n.lifecycleMu.Unlock()
	if n.lifecycle == nodeLifecycleStarting || n.lifecycle == nodeLifecycleRunning {
		return errors.New("node already started")
	}
	if n.lifecycle == nodeLifecycleStopping || n.lifecycle == nodeLifecycleStopped {
		return ErrNodeStopped
	}
	n.lifecycle = nodeLifecycleStarting
	n.running.Store(true)
	n.controllerStarted = true
	go n.run()
	if err := n.startServers(); err != nil {
		n.terminalErr = err
		n.stopSignal.Do(func() { close(n.stopping) })
		n.rootCancel()
		<-n.done
		n.backgroundTasks.Wait()
		n.running.Store(false)
		n.closeErr = errors.Join(err, n.chain.close())
		n.lifecycle = nodeLifecycleStopped
		n.closing = true
		close(n.closeDone)
		return err
	}
	select {
	case <-n.stopping:
		err := n.terminalErr
		if err == nil {
			err = ErrNodeStopped
		}
		<-n.done
		n.backgroundTasks.Wait()
		n.running.Store(false)
		n.closeErr = errors.Join(err, n.chain.close())
		n.lifecycle = nodeLifecycleStopped
		n.closing = true
		close(n.closeDone)
		return err
	default:
	}
	n.lifecycle = nodeLifecycleRunning
	n.startedAt = time.Now()
	head := n.chain.blockchain.CurrentBlock()
	endpoints := n.Endpoints()
	n.logger.Info("node started",
		"event", "node_started",
		"version", Version,
		"chain_id", n.cfg.Chain.ChainID,
		"fork", n.consensus.forkName(n.chain.currentSlot()),
		"head_number", head.Number.Uint64(),
		"head_hash", head.Hash().Hex(),
		"slot", n.chain.currentSlot(),
		"mining_mode", n.currentMiningMode(),
		"storage_engine", n.cfg.Storage.Engine,
		"restored", head.Number.Uint64() != 0,
		"execution_endpoint", endpoints.Execution,
		"beacon_endpoint", endpoints.Beacon,
		"ipc_endpoint", endpoints.IPC,
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
	defer n.doneSignal.Do(func() { close(n.done) })
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
	background := n.rootCtx
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
				value, err, panicked := n.callCommand(request.fn)
				request.out <- commandResult{value: value, err: err}
				if panicked {
					n.flushProgress()
					return
				}
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
			if n.chain.pendingCount() == 0 && len(n.pendingWithdrawals) == 0 &&
				n.pendingExecutionRequests.empty() && !n.cfg.Mining.AutoMineEmpty {
				continue
			}
			block, _, err := n.mineExecutionBlock(background, n.chain, false)
			if err != nil {
				n.reportIntervalFailure("interval_mining_failed", "interval mining failed", err)
				continue
			}
			n.reportIntervalRecovery()
			n.recordAutomaticBlock(block, "interval")
		}
	}
}

func (n *Node) callCommand(fn func(*executionChain) (any, error)) (value any, err error, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			err = fmt.Errorf("single-writer command panicked: %v", recovered)
			n.requestStop(err)
		}
	}()
	value, err = fn(n.chain)
	return value, err, false
}

func (n *Node) requestStop(err error) {
	n.running.Store(false)
	if err != nil {
		n.lifecycleMu.Lock()
		if n.terminalErr == nil {
			n.terminalErr = err
		}
		if n.lifecycle == nodeLifecycleStarting || n.lifecycle == nodeLifecycleRunning {
			n.lifecycle = nodeLifecycleStopping
		}
		n.lifecycleMu.Unlock()
	}
	n.stopSignal.Do(func() { close(n.stopping) })
	n.rootCancel()
}

// Wait blocks until the controller stops, a background server fails, or ctx is
// canceled. It does not close storage; callers should always call Close.
func (n *Node) Wait(ctx context.Context) error {
	select {
	case <-n.done:
		n.lifecycleMu.Lock()
		err := n.terminalErr
		n.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
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

func (n *Node) executeWrite(ctx context.Context, fn func(*executionChain) (any, error)) (any, error) {
	return n.execute(ctx, func(chain *executionChain) (any, error) {
		if n.writeErr != nil {
			return nil, fmt.Errorf("node writes are disabled after a persistence failure: %w", n.writeErr)
		}
		return fn(chain)
	})
}

func (n *Node) SendTransaction(ctx context.Context, tx *types.Transaction) (common.Hash, error) {
	if tx == nil {
		return common.Hash{}, errors.New("transaction is required")
	}
	if sidecar := tx.BlobTxSidecar(); sidecar != nil {
		tx = tx.WithBlobTxSidecar(sidecar.Copy())
	}
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		snapshot := chain.snapshotTransactionPool()
		oldHead := chain.blockchain.CurrentBlock().Hash()
		if err := chain.addTransaction(tx); err != nil {
			return common.Hash{}, err
		}
		if n.currentMiningMode() == "transaction" {
			block, _, err := n.mineExecutionBlock(ctx, chain, false)
			if err != nil {
				if chain.blockchain.CurrentBlock().Hash() == oldHead {
					chain.restoreTransactionPool(snapshot)
				}
				return common.Hash{}, err
			}
			n.recordAutomaticBlock(block, "transaction")
		} else {
			candidate, err := n.pendingCandidate(ctx, chain, n.pendingExecutionRequests)
			if err != nil {
				chain.restoreTransactionPool(snapshot)
				return common.Hash{}, err
			}
			chain.setPendingView(candidate)
		}
		n.pendingEvents.record(tx.Hash())
		n.logger.Debug("transaction accepted",
			"event", "transaction_accepted",
			"transaction_hash", tx.Hash().Hex(),
			"transaction_type", tx.Type(),
			"nonce", tx.Nonce(),
		)
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
	if mode != miningModeManual {
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
	if count > n.cfg.Limits.MaxControlOperations {
		return nil, newResourceLimitError("block count", count, n.cfg.Limits.MaxControlOperations)
	}
	if err := n.checkFixedListResponse("mine", count, 68); err != nil {
		return nil, err
	}
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		hashes := make([]common.Hash, 0, count)
		var firstNumber uint64
		var lastNumber uint64
		var transactions uint64
		for range count {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			block, _, err := n.mineExecutionBlock(ctx, chain, empty)
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
				"source", miningModeManual,
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

func (n *Node) mineExecutionBlock(ctx context.Context, chain *executionChain, empty bool) (*types.Block, types.Receipts, error) {
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	projection, err := n.consensus.ensureProjection(chain, parent)
	if err != nil {
		return nil, nil, err
	}
	block, receipts, postState, candidate, targetSlot, nativeRequests, err := chain.buildBlock(
		ctx,
		uint64(n.cfg.Chain.SlotDuration/time.Second), empty, common.Hash(projection.Root), n.pendingWithdrawals,
	)
	if err != nil {
		return nil, nil, err
	}
	prepared, err := prepareExecutionRequestBlock(block, nativeRequests, n.pendingExecutionRequests)
	if err != nil {
		return nil, nil, err
	}
	block = prepared.Block
	if err := chain.deriveReceiptFields(block, receipts); err != nil {
		return nil, nil, err
	}
	projectionPut, err := n.consensus.projectionPut(chain, block, prepared.Requests)
	if err != nil {
		return nil, nil, err
	}
	requestRecordPut, err := executionRequestRecordPut(block.Hash(), prepared.Record)
	if err != nil {
		return nil, nil, err
	}
	chain.mu.RLock()
	parentSafety := chain.blockSafety[parent.Hash()]
	timeline := chain.timeline()
	sessionSafety := chain.sessionSafety()
	chain.mu.RUnlock()
	timeline.CurrentSlot = targetSlot
	timeline.LastProcessedSlot = targetSlot
	unsafeReasons := []string(nil)
	if prepared.Controlled {
		unsafeReasons = append(unsafeReasons, taintExecutionRequestControl)
	}
	safety := blockSafetyForChild(parentSafety, block.Hash(), unsafeReasons...)
	if prepared.Controlled {
		sessionSafety = taintStoredSession(sessionSafety, safety, unsafeReasons...)
	}
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
			canonicalSlotPut(targetSlot, block.Hash()), safetyMutation, projectionPut, requestRecordPut,
		},
	}
	blobPuts, err := chain.blobPuts(block)
	if err != nil {
		return nil, nil, err
	}
	operation.Puts = append(operation.Puts, blobPuts...)
	if candidate == nil {
		return nil, nil, errors.New("block builder did not return an isolated candidate state")
	}
	_, root, err := candidate.commit(
		postState, block.NumberU64(), chain.config.Rules(block.Number(), true, block.Time()),
	)
	if err != nil {
		return nil, nil, err
	}
	if root != block.Root() {
		return nil, nil, fmt.Errorf("candidate state root %s does not match block %s", root, block.Root())
	}
	statePuts, err := candidate.puts()
	if err != nil {
		return nil, nil, err
	}
	operation.ExecutionPuts, operation.RollbackDeletes, err = n.planExecutionPuts(chain, statePuts)
	if err != nil {
		return nil, nil, err
	}
	if prepared.Controlled {
		queueMutation, err := executionRequestQueuePut(prepared.Remaining)
		if err != nil {
			return nil, nil, err
		}
		sessionMutation, err := sessionSafetyPut(sessionSafety)
		if err != nil {
			return nil, nil, err
		}
		operation.Puts = append(operation.Puts, queueMutation, sessionMutation)
	}
	events := []Event{{Type: "block", Slot: targetSlot, BlockHash: block.Hash(), BlockNumber: block.NumberU64()}}
	if finalized := n.finalizedEventBetween(timeline.CurrentSlot-1, targetSlot); finalized != nil {
		events = append(events, *finalized)
	}
	if err := n.commitPrepared(chain, operation, events, func() error {
		if err := writeJournalPuts(chain, operation.ExecutionPuts); err != nil {
			return err
		}
		if err := persistBuiltBlock(chain, block, receipts); err != nil {
			return err
		}
		_, setErr := chain.blockchain.SetCanonical(block)
		return setErr
	}, func() {
		chain.applyCanonicalBlock(block, targetSlot)
		chain.mu.Lock()
		chain.blockSafety[block.Hash()] = safety
		if prepared.Controlled {
			chain.sessionTainted = sessionSafety.Tainted
			chain.firstUnsafeBlock = cloneHashPointer(sessionSafety.FirstUnsafeBlock)
			for _, reason := range sessionSafety.Reasons {
				chain.taintReasons[reason] = struct{}{}
			}
		}
		chain.mu.Unlock()
		applyProjectionIndex(chain, block.Hash(), projectionPut)
		n.pendingExecutionRequests = prepared.Remaining
	}); err != nil {
		return nil, nil, err
	}
	n.pendingWithdrawals = nil
	if err := n.rebuildPendingView(ctx, chain); err != nil {
		n.disableWrites(err)
		return nil, nil, fmt.Errorf("block committed but pending view rebuild failed: %w", err)
	}
	return block, receipts, nil
}

func (n *Node) rebuildPendingView(ctx context.Context, chain *executionChain) error {
	candidate, err := n.pendingCandidate(ctx, chain, n.pendingExecutionRequests)
	if err != nil {
		return err
	}
	chain.setPendingView(candidate)
	return nil
}

func (n *Node) MissSlots(ctx context.Context, count uint64) ([]uint64, error) {
	if count > n.cfg.Limits.MaxControlOperations {
		return nil, newResourceLimitError("missed slot count", count, n.cfg.Limits.MaxControlOperations)
	}
	if err := n.checkFixedListResponse("missed slot", count, 1); err != nil {
		return nil, err
	}
	value, err := n.executeWrite(ctx, func(chain *executionChain) (any, error) {
		chain.mu.RLock()
		start := chain.slot
		timeline := chain.timeline()
		chain.mu.RUnlock()
		if count > ^uint64(0)-start {
			return nil, errors.New("missed slot range overflows uint64")
		}
		responseSize := uint64(2)
		for offset := uint64(1); offset <= count; offset++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if offset != 1 {
				responseSize++
			}
			responseSize += uint64(len(strconv.FormatUint(start+offset, 10)))
			if responseSize > uint64(n.cfg.Limits.MaxResponseBytes) {
				return nil, newResourceLimitError(
					"missed slot response bytes", responseSize, uint64(n.cfg.Limits.MaxResponseBytes),
				)
			}
		}
		slots := make([]uint64, 0, count)
		events := make([]Event, 0, count)
		for offset := uint64(1); offset <= count; offset++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			slot := start + offset
			slots = append(slots, slot)
			events = append(events, Event{Type: "missed_slot", Slot: slot})
		}
		if count != 0 {
			timeline.CurrentSlot = start + count
			timeline.LastProcessedSlot = start + count
			pending, err := n.pendingCandidateAtSlot(
				ctx, chain, n.pendingExecutionRequests, start+count+1,
			)
			if err != nil {
				return nil, err
			}
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
				chain.setPendingView(pending)
			}); err != nil {
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
	n.lifecycleMu.Lock()
	if n.lifecycle == nodeLifecycleStopped {
		err := n.closeErr
		n.lifecycleMu.Unlock()
		return err
	}
	if n.closing {
		done := n.closeDone
		n.lifecycleMu.Unlock()
		<-done
		n.lifecycleMu.Lock()
		err := n.closeErr
		n.lifecycleMu.Unlock()
		return err
	}
	n.closing = true
	wasRunning := n.lifecycle == nodeLifecycleRunning || n.lifecycle == nodeLifecycleStarting || n.controllerStarted
	n.lifecycle = nodeLifecycleStopping
	n.running.Store(false)
	n.stopSignal.Do(func() { close(n.stopping) })
	n.rootCancel()
	n.lifecycleMu.Unlock()
	n.handlerMu.Lock()
	n.handlersClosing = true
	n.handlerMu.Unlock()

	if wasRunning {
		n.logger.Info("node stopping", "event", "node_stopping")
	}
	var closeErr error
	if n.rpcServer != nil {
		n.rpcServer.Stop()
	}
	if ipcErr := n.stopIPC(); ipcErr != nil {
		closeErr = errors.Join(closeErr, ipcErr)
		n.logger.Error("IPC server shutdown failed",
			"event", "ipc_shutdown_failed", "endpoint", n.ipcEndpoint, "error", ipcErr,
		)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if n.httpServer != nil {
		if shutdownErr := n.httpServer.Shutdown(shutdownCtx); shutdownErr != nil {
			closeErr = errors.Join(closeErr, shutdownErr)
			n.logger.Error("HTTP server shutdown failed", "event", "http_shutdown_failed", "error", shutdownErr)
			if forceErr := n.httpServer.Close(); forceErr != nil && !errors.Is(forceErr, http.ErrServerClosed) {
				closeErr = errors.Join(closeErr, forceErr)
			}
		}
	}
	cancel()
	n.httpHandlers.Wait()
	if n.controllerStarted {
		<-n.done
	} else {
		n.doneSignal.Do(func() { close(n.done) })
	}
	n.backgroundTasks.Wait()

	head := n.chain.blockchain.CurrentBlock()
	revision := n.Revision()
	if n.cfg.DumpState != "" {
		if dumpErr := n.dumpState(n.cfg.DumpState); dumpErr != nil {
			closeErr = errors.Join(closeErr, dumpErr)
			n.logger.Error("state archive failed", "event", "state_archive_failed", "error", dumpErr)
		}
	}
	if err := n.chain.close(); err != nil {
		closeErr = errors.Join(closeErr, err)
		n.logger.Error("chain storage close failed", "event", "storage_close_failed", "error", err)
	}
	if wasRunning {
		n.logger.Info("node stopped",
			"event", "node_stopped", "head_number", head.Number.Uint64(), "head_hash", head.Hash().Hex(),
			"revision", revision, "uptime", time.Since(n.startedAt).Round(time.Millisecond).String(),
			"clean", closeErr == nil,
		)
	}
	n.lifecycleMu.Lock()
	n.closeErr = closeErr
	n.lifecycle = nodeLifecycleStopped
	close(n.closeDone)
	n.lifecycleMu.Unlock()
	return closeErr
}

func (n *Node) trackHTTPHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.handlerMu.Lock()
		if n.handlersClosing {
			n.handlerMu.Unlock()
			http.Error(w, "node is stopping", http.StatusServiceUnavailable)
			return
		}
		n.httpHandlers.Add(1)
		n.handlerMu.Unlock()
		defer n.httpHandlers.Done()
		next.ServeHTTP(w, r)
	})
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
	IPC       string `json:"ipc"`
}

func (n *Node) Endpoints() Endpoints {
	endpoints := Endpoints{Execution: n.httpEndpoint, IPC: n.ipcEndpoint}
	if n.cfg.Beacon.Enabled && n.httpEndpoint != "" {
		endpoints.Beacon = n.httpEndpoint
	}
	return endpoints
}
