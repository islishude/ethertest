package ethertest

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/rpc"
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

type resourceNotFoundError struct{ message string }

func (err *resourceNotFoundError) Error() string  { return err.message }
func (err *resourceNotFoundError) ErrorCode() int { return -32001 }

type invalidInputError struct{ message string }

func (err *invalidInputError) Error() string  { return err.message }
func (err *invalidInputError) ErrorCode() int { return -32000 }

type netAPI struct{ node *Node }
type web3API struct{}
type txpoolAPI struct{ node *Node }
type minerAPI struct{ node *Node }
type controlAPI struct{ node *Node }
type withdrawalAPI struct{ node *Node }
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
