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
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	_ "github.com/ethereum/go-ethereum/eth/tracers/native"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

const Version = "0.1.0-alpha.1"

const (
	txSyncDefaultTimeout = 20 * time.Second
	txSyncMaxTimeout     = time.Minute
)

type ethAPI struct {
	node      *Node
	filterMu  sync.Mutex
	filters   map[rpc.ID]*installedFilter
	feeOnce   sync.Once
	feeOracle *gasprice.Oracle
	capMu     sync.Mutex
	capHead   common.Hash
	capOldest uint64
}

type txSyncTimeoutError struct {
	timeout time.Duration
	hash    common.Hash
}

type resourceNotFoundError struct{ message string }

func (err *resourceNotFoundError) Error() string  { return err.message }
func (err *resourceNotFoundError) ErrorCode() int { return -32001 }

type invalidInputError struct{ message string }

func (err *invalidInputError) Error() string  { return err.message }
func (err *invalidInputError) ErrorCode() int { return -32000 }

func (err *txSyncTimeoutError) Error() string {
	return fmt.Sprintf("The transaction was added to the transaction pool but wasn't processed in %v", err.timeout)
}

func (err *txSyncTimeoutError) ErrorCode() int         { return 4 }
func (err *txSyncTimeoutError) ErrorData() interface{} { return err.hash.Hex() }

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
	Key   string       `json:"key"`
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
	Type                 *hexutil.Uint64              `json:"type"`
	From                 *common.Address              `json:"from"`
	To                   *common.Address              `json:"to"`
	Gas                  *hexutil.Uint64              `json:"gas"`
	GasPrice             *hexutil.Big                 `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big                 `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big                 `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big                 `json:"value"`
	Nonce                *hexutil.Uint64              `json:"nonce"`
	Data                 *hexutil.Bytes               `json:"data"`
	Input                *hexutil.Bytes               `json:"input"`
	AccessList           *types.AccessList            `json:"accessList"`
	ChainID              *hexutil.Big                 `json:"chainId"`
	BlobFeeCap           *hexutil.Big                 `json:"maxFeePerBlobGas"`
	BlobHashes           []common.Hash                `json:"blobVersionedHashes,omitempty"`
	Blobs                []kzg4844.Blob               `json:"blobs"`
	Commitments          []kzg4844.Commitment         `json:"commitments"`
	Proofs               []kzg4844.Proof              `json:"proofs"`
	AuthorizationList    []types.SetCodeAuthorization `json:"authorizationList"`
}

// rpcTransaction mirrors geth's internal/ethapi.RPCTransaction. Keeping a
// dedicated wire type avoids exposing geth internals while preserving the
// standard transaction fields expected by RPC clients.
type rpcTransaction struct {
	BlockHash           *common.Hash                 `json:"blockHash"`
	BlockNumber         *hexutil.Big                 `json:"blockNumber"`
	BlockTimestamp      *hexutil.Uint64              `json:"blockTimestamp"`
	From                common.Address               `json:"from"`
	Gas                 hexutil.Uint64               `json:"gas"`
	GasPrice            *hexutil.Big                 `json:"gasPrice"`
	GasFeeCap           *hexutil.Big                 `json:"maxFeePerGas,omitempty"`
	GasTipCap           *hexutil.Big                 `json:"maxPriorityFeePerGas,omitempty"`
	MaxFeePerBlobGas    *hexutil.Big                 `json:"maxFeePerBlobGas,omitempty"`
	Hash                common.Hash                  `json:"hash"`
	Input               hexutil.Bytes                `json:"input"`
	Nonce               hexutil.Uint64               `json:"nonce"`
	To                  *common.Address              `json:"to"`
	TransactionIndex    *hexutil.Uint64              `json:"transactionIndex"`
	Value               *hexutil.Big                 `json:"value"`
	Type                hexutil.Uint64               `json:"type"`
	Accesses            *types.AccessList            `json:"accessList,omitempty"`
	ChainID             *hexutil.Big                 `json:"chainId,omitempty"`
	BlobVersionedHashes []common.Hash                `json:"blobVersionedHashes,omitempty"`
	AuthorizationList   []types.SetCodeAuthorization `json:"authorizationList,omitempty"`
	V                   *hexutil.Big                 `json:"v"`
	R                   *hexutil.Big                 `json:"r"`
	S                   *hexutil.Big                 `json:"s"`
	YParity             *hexutil.Uint64              `json:"yParity,omitempty"`
}

type overrideAccount struct {
	Nonce            *hexutil.Uint64              `json:"nonce"`
	Code             *hexutil.Bytes               `json:"code"`
	Balance          *hexutil.Big                 `json:"balance"`
	State            *map[common.Hash]common.Hash `json:"state"`
	StateDiff        *map[common.Hash]common.Hash `json:"stateDiff"`
	MovePrecompileTo *common.Address              `json:"movePrecompileToAddress"`
}

type stateOverride map[common.Address]overrideAccount

func (n *Node) startServers() error {
	server := rpc.NewServer()
	server.SetBatchLimits(n.cfg.Limits.MaxBatchItems, int(n.cfg.Limits.MaxResponseBytes))
	ethService := &ethAPI{node: n, filters: make(map[rpc.ID]*installedFilter)}
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
		beaconHandler := n.beaconHandler()
		handler := corsHandler(n.cfg.HTTP.CORS, n.cfg.Limits.MaxRequestBytes, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/eth/") {
				if n.cfg.Beacon.Enabled {
					beaconHandler.ServeHTTP(w, r)
				} else {
					http.NotFound(w, r)
				}
				return
			}
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
				n.logger.Error("HTTP server failed",
					"event", "http_server_failed",
					"address", n.httpEndpoint,
					"error", serveErr,
				)
				n.stopSignal.Do(func() { close(n.stopping) })
			}
		}()
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
func (api *ethAPI) Coinbase() common.Address   { return api.node.chain.feeRecipientAddress() }
func (api *ethAPI) Mining() bool               { return api.node.currentMiningMode() != "manual" }
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
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	balance := (*hexutil.Big)(state.GetBalance(address).ToBig())
	return balance, state.Error()
}

func (api *ethAPI) GetTransactionCount(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Uint64, error) {
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return 0, err
	}
	nonce := hexutil.Uint64(state.GetNonce(address))
	return nonce, state.Error()
}

func (api *ethAPI) GetCode(_ context.Context, address common.Address, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	code := state.GetCode(address)
	return code, state.Error()
}

func (api *ethAPI) GetStorageAt(_ context.Context, address common.Address, hexKey string, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	key, err := decodeStorageKey(hexKey)
	if err != nil {
		return nil, &invalidParamsError{message: fmt.Sprintf("%v: %q", err, hexKey)}
	}
	_, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	value := state.GetState(address, key)
	return value[:], state.Error()
}

const maxGetProofKeys = 1024

type orderedProofList []string

func (proof *orderedProofList) Put(_ []byte, value []byte) error {
	*proof = append(*proof, hexutil.Encode(value))
	return nil
}

func (*orderedProofList) Delete([]byte) error { return errors.New("proof deletion is unsupported") }

func (api *ethAPI) GetProof(ctx context.Context, address common.Address, encodedKeys []string, selector rpc.BlockNumberOrHash) (*accountProof, error) {
	if len(encodedKeys) > maxGetProofKeys {
		return nil, &invalidParamsError{message: fmt.Sprintf("too many storage keys requested (max %d, got %d)", maxGetProofKeys, len(encodedKeys))}
	}
	keys := make([]common.Hash, len(encodedKeys))
	keyLengths := make([]int, len(encodedKeys))
	for index, encoded := range encodedKeys {
		var err error
		keys[index], keyLengths[index], err = decodeStorageKeyWithLength(encoded)
		if err != nil {
			return nil, &invalidParamsError{message: fmt.Sprintf("%v: %q", err, encoded)}
		}
	}
	header, statedb, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	accountTrie, err := statedb.Database().OpenTrie(header.Root)
	if err != nil {
		return nil, err
	}
	accountProofNodes := make(orderedProofList, 0)
	if err := accountTrie.Prove(crypto.Keccak256(address.Bytes()), &accountProofNodes); err != nil {
		return nil, err
	}
	storageRoot := statedb.GetStorageRoot(address)
	result := &accountProof{
		Address: address, AccountProof: accountProofNodes,
		Balance:  (*hexutil.Big)(statedb.GetBalance(address).ToBig()),
		CodeHash: statedb.GetCodeHash(address), Nonce: hexutil.Uint64(statedb.GetNonce(address)),
		StorageHash: storageRoot, StorageProof: make([]storageProof, len(keys)),
	}
	var storageTrie state.Trie
	if storageRoot != types.EmptyRootHash && storageRoot != (common.Hash{}) {
		storageTrie, err = statedb.Database().OpenStorageTrie(header.Root, address, storageRoot, nil)
		if err != nil {
			return nil, err
		}
	}
	for index, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outputKey := hexutil.EncodeBig(key.Big())
		if keyLengths[index] == common.HashLength {
			outputKey = hexutil.Encode(key[:])
		}
		proof := make(orderedProofList, 0)
		if storageTrie != nil {
			if err := storageTrie.Prove(crypto.Keccak256(key.Bytes()), &proof); err != nil {
				return nil, err
			}
		}
		value := statedb.GetState(address, key).Big()
		result.StorageProof[index] = storageProof{
			Key: outputKey, Value: (*hexutil.Big)(value), Proof: proof,
		}
	}
	return result, statedb.Error()
}

func (api *ethAPI) SendRawTransaction(ctx context.Context, raw hexutil.Bytes) (common.Hash, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, &tx)
}

func (api *ethAPI) SendRawTransactionSync(ctx context.Context, raw hexutil.Bytes, timeoutMilliseconds *uint64) (map[string]any, error) {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil {
		return nil, err
	}

	revision := api.node.Revision()
	hash, err := api.node.SendTransaction(ctx, &tx)
	if err != nil {
		return nil, err
	}
	if receipt, err := api.transactionReceipt(hash); err != nil || receipt != nil {
		return receipt, err
	}

	timeout := txSyncDefaultTimeout
	if timeoutMilliseconds != nil && *timeoutMilliseconds > 0 {
		maxMilliseconds := uint64(txSyncMaxTimeout / time.Millisecond)
		if *timeoutMilliseconds >= maxMilliseconds {
			timeout = txSyncMaxTimeout
		} else {
			timeout = time.Duration(*timeoutMilliseconds) * time.Millisecond
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		events, changed, err := api.node.events.sinceAndWait(revision)
		if errors.Is(err, ErrEventGap) {
			revision = api.node.Revision()
			if receipt, receiptErr := api.transactionReceipt(hash); receiptErr != nil || receipt != nil {
				return receipt, receiptErr
			}
		} else if err != nil {
			return nil, err
		} else {
			for _, event := range events {
				revision = event.Revision
				if event.Type != "block" || event.Removed {
					continue
				}
				if receipt, err := api.transactionReceipt(hash); err != nil || receipt != nil {
					return receipt, err
				}
			}
		}

		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, &txSyncTimeoutError{timeout: timeout, hash: hash}
			}
			return nil, waitCtx.Err()
		case <-changed:
		case <-api.node.stopping:
			return nil, ErrNodeStopped
		}
	}
}

func (api *ethAPI) SendTransaction(ctx context.Context, args callArgs) (common.Hash, error) {
	tx, err := api.buildAndSignTransaction(ctx, args, false)
	if err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, tx)
}

func (api *ethAPI) Call(ctx context.Context, args callArgs, selector *rpc.BlockNumberOrHash, overrides *stateOverride) (hexutil.Bytes, error) {
	blockSelector := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if selector != nil {
		blockSelector = *selector
	}
	result, err := api.executeCall(ctx, args, blockSelector, 0, overrides)
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
	header, state, err := api.node.resolveState(blockSelector)
	if err != nil {
		return 0, err
	}
	low, high := uint64(21_000), header.GasLimit
	if args.Gas != nil && uint64(*args.Gas) < high {
		high = uint64(*args.Gas)
	}
	for low < high {
		mid := low + (high-low)/2
		result, callErr := api.executeCallAt(ctx, args, header, state, mid, overrides, nil)
		if callErr != nil || result.Failed() {
			low = mid + 1
		} else {
			high = mid
		}
	}
	result, err := api.executeCallAt(ctx, args, header, state, high, overrides, nil)
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
	header, state, err := api.node.resolveState(selector)
	if err != nil {
		return nil, err
	}
	return api.executeCallAt(ctx, args, header, state, gasOverride, overrides, hooks)
}

func (api *ethAPI) executeCallAt(ctx context.Context, args callArgs, header *types.Header, state *state.StateDB, gasOverride uint64, overrides *stateOverride, hooks *tracing.Hooks) (*core.ExecutionResult, error) {
	if err := args.validateData(); err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	state = state.Copy()
	if overrides != nil {
		for address, account := range *overrides {
			if account.State != nil && account.StateDiff != nil {
				return nil, errors.New("state and stateDiff cannot be used together")
			}
			if account.Balance != nil {
				balance, err := checkedU256("balance", (*big.Int)(account.Balance))
				if err != nil {
					return nil, &invalidParamsError{message: err.Error()}
				}
				state.SetBalance(address, balance, tracing.BalanceChangeUnspecified)
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
	gasPrice, feeCap, tipCap := new(big.Int), new(big.Int), new(big.Int)
	if args.GasPrice != nil {
		gasPrice.Set((*big.Int)(args.GasPrice))
		feeCap.Set(gasPrice)
		tipCap.Set(gasPrice)
	} else {
		if args.MaxFeePerGas != nil {
			feeCap.Set((*big.Int)(args.MaxFeePerGas))
		}
		if args.MaxPriorityFeePerGas != nil {
			tipCap.Set((*big.Int)(args.MaxPriorityFeePerGas))
		}
		if header.BaseFee != nil && (feeCap.Sign() != 0 || tipCap.Sign() != 0) {
			gasPrice.Add(header.BaseFee, tipCap)
			if gasPrice.Cmp(feeCap) > 0 {
				gasPrice.Set(feeCap)
			}
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
	nonce := uint64(0)
	if args.Nonce != nil {
		nonce = uint64(*args.Nonce)
	}
	blobFeeCap := new(big.Int)
	if args.BlobFeeCap != nil {
		blobFeeCap.Set((*big.Int)(args.BlobFeeCap))
	}
	valueU256, err := checkedU256("value", value)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	gasPriceU256, err := checkedU256("gas price", gasPrice)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	feeCapU256, err := checkedU256("gas fee cap", feeCap)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	tipCapU256, err := checkedU256("priority fee", tipCap)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	blobFeeCapU256, err := checkedU256("blob fee cap", blobFeeCap)
	if err != nil {
		return nil, &invalidParamsError{message: err.Error()}
	}
	message := &core.Message{
		From: from, To: args.To, Nonce: nonce, Value: valueU256, GasLimit: gas,
		GasPrice: gasPriceU256, GasFeeCap: feeCapU256,
		GasTipCap: tipCapU256, Data: data, AccessList: accessList,
		BlobGasFeeCap: blobFeeCapU256, BlobHashes: args.BlobHashes,
		SetCodeAuthorizations: args.AuthorizationList,
		SkipNonceChecks:       true, SkipTransactionChecks: true,
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
	preEVM := vm.NewEVM(blockContext, state, api.node.chain.config, vm.Config{})
	core.PreExecution(ctx, block.BeaconRoot(), parent, api.node.chain.config, preEVM, block.Number(), block.Time())
	preEVM.Release()
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
		state.SetTxContext(tx.Hash(), index, uint32(index+1))
		stop := context.AfterFunc(ctx, evm.Cancel)
		_, _, applyErr := core.ApplyTransactionWithEVM(
			message, gasPool, state, block.Number(), block.Hash(), block.Time(), tx, evm,
		)
		stop()
		evm.Release()
		if applyErr != nil {
			if uint64(index) == transactionIndex {
				stopTracer(applyErr)
			}
			return nil, applyErr
		}
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
	result := marshalBlock(block, full, api.node.chain.config)
	if number == rpc.PendingBlockNumber {
		for _, field := range []string{"hash", "nonce", "miner"} {
			result[field] = nil
		}
		if full {
			transactions := block.Transactions()
			items := make([]*rpcTransaction, len(transactions))
			for index := range transactions {
				items[index] = newRPCTransaction(
					transactions[index], common.Hash{}, block.NumberU64(), block.Time(), uint64(index), nil, api.node.chain.config,
				)
			}
			result["transactions"] = items
		}
	}
	return result, nil
}

func (api *ethAPI) GetBlockByHash(_ context.Context, hash common.Hash, full bool) map[string]any {
	block := api.node.chain.blockchain.GetBlockByHash(hash)
	if block == nil {
		return nil
	}
	return marshalBlock(block, full, api.node.chain.config)
}

func (api *ethAPI) GetTransactionByHash(_ context.Context, hash common.Hash) *rpcTransaction {
	tx, blockHash, blockNumber, index := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if tx != nil {
		block := api.node.chain.blockchain.GetBlockByHash(blockHash)
		if block == nil {
			return nil
		}
		return newRPCTransaction(tx, blockHash, blockNumber, block.Time(), index, block.BaseFee(), api.node.chain.config)
	}
	if pending := api.node.chain.poolTransaction(hash); pending != nil {
		head := api.node.chain.blockchain.CurrentBlock()
		if block := api.node.chain.pendingBlock(); block != nil {
			head = block.Header()
		}
		return newRPCTransaction(pending, common.Hash{}, head.Number.Uint64(), head.Time, 0, nil, api.node.chain.config)
	}
	return nil
}

func (api *ethAPI) GetTransactionReceipt(_ context.Context, hash common.Hash) (map[string]any, error) {
	return api.transactionReceipt(hash)
}

func (api *ethAPI) transactionReceipt(hash common.Hash) (map[string]any, error) {
	tx, blockHash, blockNumber, index := rawdb.ReadCanonicalTransaction(api.node.chain.db, hash)
	if tx == nil {
		return nil, nil
	}
	receipt, _, _, _ := rawdb.ReadCanonicalReceipt(api.node.chain.db, hash, api.node.chain.config)
	if receipt == nil {
		return nil, nil
	}
	return marshalReceipt(receipt, tx, blockHash, blockNumber, index), nil
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
	pending := make(map[string]map[string]any)
	queued := make(map[string]map[string]any)
	for address, transactions := range api.node.chain.pending {
		for nonce, transaction := range transactions {
			target := api.poolTarget(transaction, pending, queued)
			if target[address.Hex()] == nil {
				target[address.Hex()] = make(map[string]any)
			}
			target[address.Hex()][strconv.FormatUint(nonce, 10)] = api.poolRPCTransaction(transaction)
		}
	}
	return map[string]any{"pending": pending, "queued": queued}
}

func (api *txpoolAPI) ContentFrom(address common.Address) map[string]map[string]any {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	pending := make(map[string]any)
	queued := make(map[string]any)
	for nonce, transaction := range api.node.chain.pending[address] {
		target := queued
		if api.node.chain.pendingView != nil {
			if _, executable := api.node.chain.pendingView.executable[transaction.Hash()]; executable {
				target = pending
			}
		}
		target[strconv.FormatUint(nonce, 10)] = api.poolRPCTransaction(transaction)
	}
	return map[string]map[string]any{"pending": pending, "queued": queued}
}

func (api *txpoolAPI) poolTarget(transaction *types.Transaction, pending, queued map[string]map[string]any) map[string]map[string]any {
	if api.node.chain.pendingView != nil {
		if _, executable := api.node.chain.pendingView.executable[transaction.Hash()]; executable {
			return pending
		}
	}
	return queued
}

func (api *txpoolAPI) poolRPCTransaction(transaction *types.Transaction) *rpcTransaction {
	head := api.node.chain.blockchain.CurrentBlock()
	if api.node.chain.pendingView != nil && api.node.chain.pendingView.block != nil {
		head = api.node.chain.pendingView.block.Header()
	}
	return newRPCTransaction(transaction, common.Hash{}, head.Number.Uint64(), head.Time, 0, nil, api.node.chain.config)
}

func (api *txpoolAPI) poolCounts() (pending, queued int) {
	api.node.chain.mu.RLock()
	defer api.node.chain.mu.RUnlock()
	if api.node.chain.pendingView == nil {
		for _, transactions := range api.node.chain.pending {
			queued += len(transactions)
		}
		return 0, queued
	}
	pending = len(api.node.chain.pendingView.executable)
	queued = len(api.node.chain.pendingView.queued)
	return pending, queued
}

func (api *minerAPI) Start(ctx context.Context, _ *int) (bool, error) {
	_, err := api.node.execute(ctx, func(_ *executionChain) (any, error) {
		mode := api.node.resumeMode()
		api.node.setMiningMode(mode)
		api.node.logger.Info("mining mode changed", "event", "mining_mode_changed", "mode", mode)
		return nil, nil
	})
	return err == nil, err
}
func (api *minerAPI) Stop(ctx context.Context) (bool, error) {
	_, err := api.node.execute(ctx, func(_ *executionChain) (any, error) {
		api.node.setMiningMode("manual")
		api.node.logger.Info("mining mode changed", "event", "mining_mode_changed", "mode", "manual")
		return nil, nil
	})
	return err == nil, err
}

func (api *minerAPI) SetEtherbase(ctx context.Context, address common.Address) (bool, error) {
	_, err := api.node.execute(ctx, func(chain *executionChain) (any, error) {
		chain.setFeeRecipient(address)
		if err := api.node.rebuildPendingView(chain); err != nil {
			return nil, err
		}
		api.node.logger.Info("fee recipient changed",
			"event", "fee_recipient_changed",
			"address", address.Hex(),
		)
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
	transaction, err := service.buildAndSignTransaction(ctx, args, false)
	if err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, transaction)
}

func (api *controlAPI) Capabilities() map[string]any {
	return map[string]any{
		"version": Version, "status": "alpha", "fork": "osaka/fulu", "syntheticFinality": true,
		"consensusMode": "synthetic", "beaconApi": "v4-subset", "fullConsensus": false,
		"forkTransitions": []string{"deneb", "electra", "fulu"},
		"blobCodecs":      []string{"canonical-blob", "packed-bytes-v1"},
		"p2p":             false, "engineAPI": false, "javascriptTracers": false,
		"releaseComplete": false,
	}
}

func (api *controlAPI) SafetyStatus() SafetyStatus {
	return api.node.SafetyStatus()
}

func (api *controlAPI) BlockSafety(hash common.Hash) (BlockSafety, error) {
	safety, err := api.node.BlockSafety(hash)
	if err != nil {
		return BlockSafety{}, &resourceNotFoundError{message: err.Error()}
	}
	return safety, nil
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
	executionAddress := ""
	beaconAddress := ""
	if api.node.cfg.HTTP.Enabled {
		executionAddress = api.node.cfg.HTTP.Address
		if api.node.cfg.Beacon.Enabled {
			beaconAddress = executionAddress
		}
	}
	return map[string]any{
		"chainId": api.node.cfg.Chain.ChainID, "networkId": api.node.cfg.Chain.NetworkID,
		"genesisTime":   api.node.cfg.Chain.GenesisTime,
		"slotDuration":  api.node.cfg.Chain.SlotDuration.String(),
		"slotsPerEpoch": api.node.cfg.Chain.SlotsPerEpoch,
		"el":            executionAddress, "beacon": beaconAddress,
		"consensusMode": "synthetic", "beaconApi": "v4-subset",
		"fullConsensus": false, "releaseComplete": false,
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
			return nil, &resourceNotFoundError{message: "header not found"}
		}
		if selector.RequireCanonical && n.chain.blockchain.GetCanonicalHash(header.Number.Uint64()) != header.Hash() {
			return nil, &invalidInputError{message: "header is not canonical"}
		}
		return header, nil
	}
	number := rpc.LatestBlockNumber
	if selector.BlockNumber != nil {
		number = *selector.BlockNumber
	}
	block, err := n.blockByNumber(number)
	if err != nil || block == nil {
		if err != nil {
			return nil, err
		}
		return nil, &resourceNotFoundError{message: "header not found"}
	}
	return block.Header(), nil
}

func (n *Node) resolveState(selector rpc.BlockNumberOrHash) (*types.Header, *state.StateDB, error) {
	if selector.BlockHash == nil && selector.BlockNumber != nil && *selector.BlockNumber == rpc.PendingBlockNumber {
		block, statedb, _ := n.chain.pendingSnapshot()
		if block == nil || statedb == nil {
			return nil, nil, &resourceNotFoundError{message: "pending state is unavailable"}
		}
		return block.Header(), statedb, nil
	}
	header, err := n.resolveHeader(selector)
	if err != nil {
		return nil, nil, err
	}
	statedb, err := n.chain.blockchain.StateAt(header)
	if err != nil {
		return nil, nil, err
	}
	return header, statedb, nil
}

func (n *Node) blockByNumber(number rpc.BlockNumber) (*types.Block, error) {
	head := n.chain.blockchain.CurrentBlock().Number.Uint64()
	switch number {
	case rpc.LatestBlockNumber:
		return n.chain.blockchain.GetBlockByNumber(head), nil
	case rpc.PendingBlockNumber:
		return n.chain.pendingBlock(), nil
	case rpc.EarliestBlockNumber:
		return n.chain.blockchain.GetBlockByNumber(0), nil
	case rpc.SafeBlockNumber:
		return n.resolveSyntheticFinality(n.chain.currentSlot()).Safe, nil
	case rpc.FinalizedBlockNumber:
		return n.resolveSyntheticFinality(n.chain.currentSlot()).Finalized, nil
	default:
		if number < 0 {
			return nil, fmt.Errorf("unsupported block tag %s", number.String())
		}
		return n.chain.blockchain.GetBlockByNumber(uint64(number)), nil
	}
}

func marshalBlock(block *types.Block, full bool, config *params.ChainConfig) map[string]any {
	headerJSON, _ := headerMap(block.Header())
	result := headerJSON
	result["size"] = hexutil.Uint64(block.Size())
	result["uncles"] = []common.Hash{}
	result["withdrawals"] = block.Withdrawals()
	txs := block.Transactions()
	if full {
		items := make([]*rpcTransaction, len(txs))
		for i, tx := range txs {
			items[i] = newRPCTransaction(
				tx, block.Hash(), block.NumberU64(), block.Time(), uint64(i), block.BaseFee(), config,
			)
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

func newRPCTransaction(
	tx *types.Transaction,
	blockHash common.Hash,
	blockNumber uint64,
	blockTime uint64,
	index uint64,
	baseFee *big.Int,
	config *params.ChainConfig,
) *rpcTransaction {
	signer := types.MakeSigner(config, new(big.Int).SetUint64(blockNumber), blockTime)
	from, _ := types.Sender(signer, tx)
	v, r, s := tx.RawSignatureValues()
	result := &rpcTransaction{
		Type:     hexutil.Uint64(tx.Type()),
		From:     from,
		Gas:      hexutil.Uint64(tx.Gas()),
		GasPrice: (*hexutil.Big)(tx.GasPrice()),
		Hash:     tx.Hash(),
		Input:    hexutil.Bytes(tx.Data()),
		Nonce:    hexutil.Uint64(tx.Nonce()),
		To:       tx.To(),
		Value:    (*hexutil.Big)(tx.Value()),
		V:        (*hexutil.Big)(v),
		R:        (*hexutil.Big)(r),
		S:        (*hexutil.Big)(s),
	}
	if blockHash != (common.Hash{}) {
		result.BlockHash = &blockHash
		result.BlockNumber = (*hexutil.Big)(new(big.Int).SetUint64(blockNumber))
		result.BlockTimestamp = (*hexutil.Uint64)(&blockTime)
		result.TransactionIndex = (*hexutil.Uint64)(&index)
	}

	switch tx.Type() {
	case types.LegacyTxType:
		if chainID := tx.ChainId(); chainID.Sign() != 0 {
			result.ChainID = (*hexutil.Big)(chainID)
		}
	case types.AccessListTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
	case types.DynamicFeeTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		if baseFee != nil && blockHash != (common.Hash{}) {
			result.GasPrice = (*hexutil.Big)(effectiveGasPrice(tx, baseFee))
		} else {
			result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
		}
	case types.BlobTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		if baseFee != nil && blockHash != (common.Hash{}) {
			result.GasPrice = (*hexutil.Big)(effectiveGasPrice(tx, baseFee))
		} else {
			result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
		}
		result.MaxFeePerBlobGas = (*hexutil.Big)(tx.BlobGasFeeCap())
		result.BlobVersionedHashes = tx.BlobHashes()
	case types.SetCodeTxType:
		accessList := tx.AccessList()
		yParity := hexutil.Uint64(v.Sign())
		result.Accesses = &accessList
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yParity
		result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
		result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
		if baseFee != nil && blockHash != (common.Hash{}) {
			result.GasPrice = (*hexutil.Big)(effectiveGasPrice(tx, baseFee))
		} else {
			result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
		}
		result.AuthorizationList = tx.SetCodeAuthorizations()
	}
	return result
}

func effectiveGasPrice(tx *types.Transaction, baseFee *big.Int) *big.Int {
	price := tx.GasTipCap()
	price.Add(price, baseFee)
	if tx.GasFeeCapIntCmp(price) < 0 {
		return tx.GasFeeCap()
	}
	return price
}

func marshalReceipt(receipt *types.Receipt, tx *types.Transaction, blockHash common.Hash, blockNumber, index uint64) map[string]any {
	from, _ := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	logs := receipt.Logs
	if logs == nil {
		logs = []*types.Log{}
	}
	result := map[string]any{
		"blockHash":         blockHash,
		"blockNumber":       hexutil.Uint64(blockNumber),
		"transactionHash":   tx.Hash(),
		"transactionIndex":  hexutil.Uint64(index),
		"from":              from,
		"to":                tx.To(),
		"gasUsed":           hexutil.Uint64(receipt.GasUsed),
		"cumulativeGasUsed": hexutil.Uint64(receipt.CumulativeGasUsed),
		"contractAddress":   nil,
		"logs":              logs,
		"logsBloom":         receipt.Bloom,
		"type":              hexutil.Uint64(tx.Type()),
		"effectiveGasPrice": (*hexutil.Big)(receipt.EffectiveGasPrice),
	}
	if len(receipt.PostState) > 0 {
		result["root"] = hexutil.Bytes(receipt.PostState)
	} else {
		result["status"] = hexutil.Uint64(receipt.Status)
	}
	if receipt.ContractAddress != (common.Address{}) {
		result["contractAddress"] = receipt.ContractAddress
	}
	if tx.Type() == types.BlobTxType {
		result["blobGasUsed"] = hexutil.Uint64(receipt.BlobGasUsed)
		result["blobGasPrice"] = (*hexutil.Big)(receipt.BlobGasPrice)
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
