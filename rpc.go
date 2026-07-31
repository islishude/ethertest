package ethertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

const Version = "0.1.0-alpha.1"

type ethAPI struct {
	node       *Node
	filterMu   sync.Mutex
	logFilters map[rpc.ID]*installedLogFilter
}
type installedLogFilter struct {
	criteria  filters.FilterCriteria
	nextBlock uint64
}

type accountProof struct {
	Address      common.Address `json:"address"`
	AccountProof []string       `json:"accountProof"`
	Balance      *hexutil.Big   `json:"balance"`
	CodeHash     common.Hash    `json:"codeHash"`
	Nonce        hexutil.Uint64 `json:"nonce"`
	StorageHash  common.Hash    `json:"storageHash"`
	StorageProof []storageProof `json:"storageProof"`
}

type storageProof struct {
	Key   common.Hash  `json:"key"`
	Value *hexutil.Big `json:"value"`
	Proof []string     `json:"proof"`
}
type netAPI struct{ node *Node }
type web3API struct{}
type txpoolAPI struct{ node *Node }
type minerAPI struct{ node *Node }
type controlAPI struct{ node *Node }
type debugAPI struct{ node *Node }
type personalAPI struct{ node *Node }

type callArgs struct {
	From                 *common.Address   `json:"from"`
	To                   *common.Address   `json:"to"`
	Gas                  *hexutil.Uint64   `json:"gas"`
	GasPrice             *hexutil.Big      `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big      `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big      `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big      `json:"value"`
	Nonce                *hexutil.Uint64   `json:"nonce"`
	Data                 *hexutil.Bytes    `json:"data"`
	Input                *hexutil.Bytes    `json:"input"`
	AccessList           *types.AccessList `json:"accessList"`
}

type overrideAccount struct {
	Nonce     *hexutil.Uint64              `json:"nonce"`
	Code      *hexutil.Bytes               `json:"code"`
	Balance   *hexutil.Big                 `json:"balance"`
	State     *map[common.Hash]common.Hash `json:"state"`
	StateDiff *map[common.Hash]common.Hash `json:"stateDiff"`
}

type stateOverride map[common.Address]overrideAccount

func (n *Node) startServers() error {
	server := rpc.NewServer()
	server.SetBatchLimits(n.cfg.Limits.MaxBatchItems, int(n.cfg.Limits.MaxResponseBytes))
	ethService := &ethAPI{node: n, logFilters: make(map[rpc.ID]*installedLogFilter)}
	services := []struct {
		namespace string
		service   any
	}{
		{"eth", ethService},
		{"net", &netAPI{n}},
		{"web3", &web3API{}},
		{"txpool", &txpoolAPI{n}},
		{"miner", &minerAPI{n}},
		{"debug", &debugAPI{n}},
		{"personal", &personalAPI{n}},
		{"ethertest", &controlAPI{n}},
		{"anvil", &controlAPI{n}},
		{"evm", &controlAPI{n}},
	}
	for _, service := range services {
		if err := server.RegisterName(service.namespace, service.service); err != nil {
			server.Stop()
			return err
		}
	}
	n.rpcServer = server
	if n.cfg.HTTP.Enabled {
		listener, err := net.Listen("tcp", n.cfg.HTTP.Address)
		if err != nil {
			server.Stop()
			return err
		}
		scheme := "http"
		if n.cfg.HTTP.TLS.CertFile != "" {
			scheme = "https"
		}
		n.httpEndpoint = scheme + "://" + listener.Addr().String()
		handler := corsHandler(n.cfg.HTTP.CORS, n.cfg.Limits.MaxRequestBytes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				server.WebsocketHandler(n.cfg.HTTP.CORS).ServeHTTP(w, r)
				return
			}
			server.ServeHTTP(w, r)
		}))
		n.httpServer = &http.Server{Addr: n.cfg.HTTP.Address, Handler: handler, ReadHeaderTimeout: 5 * 1e9}
		go func() {
			var serveErr error
			if n.cfg.HTTP.TLS.CertFile != "" {
				serveErr = n.httpServer.ServeTLS(listener, n.cfg.HTTP.TLS.CertFile, n.cfg.HTTP.TLS.KeyFile)
			} else {
				serveErr = n.httpServer.Serve(listener)
			}
			if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				n.stopOnce.Do(func() { close(n.stopping) })
			}
		}()
	}
	if n.cfg.Beacon.Enabled {
		if err := n.startBeaconServer(); err != nil {
			if n.httpServer != nil {
				_ = n.httpServer.Close()
			}
			server.Stop()
			return err
		}
	}
	return nil
}

func corsHandler(origins []string, maxBytes int64, next http.Handler) http.Handler {
	allowAll := false
	allowed := make(map[string]struct{}, len(origins))
	for _, origin := range origins {
		allowed[origin] = struct{}{}
		allowAll = allowAll || origin == "*"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowAll {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if _, ok := allowed[origin]; ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Headers", "content-type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (api *ethAPI) ChainId() hexutil.Uint64 { return hexutil.Uint64(api.node.cfg.Chain.ChainID) }
func (api *ethAPI) BlockNumber() hexutil.Uint64 {
	return hexutil.Uint64(api.node.chain.blockchain.CurrentBlock().Number.Uint64())
}
func (api *ethAPI) Accounts() []common.Address { return api.node.Accounts() }
func (api *ethAPI) Coinbase() common.Address   { return api.node.chain.feeRecipient }
func (api *ethAPI) Mining() bool               { return api.node.cfg.Mining.Mode != "manual" }
func (api *ethAPI) Hashrate() hexutil.Uint64   { return 0 }
func (api *ethAPI) Syncing() bool              { return false }
func (api *ethAPI) GasPrice() *hexutil.Big {
	price := new(big.Int).Add(api.node.chain.blockchain.CurrentBlock().BaseFee, big.NewInt(1_000_000_000))
	return (*hexutil.Big)(price)
}
func (api *ethAPI) MaxPriorityFeePerGas() *hexutil.Big {
	return (*hexutil.Big)(big.NewInt(1_000_000_000))
}

func (api *ethAPI) GetBalance(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	header, err := api.node.resolveHeader(selector)
	if err != nil {
		return nil, err
	}
	state, err := api.node.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, err
	}
	return (*hexutil.Big)(state.GetBalance(address).ToBig()), nil
}

func (api *ethAPI) GetTransactionCount(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	header, err := api.node.resolveHeader(selector)
	if err != nil {
		return 0, err
	}
	state, err := api.node.chain.blockchain.StateAt(header)
	if err != nil {
		return 0, err
	}
	nonce := state.GetNonce(address)
	if selector.BlockNumber != nil && *selector.BlockNumber == rpc.PendingBlockNumber {
		api.node.chain.mu.RLock()
		defer api.node.chain.mu.RUnlock()
		for api.node.chain.pending[address][nonce] != nil {
			nonce++
		}
	}
	return hexutil.Uint64(nonce), nil
}

func (api *ethAPI) GetCode(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	header, err := api.node.resolveHeader(selector)
	if err != nil {
		return nil, err
	}
	state, err := api.node.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, err
	}
	return state.GetCode(address), nil
}

func (api *ethAPI) GetStorageAt(_ context.Context, address common.Address, key common.Hash, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	header, err := api.node.resolveHeader(selector)
	if err != nil {
		return nil, err
	}
	state, err := api.node.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, err
	}
	value := state.GetState(address, key)
	return value[:], nil
}

func (api *ethAPI) GetProof(_ context.Context, address common.Address, keys []common.Hash, selector rpc.BlockNumberOrHash) (*accountProof, error) {
	header, err := api.node.resolveHeader(selector)
	if err != nil {
		return nil, err
	}
	state, err := api.node.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, err
	}
	accountTrie, err := state.Database().OpenTrie(header.Root)
	if err != nil {
		return nil, err
	}
	accountProofDB := memorydb.New()
	if err := accountTrie.Prove(crypto.Keccak256(address.Bytes()), accountProofDB); err != nil {
		return nil, err
	}
	storageRoot := state.GetStorageRoot(address)
	result := &accountProof{
		Address: address, AccountProof: proofValues(accountProofDB),
		Balance:  (*hexutil.Big)(state.GetBalance(address).ToBig()),
		CodeHash: state.GetCodeHash(address), Nonce: hexutil.Uint64(state.GetNonce(address)),
		StorageHash: storageRoot, StorageProof: make([]storageProof, len(keys)),
	}
	storageTrie, err := state.Database().OpenStorageTrie(header.Root, address, storageRoot, nil)
	if err != nil {
		return nil, err
	}
	for index, key := range keys {
		proofDB := memorydb.New()
		if err := storageTrie.Prove(crypto.Keccak256(key.Bytes()), proofDB); err != nil {
			return nil, err
		}
		value := state.GetState(address, key).Big()
		result.StorageProof[index] = storageProof{
			Key: key, Value: (*hexutil.Big)(value), Proof: proofValues(proofDB),
		}
	}
	return result, nil
}

func proofValues(database *memorydb.Database) []string {
	iterator := database.NewIterator(nil, nil)
	defer iterator.Release()
	result := make([]string, 0)
	for iterator.Next() {
		result = append(result, hexutil.Encode(iterator.Value()))
	}
	return result
}

func (api *ethAPI) SendRawTransaction(ctx context.Context, raw hexutil.Bytes) (common.Hash, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, &tx)
}

func (api *ethAPI) SendTransaction(ctx context.Context, args callArgs) (common.Hash, error) {
	tx, err := api.signTransaction(ctx, args)
	if err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, tx)
}

func (api *ethAPI) signTransaction(ctx context.Context, args callArgs) (*types.Transaction, error) {
	if args.From == nil {
		return nil, errors.New("from is required")
	}
	var account *Account
	for index := range api.node.chain.accounts {
		if api.node.chain.accounts[index].Address == *args.From {
			account = &api.node.chain.accounts[index]
			break
		}
	}
	if account == nil {
		return nil, errors.New("unknown unlocked account")
	}
	selector := rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)
	nonce, err := api.GetTransactionCount(ctx, *args.From, selector)
	if err != nil {
		return nil, err
	}
	if args.Nonce != nil {
		nonce = *args.Nonce
	}
	var gas uint64
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	} else {
		estimate, err := api.EstimateGas(ctx, args, nil, nil)
		if err != nil {
			return nil, err
		}
		gas = uint64(estimate) * 120 / 100
	}
	value := new(big.Int)
	if args.Value != nil {
		value.Set((*big.Int)(args.Value))
	}
	tip := big.NewInt(1_000_000_000)
	if args.MaxPriorityFeePerGas != nil {
		tip.Set((*big.Int)(args.MaxPriorityFeePerGas))
	} else if args.GasPrice != nil {
		tip.Set((*big.Int)(args.GasPrice))
	}
	feeCap := new(big.Int).Mul(api.node.chain.blockchain.CurrentBlock().BaseFee, big.NewInt(2))
	feeCap.Add(feeCap, tip)
	if args.MaxFeePerGas != nil {
		feeCap.Set((*big.Int)(args.MaxFeePerGas))
	} else if args.GasPrice != nil {
		feeCap.Set((*big.Int)(args.GasPrice))
	}
	var data []byte
	if args.Input != nil {
		data = *args.Input
	} else if args.Data != nil {
		data = *args.Data
	}
	var accessList types.AccessList
	if args.AccessList != nil {
		accessList = *args.AccessList
	}
	unsigned := types.NewTx(&types.DynamicFeeTx{
		ChainID: api.node.chain.config.ChainID, Nonce: uint64(nonce),
		GasTipCap: tip, GasFeeCap: feeCap, Gas: gas, To: args.To,
		Value: value, Data: data, AccessList: accessList,
	})
	return types.SignTx(unsigned, types.LatestSignerForChainID(api.node.chain.config.ChainID), account.PrivateKey)
}

func (api *ethAPI) Call(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, overrides *stateOverride) (hexutil.Bytes, error) {
	result, err := api.executeCall(ctx, args, selector, 0, overrides)
	if err != nil {
		return nil, err
	}
	if result.Failed() {
		return nil, result.Err
	}
	return result.Return(), nil
}

func (api *ethAPI) EstimateGas(ctx context.Context, args callArgs, selector *rpc.BlockNumberOrHash, overrides *stateOverride) (hexutil.Uint64, error) {
	blockSelector := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if selector != nil {
		blockSelector = *selector
	}
	header, err := api.node.resolveHeader(blockSelector)
	if err != nil {
		return 0, err
	}
	low, high := uint64(21_000), header.GasLimit
	if args.Gas != nil && uint64(*args.Gas) < high {
		high = uint64(*args.Gas)
	}
	for low < high {
		mid := low + (high-low)/2
		result, callErr := api.executeCall(ctx, args, blockSelector, mid, overrides)
		if callErr != nil || result.Failed() {
			low = mid + 1
		} else {
			high = mid
		}
	}
	result, err := api.executeCall(ctx, args, blockSelector, high, overrides)
	if err != nil || result.Failed() {
		if err != nil {
			return 0, err
		}
		return 0, result.Err
	}
	return hexutil.Uint64(high), nil
}

func (api *ethAPI) executeCall(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, gasOverride uint64, overrides *stateOverride) (*core.ExecutionResult, error) {
	return api.executeCallWithTracer(ctx, args, selector, gasOverride, overrides, nil)
}

func (api *ethAPI) executeCallWithTracer(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, gasOverride uint64, overrides *stateOverride, hooks *tracing.Hooks) (*core.ExecutionResult, error) {
	header, err := api.node.resolveHeader(selector)
	if err != nil {
		return nil, err
	}
	state, err := api.node.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, err
	}
	state = state.Copy()
	if overrides != nil {
		for address, account := range *overrides {
			if account.State != nil && account.StateDiff != nil {
				return nil, errors.New("state and stateDiff cannot be used together")
			}
			if account.Balance != nil {
				state.SetBalance(address, uint256.MustFromBig((*big.Int)(account.Balance)), tracing.BalanceChangeUnspecified)
			}
			if account.Nonce != nil {
				state.SetNonce(address, uint64(*account.Nonce), tracing.NonceChangeUnspecified)
			}
			if account.Code != nil {
				state.SetCode(address, *account.Code, tracing.CodeChangeUnspecified)
			}
			if account.State != nil {
				state.SetStorage(address, *account.State)
			}
			if account.StateDiff != nil {
				for key, value := range *account.StateDiff {
					state.SetState(address, key, value)
				}
			}
		}
	}
	from := common.Address{}
	if args.From != nil {
		from = *args.From
	}
	gas := header.GasLimit
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	if gasOverride != 0 {
		gas = gasOverride
	}
	value := new(big.Int)
	if args.Value != nil {
		value = (*big.Int)(args.Value)
	}
	feeCap, tipCap := new(big.Int), new(big.Int)
	if args.GasPrice != nil {
		feeCap.Set((*big.Int)(args.GasPrice))
		tipCap.Set((*big.Int)(args.GasPrice))
	} else {
		if args.MaxFeePerGas != nil {
			feeCap.Set((*big.Int)(args.MaxFeePerGas))
		}
		if args.MaxPriorityFeePerGas != nil {
			tipCap.Set((*big.Int)(args.MaxPriorityFeePerGas))
		}
	}
	data := []byte(nil)
	if args.Input != nil {
		data = *args.Input
	} else if args.Data != nil {
		data = *args.Data
	}
	accessList := types.AccessList(nil)
	if args.AccessList != nil {
		accessList = *args.AccessList
	}
	message := &core.Message{
		From: from, To: args.To, Value: uint256.MustFromBig(value), GasLimit: gas,
		GasPrice: uint256.MustFromBig(feeCap), GasFeeCap: uint256.MustFromBig(feeCap),
		GasTipCap: uint256.MustFromBig(tipCap), Data: data, AccessList: accessList,
		SkipNonceChecks: true, SkipTransactionChecks: true,
	}
	blockContext := core.NewEVMBlockContext(header, api.node.chain.blockchain, nil)
	evm := vm.NewEVM(blockContext, state, api.node.chain.config, vm.Config{NoBaseFee: true, Tracer: hooks})
	evm.SetTxContext(core.NewEVMTxContext(message))
	stop := context.AfterFunc(ctx, evm.Cancel)
	defer stop()
	return core.ApplyMessage(evm, message, core.NewGasPool(gas))
}

type traceConfig struct {
	Tracer           *string         `json:"tracer"`
	TracerConfig     json.RawMessage `json:"tracerConfig"`
	EnableMemory     bool            `json:"enableMemory"`
	DisableStack     bool            `json:"disableStack"`
	DisableStorage   bool            `json:"disableStorage"`
	EnableReturnData bool            `json:"enableReturnData"`
	Limit            int             `json:"limit"`
}

func (api *debugAPI) TraceCall(ctx context.Context, args callArgs, selector rpc.BlockNumberOrHash, config *traceConfig) (json.RawMessage, error) {
	if config == nil || config.Tracer == nil {
		loggerConfig := new(logger.Config)
		if config != nil {
			loggerConfig.EnableMemory = config.EnableMemory
			loggerConfig.DisableStack = config.DisableStack
			loggerConfig.DisableStorage = config.DisableStorage
			loggerConfig.EnableReturnData = config.EnableReturnData
			loggerConfig.Limit = config.Limit
		}
		tracer := logger.NewStructLogger(loggerConfig)
		if _, err := (&ethAPI{node: api.node}).executeCallWithTracer(ctx, args, selector, 0, nil, tracer.Hooks()); err != nil {
			return nil, err
		}
		return tracer.GetResult()
	}
	if tracers.DefaultDirectory.IsJS(*config.Tracer) {
		return nil, errors.New("JavaScript tracers are not supported")
	}
	tracer, err := tracers.DefaultDirectory.New(*config.Tracer, &tracers.Context{}, config.TracerConfig, api.node.chain.config)
	if err != nil {
		return nil, err
	}
	if _, err := (&ethAPI{node: api.node}).executeCallWithTracer(ctx, args, selector, 0, nil, tracer.Hooks); err != nil {
		tracer.Stop(err)
		return nil, err
	}
	return tracer.GetResult()
}

func (api *debugAPI) TraceTransaction(ctx context.Context, hash common.Hash, config *traceConfig) (json.RawMessage, error) {
	transaction, blockHash, blockNumber, transactionIndex := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if transaction == nil {
		return nil, errors.New("transaction not found")
	}
	block := api.node.chain.blockchain.GetBlock(blockHash, blockNumber)
	if block == nil || blockNumber == 0 {
		return nil, errors.New("transaction block is unavailable")
	}
	parent := api.node.chain.blockchain.GetHeaderByHash(block.ParentHash())
	if parent == nil {
		return nil, errors.New("transaction parent is unavailable")
	}
	state, err := api.node.chain.blockchain.StateAt(parent)
	if err != nil {
		return nil, err
	}
	blockContext := core.NewEVMBlockContext(block.Header(), api.node.chain.blockchain, nil)
	gasPool := core.NewGasPool(block.GasLimit())
	signer := types.MakeSigner(api.node.chain.config, block.Number(), block.Time())
	var hooks *tracing.Hooks
	var getResult func() (json.RawMessage, error)
	var stopTracer func(error)
	if config == nil || config.Tracer == nil {
		loggerConfig := new(logger.Config)
		if config != nil {
			loggerConfig.EnableMemory, loggerConfig.DisableStack = config.EnableMemory, config.DisableStack
			loggerConfig.DisableStorage, loggerConfig.EnableReturnData = config.DisableStorage, config.EnableReturnData
			loggerConfig.Limit = config.Limit
		}
		structured := logger.NewStructLogger(loggerConfig)
		hooks, getResult, stopTracer = structured.Hooks(), structured.GetResult, func(error) {}
	} else {
		if tracers.DefaultDirectory.IsJS(*config.Tracer) {
			return nil, errors.New("JavaScript tracers are not supported")
		}
		native, err := tracers.DefaultDirectory.New(*config.Tracer, &tracers.Context{
			BlockHash: blockHash, BlockNumber: new(big.Int).SetUint64(blockNumber),
			TxIndex: int(transactionIndex), TxHash: hash,
		}, config.TracerConfig, api.node.chain.config)
		if err != nil {
			return nil, err
		}
		hooks, getResult, stopTracer = native.Hooks, native.GetResult, native.Stop
	}
	for index, tx := range block.Transactions() {
		message, err := core.TransactionToMessage(tx, signer, block.BaseFee())
		if err != nil {
			return nil, err
		}
		vmConfig := vm.Config{}
		if uint64(index) == transactionIndex {
			vmConfig.Tracer = hooks
		}
		evm := vm.NewEVM(blockContext, state, api.node.chain.config, vmConfig)
		evm.SetTxContext(core.NewEVMTxContext(message))
		stop := context.AfterFunc(ctx, evm.Cancel)
		_, applyErr := core.ApplyMessage(evm, message, gasPool)
		stop()
		if applyErr != nil {
			if uint64(index) == transactionIndex {
				stopTracer(applyErr)
			}
			return nil, applyErr
		}
		state.Finalise(true)
		if uint64(index) == transactionIndex {
			return getResult()
		}
	}
	return nil, errors.New("transaction index is out of bounds")
}

func (api *ethAPI) GetBlockByNumber(_ context.Context, number rpc.BlockNumber, full bool) (map[string]any, error) {
	block, err := api.node.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	return marshalBlock(block, full), nil
}

func (api *ethAPI) GetBlockByHash(_ context.Context, hash common.Hash, full bool) map[string]any {
	block := api.node.chain.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil
	}
	return marshalBlock(block, full)
}

func (api *ethAPI) GetTransactionByHash(_ context.Context, hash common.Hash) map[string]any {
	tx, blockHash, blockNumber, index := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if tx == nil {
		return nil
	}
	return marshalTransaction(tx, &blockHash, &blockNumber, &index)
}

func (api *ethAPI) GetTransactionReceipt(_ context.Context, hash common.Hash) *types.Receipt {
	receipt, _, _, _ := rawdb.ReadCanonicalReceipt(api.node.chain.db, hash, api.node.chain.config)
	return receipt
}

func (api *ethAPI) GetLogs(_ context.Context, criteria filters.FilterCriteria) ([]*types.Log, error) {
	return api.logs(criteria, nil)
}

func (api *ethAPI) NewFilter(criteria filters.FilterCriteria) rpc.ID {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	id := rpc.NewID()
	from := uint64(0)
	if criteria.FromBlock != nil && criteria.FromBlock.Sign() >= 0 {
		from = criteria.FromBlock.Uint64()
	}
	api.logFilters[id] = &installedLogFilter{criteria: criteria, nextBlock: from}
	return id
}

func (api *ethAPI) GetFilterLogs(id rpc.ID) ([]*types.Log, error) {
	api.filterMu.Lock()
	filter := api.logFilters[id]
	api.filterMu.Unlock()
	if filter == nil {
		return nil, errors.New("filter not found")
	}
	return api.logs(filter.criteria, nil)
}

func (api *ethAPI) GetFilterChanges(id rpc.ID) ([]*types.Log, error) {
	api.filterMu.Lock()
	filter := api.logFilters[id]
	if filter == nil {
		api.filterMu.Unlock()
		return nil, errors.New("filter not found")
	}
	from := filter.nextBlock
	head := api.node.chain.blockchain.CurrentBlock().Number.Uint64()
	filter.nextBlock = head + 1
	criteria := filter.criteria
	api.filterMu.Unlock()
	return api.logs(criteria, &from)
}

func (api *ethAPI) UninstallFilter(id rpc.ID) bool {
	api.filterMu.Lock()
	defer api.filterMu.Unlock()
	if api.logFilters[id] == nil {
		return false
	}
	delete(api.logFilters, id)
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
					if event.Type != "block" || event.Removed {
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

func (api *netAPI) Version() string {
	return strconv.FormatUint(api.node.cfg.Chain.NetworkID, 10)
}
func (api *netAPI) Listening() bool         { return false }
func (api *netAPI) PeerCount() hexutil.Uint { return 0 }
func (*web3API) ClientVersion() string      { return "ethertest/" + Version }
func (*web3API) Sha3(input hexutil.Bytes) hexutil.Bytes {
	return cryptoKeccak(input)
}

func (api *txpoolAPI) Status() map[string]hexutil.Uint {
	pending, queued := api.poolCounts()
	return map[string]hexutil.Uint{"pending": hexutil.Uint(pending), "queued": hexutil.Uint(queued)}
}

func (api *txpoolAPI) Content() map[string]any {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	pending := make(map[string]map[string]map[string]any)
	queued := make(map[string]map[string]map[string]any)
	state, _ := api.node.chain.blockchain.State()
	for address, transactions := range api.node.chain.pending {
		frontier := state.GetNonce(address)
		for transactions[frontier] != nil {
			frontier++
		}
		for nonce, transaction := range transactions {
			target := queued
			if nonce < frontier {
				target = pending
			}
			if target[address.Hex()] == nil {
				target[address.Hex()] = make(map[string]map[string]any)
			}
			target[address.Hex()][hexutil.EncodeUint64(nonce)] = marshalTransaction(transaction, nil, nil, nil)
		}
	}
	return map[string]any{"pending": pending, "queued": queued}
}

func (api *txpoolAPI) poolCounts() (pending, queued int) {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	state, err := api.node.chain.blockchain.State()
	if err != nil {
		for _, transactions := range api.node.chain.pending {
			queued += len(transactions)
		}
		return 0, queued
	}
	for address, transactions := range api.node.chain.pending {
		frontier := state.GetNonce(address)
		for transactions[frontier] != nil {
			frontier++
		}
		pendingForAccount := int(frontier - state.GetNonce(address))
		pending += pendingForAccount
		queued += len(transactions) - pendingForAccount
	}
	return pending, queued
}

func (api *minerAPI) Start(_ *int) bool {
	_, _ = api.node.execute(context.Background(), func(_ *executionChain) (any, error) {
		api.node.cfg.Mining.Mode = "transaction"
		return nil, nil
	})
	return true
}
func (api *minerAPI) Stop() bool {
	_, _ = api.node.execute(context.Background(), func(_ *executionChain) (any, error) {
		api.node.cfg.Mining.Mode = "manual"
		return nil, nil
	})
	return true
}

func (api *minerAPI) SetEtherbase(ctx context.Context, address common.Address) (bool, error) {
	_, err := api.node.execute(ctx, func(chain *executionChain) (any, error) {
		chain.feeRecipient = address
		return nil, nil
	})
	return err == nil, err
}

func (api *personalAPI) ListAccounts() []common.Address {
	return api.node.Accounts()
}

func (api *personalAPI) UnlockAccount(address common.Address, _ string, _ *hexutil.Uint64) bool {
	for _, account := range api.node.chain.accounts {
		if account.Address == address {
			return true
		}
	}
	return false
}

func (api *personalAPI) SendTransaction(ctx context.Context, args callArgs, _ string) (common.Hash, error) {
	service := &ethAPI{node: api.node}
	transaction, err := service.signTransaction(ctx, args)
	if err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, transaction)
}

func (api *controlAPI) Capabilities() map[string]any {
	return map[string]any{
		"version": Version, "status": "alpha", "fork": "osaka/fulu", "syntheticFinality": true,
		"forkTransitions": []string{"deneb", "electra", "fulu"},
		"blobCodecs":      []string{"canonical-blob", "packed-bytes-v1"},
		"p2p":             false, "engineAPI": false, "javascriptTracers": false,
		"releaseComplete": false,
	}
}
func (api *controlAPI) Mine(ctx context.Context, count *hexutil.Uint64) ([]common.Hash, error) {
	n := uint64(1)
	if count != nil {
		n = uint64(*count)
	}
	return api.node.Mine(ctx, n, false)
}
func (api *controlAPI) MineEmpty(ctx context.Context, count *hexutil.Uint64) ([]common.Hash, error) {
	n := uint64(1)
	if count != nil {
		n = uint64(*count)
	}
	return api.node.Mine(ctx, n, true)
}
func (api *controlAPI) MissSlots(ctx context.Context, count hexutil.Uint64) ([]uint64, error) {
	return api.node.MissSlots(ctx, uint64(count))
}
func (api *controlAPI) Snapshot(ctx context.Context) (hexutil.Uint64, error) {
	id, err := api.node.Snapshot(ctx)
	return hexutil.Uint64(id), err
}
func (api *controlAPI) Revert(ctx context.Context, id hexutil.Uint64) (bool, error) {
	return api.node.Revert(ctx, uint64(id))
}
func (api *controlAPI) Checkpoint(ctx context.Context, name string) (bool, error) {
	return true, api.node.Checkpoint(ctx, name)
}
func (api *controlAPI) Restore(ctx context.Context, name string) (bool, error) {
	return true, api.node.Restore(ctx, name)
}
func (api *controlAPI) BranchCreate(ctx context.Context, name string, number hexutil.Uint64) (bool, error) {
	return true, api.node.CreateBranch(ctx, name, uint64(number))
}
func (api *controlAPI) BranchMine(ctx context.Context, name string, count hexutil.Uint64) ([]common.Hash, error) {
	return api.node.MineBranch(ctx, name, uint64(count))
}
func (api *controlAPI) BranchSwitch(ctx context.Context, name string) (bool, error) {
	return true, api.node.SwitchBranch(ctx, name)
}
func (api *controlAPI) NetworkConfig() map[string]any {
	return map[string]any{
		"chainId": api.node.cfg.Chain.ChainID, "networkId": api.node.cfg.Chain.NetworkID,
		"genesisTime":   api.node.cfg.Chain.GenesisTime,
		"slotDuration":  api.node.cfg.Chain.SlotDuration.String(),
		"slotsPerEpoch": api.node.cfg.Chain.SlotsPerEpoch,
		"el":            api.node.cfg.HTTP.Address, "beacon": api.node.cfg.Beacon.Address,
	}
}

func (api *controlAPI) SetFeeRecipient(ctx context.Context, address common.Address) (bool, error) {
	return (&minerAPI{node: api.node}).SetEtherbase(ctx, address)
}

func (api *controlAPI) SetBalance(ctx context.Context, address common.Address, balance hexutil.Big) (common.Hash, error) {
	value := new(big.Int).Set((*big.Int)(&balance))
	return api.node.ApplyControl(ctx, ControlChanges{address: {Balance: value}})
}

func (api *controlAPI) SetCode(ctx context.Context, address common.Address, code hexutil.Bytes) (common.Hash, error) {
	value := append([]byte(nil), code...)
	return api.node.ApplyControl(ctx, ControlChanges{address: {Code: &value}})
}

func (api *controlAPI) SetNonce(ctx context.Context, address common.Address, nonce hexutil.Uint64) (common.Hash, error) {
	value := uint64(nonce)
	return api.node.ApplyControl(ctx, ControlChanges{address: {Nonce: &value}})
}

func (api *controlAPI) SetStorageAt(ctx context.Context, address common.Address, key, value common.Hash) (common.Hash, error) {
	storage := map[common.Hash]common.Hash{key: value}
	return api.node.ApplyControl(ctx, ControlChanges{address: {StorageDiff: &storage}})
}

func (n *Node) resolveHeader(selector rpc.BlockNumberOrHash) (*types.Header, error) {
	if selector.BlockHash != nil {
		header := n.chain.blockchain.GetHeaderByHash(*selector.BlockHash)
		if header == nil {
			return nil, errors.New("header not found")
		}
		return header, nil
	}
	number := rpc.LatestBlockNumber
	if selector.BlockNumber != nil {
		number = *selector.BlockNumber
	}
	block, err := n.blockByNumber(number)
	if err != nil || block == nil {
		return nil, err
	}
	return block.Header(), nil
}

func (n *Node) blockByNumber(number rpc.BlockNumber) (*types.Block, error) {
	head := n.chain.blockchain.CurrentBlock().Number.Uint64()
	switch number {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		return n.chain.blockchain.GetBlockByNumber(head), nil
	case rpc.EarliestBlockNumber:
		return n.chain.blockchain.GetBlockByNumber(0), nil
	case rpc.SafeBlockNumber:
		slot := n.chain.currentSlot()
		if slot > n.cfg.Chain.SlotsPerEpoch {
			slot -= n.cfg.Chain.SlotsPerEpoch
		} else {
			slot = 0
		}
		return n.chain.blockAtOrBeforeSlot(slot), nil
	case rpc.FinalizedBlockNumber:
		slot := n.chain.currentSlot()
		lag := 2 * n.cfg.Chain.SlotsPerEpoch
		if slot > lag {
			slot -= lag
		} else {
			slot = 0
		}
		return n.chain.blockAtOrBeforeSlot(slot), nil
	default:
		if number < 0 {
			return nil, fmt.Errorf("unsupported block tag %s", number.String())
		}
		return n.chain.blockchain.GetBlockByNumber(uint64(number)), nil
	}
}

func marshalBlock(block *types.Block, full bool) map[string]any {
	headerJSON, _ := headerMap(block.Header())
	result := headerJSON
	result["size"] = hexutil.Uint64(block.Size())
	result["uncles"] = []common.Hash{}
	result["withdrawals"] = block.Withdrawals()
	txs := block.Transactions()
	if full {
		items := make([]map[string]any, len(txs))
		for i, tx := range txs {
			hash, number, index := block.Hash(), block.NumberU64(), uint64(i)
			items[i] = marshalTransaction(tx, &hash, &number, &index)
		}
		result["transactions"] = items
	} else {
		hashes := make([]common.Hash, len(txs))
		for i, tx := range txs {
			hashes[i] = tx.Hash()
		}
		result["transactions"] = hashes
	}
	return result
}

func headerMap(header *types.Header) (map[string]any, error) {
	data, err := header.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = json.Unmarshal(data, &result)
	return result, err
}

func marshalTransaction(tx *types.Transaction, blockHash *common.Hash, blockNumber, index *uint64) map[string]any {
	data, _ := tx.MarshalJSON()
	var result map[string]any
	_ = json.Unmarshal(data, &result)
	if blockHash != nil {
		result["blockHash"] = blockHash
		result["blockNumber"] = hexutil.Uint64(*blockNumber)
		result["transactionIndex"] = hexutil.Uint64(*index)
	}
	return result
}

func cryptoKeccak(input []byte) []byte {
	// Kept behind a helper so the public RPC surface does not expose geth's
	// crypto package.
	return common.BytesToHash(keccak(input)).Bytes()
}

func keccak(input []byte) []byte {
	return crypto.Keccak256(input)
}
