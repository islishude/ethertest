package ethertest

import (
	"context"
	"errors"
	"fmt"
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
	cfg            Config
	chain          *executionChain
	events         *eventLog
	commands       chan command
	stopping       chan struct{}
	done           chan struct{}
	stopOnce       sync.Once
	running        atomic.Bool
	rpcServer      *rpc.Server
	httpServer     *http.Server
	beaconServer   *http.Server
	httpEndpoint   string
	beaconEndpoint string
	nextSnapshot   uint64
	snapshots      map[uint64]*chainPoint
	checkpoints    map[string]*chainPoint
	branches       map[string]*branch
	consensus      *consensusModel
}

type chainPoint struct {
	hash   common.Hash
	number uint64
	used   bool
}

type branch struct {
	name    string
	base    common.Hash
	head    common.Hash
	blocks  []common.Hash
	tainted bool
}

func New(cfg Config) (*Node, error) {
	if cfg.Chain.GenesisTime == 0 {
		cfg.Chain.GenesisTime = time.Now().UTC().Unix()
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	chain, err := newExecutionChain(cfg)
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
		cfg: cfg, chain: chain, events: events,
		commands: make(chan command), stopping: make(chan struct{}), done: make(chan struct{}),
		snapshots: make(map[uint64]*chainPoint), checkpoints: checkpoints,
		branches: branches,
	}
	consensus, err := newConsensusModel(cfg, accountsFromChain(chain))
	if err != nil {
		_ = chain.close()
		return nil, err
	}
	n.consensus = consensus
	return n, nil
}

func accountsFromChain(chain *executionChain) []common.Address {
	addresses := make([]common.Address, len(chain.accounts))
	for i := range chain.accounts {
		addresses[i] = chain.accounts[i].Address
	}
	return addresses
}

func (n *Node) Snapshot(ctx context.Context) (uint64, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		head := chain.blockchain.CurrentBlock()
		n.nextSnapshot++
		n.snapshots[n.nextSnapshot] = &chainPoint{hash: head.Hash(), number: head.Number.Uint64()}
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
		if err := n.switchCanonical(chain, target); err != nil {
			return false, err
		}
		point.used = true
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
		point := &chainPoint{hash: head.Hash(), number: head.Number.Uint64()}
		if err := persistCheckpoint(chain.db, name, point); err != nil {
			return nil, err
		}
		n.checkpoints[name] = point
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
		return nil, n.switchCanonical(chain, target)
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
		close(n.stopping)
		<-n.done
		n.running.Store(false)
		_ = n.chain.close()
		return err
	}
	return nil
}

func (n *Node) run() {
	defer close(n.done)
	var ticker *time.Ticker
	var ticks <-chan time.Time
	if n.cfg.Mining.Mode == "interval" {
		ticker = time.NewTicker(n.cfg.Mining.Interval)
		ticks = ticker.C
		defer ticker.Stop()
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
			return
		case <-ticks:
			if n.chain.pendingCount() == 0 && !n.cfg.Mining.AutoMineEmpty {
				continue
			}
			block, _, err := n.chain.mine(uint64(n.cfg.Chain.SlotDuration/time.Second), false)
			if err == nil {
				_, _ = n.events.append(Event{Type: "block", BlockHash: block.Hash(), BlockNumber: block.NumberU64()})
			}
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
		if n.cfg.Mining.Mode == "transaction" {
			block, _, err := chain.mine(uint64(n.cfg.Chain.SlotDuration/time.Second), false)
			if err != nil {
				return common.Hash{}, err
			}
			if _, err := n.events.append(Event{Type: "block", BlockHash: block.Hash(), BlockNumber: block.NumberU64()}); err != nil {
				return common.Hash{}, err
			}
		}
		return tx.Hash(), nil
	})
	if err != nil {
		return common.Hash{}, err
	}
	return value.(common.Hash), nil
}

func (n *Node) Mine(ctx context.Context, count uint64, empty bool) ([]common.Hash, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		hashes := make([]common.Hash, 0, count)
		for range count {
			block, _, err := chain.mine(uint64(n.cfg.Chain.SlotDuration/time.Second), empty)
			if err != nil {
				return nil, err
			}
			hashes = append(hashes, block.Hash())
			if _, err := n.events.append(Event{Type: "block", BlockHash: block.Hash(), BlockNumber: block.NumberU64()}); err != nil {
				return nil, err
			}
		}
		return hashes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]common.Hash), nil
}

func (n *Node) MissSlots(ctx context.Context, count uint64) ([]uint64, error) {
	value, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		slots := make([]uint64, 0, count)
		for range count {
			slot := chain.missedSlot()
			slots = append(slots, slot)
			if _, err := n.events.append(Event{Type: "missed_slot"}); err != nil {
				return nil, err
			}
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
	out := make([]common.Address, len(n.chain.accounts))
	for i, account := range n.chain.accounts {
		out[i] = account.Address
	}
	return out
}

func (n *Node) Close() error {
	var err error
	n.stopOnce.Do(func() {
		wasRunning := n.running.Swap(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if n.httpServer != nil {
			_ = n.httpServer.Shutdown(shutdownCtx)
		}
		if n.beaconServer != nil {
			_ = n.beaconServer.Shutdown(shutdownCtx)
		}
		if n.rpcServer != nil {
			n.rpcServer.Stop()
		}
		if wasRunning {
			close(n.stopping)
			<-n.done
		}
		if n.cfg.DumpState != "" {
			err = n.dumpState(n.cfg.DumpState)
		}
		if closeErr := n.chain.close(); err == nil {
			err = closeErr
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
	return Endpoints{Execution: n.httpEndpoint, Beacon: n.beaconEndpoint}
}
