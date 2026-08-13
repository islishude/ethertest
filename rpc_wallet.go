package ethertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

type walletAPI struct{ node *Node }

type authorizationArgs struct {
	ChainID *hexutil.Big    `json:"chainId"`
	Address *common.Address `json:"address"`
	Nonce   *hexutil.Uint64 `json:"nonce"`
}

func (api *walletAPI) ImportAccount(ctx context.Context, encodedKey string, balance *hexutil.Big) (ImportAccountResult, error) {
	if !strings.HasPrefix(encodedKey, "0x") {
		return ImportAccountResult{}, errors.New("private key must be 0x-prefixed")
	}
	rawKey, err := hexutil.Decode(encodedKey)
	if err != nil || len(rawKey) != 32 {
		return ImportAccountResult{}, errors.New("private key must encode exactly 32 bytes")
	}
	privateKey, err := crypto.ToECDSA(rawKey)
	if err != nil {
		return ImportAccountResult{}, errors.New("invalid private key")
	}
	var requestedBalance *big.Int
	if balance != nil {
		requestedBalance = new(big.Int).Set((*big.Int)(balance))
	}
	return api.node.ImportAccount(ctx, privateKey, requestedBalance)
}

func (api *walletAPI) RemoveAccount(ctx context.Context, address common.Address) (bool, error) {
	return api.node.RemoveAccount(ctx, address)
}

// SignAuthorization signs a complete EIP-7702 authorization tuple. Nonce and
// chain ID resolution intentionally stay with clients such as viem's
// prepareAuthorization action, avoiding a read-then-sign race in this method.
func (api *walletAPI) SignAuthorization(authority common.Address, args authorizationArgs) (types.SetCodeAuthorization, error) {
	if args.ChainID == nil {
		return types.SetCodeAuthorization{}, &invalidParamsError{message: errAuthorizationChainIDRequired.Error()}
	}
	if args.Address == nil {
		return types.SetCodeAuthorization{}, &invalidParamsError{message: "authorization address is required"}
	}
	if args.Nonce == nil {
		return types.SetCodeAuthorization{}, &invalidParamsError{message: "authorization nonce is required"}
	}
	result, err := api.node.SignAuthorization(authority, AuthorizationRequest{
		ChainID: new(big.Int).Set((*big.Int)(args.ChainID)),
		Address: *args.Address,
		Nonce:   uint64(*args.Nonce),
	})
	if errors.Is(err, errAuthorizationChainIDNegative) ||
		errors.Is(err, errAuthorizationChainIDOverflow) ||
		errors.Is(err, errAuthorizationChainIDMismatch) {
		return types.SetCodeAuthorization{}, &invalidParamsError{message: err.Error()}
	}
	return result, err
}

// SignTypedData_v4 signs an EIP-712 payload. Common providers send either the
// typed-data object itself or a JSON string containing that object.
func (api *ethAPI) SignTypedData_v4(address common.Address, input json.RawMessage) (hexutil.Bytes, error) {
	if !api.node.wallet.contains(address) {
		return nil, errUnknownUnlockedAccount
	}
	typedData, err := decodeTypedData(input)
	if err != nil {
		return nil, err
	}
	if typedData.Domain.ChainId != nil && (*big.Int)(typedData.Domain.ChainId).Cmp(api.node.chain.config.ChainID) != 0 {
		return nil, errors.New("typed data domain chainId does not match node")
	}
	hash, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return nil, err
	}
	signature, err := api.node.wallet.signHash(address, hash)
	if err != nil {
		return nil, err
	}
	signature[crypto.RecoveryIDOffset] += 27
	return signature, nil
}

func decodeTypedData(input json.RawMessage) (apitypes.TypedData, error) {
	payload := bytes.TrimSpace(input)
	if len(payload) == 0 {
		return apitypes.TypedData{}, errors.New("typed data is required")
	}
	if payload[0] == '"' {
		var encoded string
		if err := json.Unmarshal(payload, &encoded); err != nil {
			return apitypes.TypedData{}, errors.New("invalid typed data JSON string")
		}
		payload = []byte(encoded)
	}
	var typedData apitypes.TypedData
	if err := json.Unmarshal(payload, &typedData); err != nil {
		return apitypes.TypedData{}, errors.New("invalid typed data")
	}
	return typedData, nil
}
