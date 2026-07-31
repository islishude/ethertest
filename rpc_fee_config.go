// Copyright 2015 The go-ethereum Authors
// Copyright 2026 The ethertest Authors
//
// This file is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This file is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// The EIP-7910 configuration assembly and fee-history integration in this file
// adapt go-ethereum behavior to ethertest's public backend and fork model.

package ethertest

import (
	"context"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

const maxFeeHistoryBlocks = 1024

type feeHistoryResult struct {
	OldestBlock      *hexutil.Big     `json:"oldestBlock"`
	Reward           [][]*hexutil.Big `json:"reward,omitempty"`
	BaseFee          []*hexutil.Big   `json:"baseFeePerGas,omitempty"`
	GasUsedRatio     []float64        `json:"gasUsedRatio"`
	BlobBaseFee      []*hexutil.Big   `json:"baseFeePerBlobGas,omitempty"`
	BlobGasUsedRatio []float64        `json:"blobGasUsedRatio,omitempty"`
}

func (api *ethAPI) FeeHistory(ctx context.Context, blockCount gethmath.HexOrDecimal64, newestBlock rpc.BlockNumber, rewardPercentiles []float64) (*feeHistoryResult, error) {
	api.feeOnce.Do(func() {
		api.feeOracle = gasprice.NewOracle(&rpcFeeBackend{node: api.node}, gasprice.Config{
			Blocks: 20, Percentile: 60, MaxHeaderHistory: maxFeeHistoryBlocks,
			MaxBlockHistory: maxFeeHistoryBlocks, MaxPrice: gasprice.DefaultMaxPrice,
			IgnorePrice: gasprice.DefaultIgnorePrice,
		}, big.NewInt(params.GWei))
	})
	oldest, rewards, baseFees, gasRatios, blobBaseFees, blobRatios, err := api.feeOracle.FeeHistory(
		ctx, uint64(blockCount), newestBlock, rewardPercentiles,
	)
	if err != nil {
		return nil, err
	}
	result := &feeHistoryResult{OldestBlock: (*hexutil.Big)(oldest), GasUsedRatio: gasRatios}
	if rewards != nil {
		result.Reward = make([][]*hexutil.Big, len(rewards))
		for blockIndex, blockRewards := range rewards {
			result.Reward[blockIndex] = make([]*hexutil.Big, len(blockRewards))
			for rewardIndex, reward := range blockRewards {
				result.Reward[blockIndex][rewardIndex] = (*hexutil.Big)(reward)
			}
		}
	}
	if baseFees != nil {
		result.BaseFee = make([]*hexutil.Big, len(baseFees))
		for index, fee := range baseFees {
			result.BaseFee[index] = (*hexutil.Big)(fee)
		}
	}
	if blobBaseFees != nil {
		result.BlobBaseFee = make([]*hexutil.Big, len(blobBaseFees))
		for index, fee := range blobBaseFees {
			result.BlobBaseFee[index] = (*hexutil.Big)(fee)
		}
	}
	if blobRatios != nil {
		result.BlobGasUsedRatio = blobRatios
	}
	return result, nil
}

type rpcFeeBackend struct{ node *Node }

func (backend *rpcFeeBackend) HeaderByNumber(_ context.Context, number rpc.BlockNumber) (*types.Header, error) {
	block, err := backend.node.blockByNumber(number)
	if block == nil || err != nil {
		return nil, err
	}
	return block.Header(), nil
}

func (backend *rpcFeeBackend) BlockByNumber(_ context.Context, number rpc.BlockNumber) (*types.Block, error) {
	return backend.node.blockByNumber(number)
}

func (backend *rpcFeeBackend) GetReceipts(_ context.Context, hash common.Hash) (types.Receipts, error) {
	block := backend.node.chain.blockchain.GetBlockByHash(hash)
	receipts := backend.node.chain.blockchain.GetReceiptsByHash(hash)
	if receipts == nil {
		if block != nil && len(block.Transactions()) == 0 {
			return types.Receipts{}, nil
		}
		return nil, errors.New("receipts are unavailable")
	}
	if block == nil {
		return nil, errors.New("receipt block is unavailable")
	}
	if len(receipts) != len(block.Transactions()) {
		return nil, errors.New("receipts length does not match transactions")
	}
	return receipts, nil
}

func (backend *rpcFeeBackend) Pending() (*types.Block, types.Receipts, *state.StateDB) {
	block, statedb, receipts := backend.node.chain.pendingSnapshot()
	return block, receipts, statedb
}

func (backend *rpcFeeBackend) ChainConfig() *params.ChainConfig {
	return backend.node.chain.config
}

func (backend *rpcFeeBackend) SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription {
	return event.NewSubscription(func(quit <-chan struct{}) error {
		revision := backend.node.Revision()
		for {
			events, changed, err := backend.node.events.sinceAndWait(revision)
			if errors.Is(err, ErrEventGap) {
				revision = backend.node.Revision()
				continue
			}
			if err != nil {
				return err
			}
			for _, chainEvent := range events {
				revision = chainEvent.Revision
				if !isBlockRevisionEvent(chainEvent) || chainEvent.Removed {
					continue
				}
				block := backend.node.chain.blockchain.GetBlockByHash(chainEvent.BlockHash)
				if block == nil {
					continue
				}
				select {
				case ch <- core.ChainHeadEvent{Header: block.Header()}:
				case <-quit:
					return nil
				}
			}
			if len(events) != 0 {
				continue
			}
			select {
			case <-changed:
			case <-quit:
				return nil
			case <-backend.node.stopping:
				return nil
			}
		}
	})
}

type capabilityHead struct {
	Number hexutil.Uint64 `json:"number"`
	Hash   common.Hash    `json:"hash"`
}

type capabilityDeleteStrategy struct {
	Type            string          `json:"type"`
	RetentionBlocks *hexutil.Uint64 `json:"retentionBlocks,omitempty"`
}

type capabilityResource struct {
	Disabled       bool                      `json:"disabled"`
	OldestBlock    *hexutil.Uint64           `json:"oldestBlock,omitempty"`
	DeleteStrategy *capabilityDeleteStrategy `json:"deleteStrategy,omitempty"`
}

type capabilitiesResponse struct {
	Head        capabilityHead     `json:"head"`
	State       capabilityResource `json:"state"`
	Tx          capabilityResource `json:"tx"`
	Logs        capabilityResource `json:"logs"`
	Receipts    capabilityResource `json:"receipts"`
	Blocks      capabilityResource `json:"blocks"`
	StateProofs capabilityResource `json:"stateproofs"`
}

func capabilityOldest(number uint64) *hexutil.Uint64 {
	oldest := hexutil.Uint64(number)
	return &oldest
}

// oldestContiguousState returns the start of the continuously readable state
// range ending at head. Walking backwards is intentional: repeated roots can
// leave an early block readable even when an intermediate, distinct state was
// pruned, so availability is not necessarily monotonic by block number.
func oldestContiguousState(head uint64, available func(uint64) bool) uint64 {
	oldest := head
	for number := head; ; number-- {
		if !available(number) {
			return oldest
		}
		oldest = number
		if number == 0 {
			return 0
		}
	}
}

func (api *ethAPI) Capabilities() *capabilitiesResponse {
	head := api.node.chain.blockchain.CurrentBlock()
	all := capabilityResource{OldestBlock: capabilityOldest(0)}
	stateResource := all
	if !api.node.cfg.Storage.Archive {
		api.capMu.Lock()
		if api.capHead != head.Hash() {
			api.capOldest = oldestContiguousState(head.Number.Uint64(), func(number uint64) bool {
				block := api.node.chain.blockchain.GetBlockByNumber(number)
				return block != nil && api.node.chain.blockchain.HasState(block.Root())
			})
			api.capHead = head.Hash()
		}
		oldest := api.capOldest
		api.capMu.Unlock()
		retention := hexutil.Uint64(state.TriesInMemory)
		stateResource = capabilityResource{
			OldestBlock: capabilityOldest(oldest),
			DeleteStrategy: &capabilityDeleteStrategy{
				Type: "window", RetentionBlocks: &retention,
			},
		}
	}
	return &capabilitiesResponse{
		Head:  capabilityHead{Number: hexutil.Uint64(head.Number.Uint64()), Hash: head.Hash()},
		State: stateResource, Tx: all, Logs: all, Receipts: all, Blocks: all, StateProofs: stateResource,
	}
}

type executionConfig struct {
	ActivationTime  uint64                    `json:"activationTime"`
	BlobSchedule    *params.BlobConfig        `json:"blobSchedule"`
	ChainID         *hexutil.Big              `json:"chainId"`
	ForkID          hexutil.Bytes             `json:"forkId"`
	Precompiles     map[string]common.Address `json:"precompiles"`
	SystemContracts map[string]common.Address `json:"systemContracts"`
}

type executionConfigResponse struct {
	Current *executionConfig `json:"current"`
	Next    *executionConfig `json:"next"`
	Last    *executionConfig `json:"last"`
}

func (api *ethAPI) Config(_ context.Context) (*executionConfigResponse, error) {
	genesis := api.node.chain.blockchain.GetHeaderByNumber(0)
	if genesis == nil {
		return nil, errors.New("unable to load genesis")
	}
	chainConfig := api.node.chain.config
	assemble := func(timestamp *uint64) *executionConfig {
		if timestamp == nil {
			return nil
		}
		forkTime := *timestamp
		activationTime := forkTime
		if genesis.Time >= forkTime {
			activationTime = 0
		}
		rules := chainConfig.Rules(chainConfig.LondonBlock, true, forkTime)
		precompiles := make(map[string]common.Address)
		for address, contract := range vm.ActivePrecompiledContracts(rules) {
			precompiles[contract.Name()] = address
		}
		id := forkid.NewID(chainConfig, types.NewBlockWithHeader(genesis), ^uint64(0), forkTime).Hash
		return &executionConfig{
			ActivationTime: activationTime,
			BlobSchedule:   chainConfig.BlobConfig(chainConfig.LatestFork(forkTime)),
			ChainID:        (*hexutil.Big)(chainConfig.ChainID), ForkID: id[:],
			Precompiles: precompiles, SystemContracts: chainConfig.ActiveSystemContracts(forkTime),
		}
	}
	currentTime := api.node.chain.blockchain.CurrentBlock().Time
	currentFork := chainConfig.LatestFork(currentTime)
	response := &executionConfigResponse{
		Current: assemble(chainConfig.Timestamp(currentFork)),
		Next:    assemble(chainConfig.Timestamp(currentFork + 1)),
		Last:    assemble(chainConfig.Timestamp(chainConfig.LatestFork(^uint64(0)))),
	}
	if response.Next == nil {
		response.Last = nil
	}
	return response, nil
}
