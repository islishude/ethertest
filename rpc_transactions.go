// Copyright 2021 The go-ethereum Authors
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
// Transaction argument normalization in this file follows go-ethereum's
// internal/ethapi transaction builder, adapted to use only public APIs and the
// ethertest account/pending-state model.

package ethertest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	gethaccounts "github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/eth/tracers/logger"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

func (args *callArgs) from() common.Address {
	if args.From == nil {
		return common.Address{}
	}
	return *args.From
}

func (args *callArgs) data() []byte {
	if args.Input != nil {
		return *args.Input
	}
	if args.Data != nil {
		return *args.Data
	}
	return nil
}

func (args *callArgs) validateData() error {
	if args.Data != nil && args.Input != nil && !bytes.Equal(*args.Data, *args.Input) {
		return errors.New(`both "data" and "input" are set and not equal; use "input" for transaction data`)
	}
	return nil
}

func (api *ethAPI) unlockedAccount(address common.Address) (*Account, error) {
	for index := range api.node.chain.accounts {
		if api.node.chain.accounts[index].Address == address {
			return &api.node.chain.accounts[index], nil
		}
	}
	return nil, errors.New("unknown unlocked account")
}

// Sign returns an EIP-191 personal-message signature. The recovery byte is in
// the legacy 27/28 form required by eth_sign.
func (api *ethAPI) Sign(address common.Address, message hexutil.Bytes) (hexutil.Bytes, error) {
	account, err := api.unlockedAccount(address)
	if err != nil {
		return nil, err
	}
	signature, err := crypto.Sign(gethaccounts.TextHash(message), account.PrivateKey)
	if err != nil {
		return nil, err
	}
	signature[crypto.RecoveryIDOffset] += 27
	return signature, nil
}

// SignTransaction signs but does not submit a transaction. EIP-2718 typed
// transactions are returned with their type prefix and blob transactions carry
// their verified sidecar when raw blobs were supplied.
func (api *ethAPI) SignTransaction(ctx context.Context, args callArgs) (hexutil.Bytes, error) {
	tx, err := api.buildAndSignTransaction(ctx, args, true)
	if err != nil {
		return nil, err
	}
	return tx.MarshalBinary()
}

func (api *ethAPI) buildAndSignTransaction(ctx context.Context, args callArgs, requireComplete bool) (*types.Transaction, error) {
	if args.From == nil {
		return nil, errors.New("from is required")
	}
	account, err := api.unlockedAccount(*args.From)
	if err != nil {
		return nil, err
	}
	if err := args.validateData(); err != nil {
		return nil, err
	}
	chainID := api.node.chain.config.ChainID
	if args.ChainID != nil && args.ChainID.ToInt().Cmp(chainID) != 0 {
		return nil, fmt.Errorf("chainId does not match node's (have=%v, want=%v)", args.ChainID.ToInt(), chainID)
	}
	txType, err := transactionType(args)
	if err != nil {
		return nil, err
	}
	if err := validateTransactionFields(txType, &args, false); err != nil {
		return nil, err
	}

	if args.Nonce == nil {
		if requireComplete {
			return nil, errors.New("nonce not specified")
		}
		selector := rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)
		nonce, err := api.GetTransactionCount(ctx, *args.From, selector)
		if err != nil {
			return nil, err
		}
		args.Nonce = &nonce
	}
	if args.Value == nil {
		args.Value = new(hexutil.Big)
	}
	if err := api.fillTransactionFees(&args, txType, requireComplete); err != nil {
		return nil, err
	}
	if txType == types.BlobTxType {
		if err := api.prepareBlobSidecar(&args); err != nil {
			return nil, err
		}
	}
	if args.Gas == nil {
		if requireComplete {
			return nil, errors.New("gas not specified")
		}
		pending := rpc.BlockNumberOrHashWithNumber(rpc.PendingBlockNumber)
		gas, err := api.EstimateGas(ctx, args, &pending, nil)
		if err != nil {
			return nil, err
		}
		args.Gas = &gas
	}

	unsigned, err := makeUnsignedTransaction(txType, &args, chainID)
	if err != nil {
		return nil, err
	}
	signed, err := types.SignTx(unsigned, types.LatestSignerForChainID(chainID), account.PrivateKey)
	if err != nil {
		return nil, err
	}
	if unsigned.BlobTxSidecar() != nil {
		signed = signed.WithBlobTxSidecar(unsigned.BlobTxSidecar())
	}
	return signed, nil
}

func transactionType(args callArgs) (uint8, error) {
	if args.Type != nil {
		txType := uint64(*args.Type)
		if txType > types.SetCodeTxType {
			return 0, fmt.Errorf("unsupported transaction type 0x%x", txType)
		}
		return uint8(txType), nil
	}
	switch {
	case args.AuthorizationList != nil:
		return types.SetCodeTxType, nil
	case args.Blobs != nil || args.BlobHashes != nil || args.BlobFeeCap != nil:
		return types.BlobTxType, nil
	case args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil:
		return types.DynamicFeeTxType, nil
	case args.AccessList != nil:
		return types.AccessListTxType, nil
	case args.GasPrice != nil:
		return types.LegacyTxType, nil
	default:
		return types.DynamicFeeTxType, nil
	}
}

func validateTransactionFields(txType uint8, args *callArgs, allowEmptyCreation bool) error {
	hasDynamicFee := args.MaxFeePerGas != nil || args.MaxPriorityFeePerGas != nil
	hasBlob := args.BlobFeeCap != nil || args.BlobHashes != nil || args.Blobs != nil || args.Commitments != nil || args.Proofs != nil
	hasAuth := args.AuthorizationList != nil
	switch txType {
	case types.LegacyTxType:
		if hasDynamicFee || args.AccessList != nil || hasBlob || hasAuth {
			return errors.New("legacy transaction contains typed-transaction fields")
		}
	case types.AccessListTxType:
		if hasDynamicFee || hasBlob || hasAuth {
			return errors.New("access-list transaction contains incompatible fee, blob, or authorization fields")
		}
	case types.DynamicFeeTxType:
		if args.GasPrice != nil || hasBlob || hasAuth {
			return errors.New("dynamic-fee transaction contains gasPrice, blob, or authorization fields")
		}
	case types.BlobTxType:
		if args.GasPrice != nil || hasAuth {
			return errors.New("blob transaction contains gasPrice or authorization fields")
		}
		if args.To == nil {
			return errors.New(`missing "to" in blob transaction`)
		}
	case types.SetCodeTxType:
		if args.GasPrice != nil || hasBlob {
			return errors.New("set-code transaction contains gasPrice or blob fields")
		}
		if args.To == nil {
			return errors.New(`missing "to" in set-code transaction`)
		}
		if len(args.AuthorizationList) == 0 {
			return errors.New("need at least 1 authorization for a set-code transaction")
		}
	default:
		return fmt.Errorf("unsupported transaction type 0x%x", txType)
	}
	if !allowEmptyCreation && args.To == nil && len(args.data()) == 0 {
		return errors.New("contract creation without any data provided")
	}
	return nil
}

func (api *ethAPI) fillTransactionFees(args *callArgs, txType uint8, requireComplete bool) error {
	head := api.node.chain.blockchain.CurrentBlock()
	if pending := api.node.chain.pendingBlock(); pending != nil {
		head = pending.Header()
	}
	if txType == types.LegacyTxType || txType == types.AccessListTxType {
		if args.GasPrice == nil {
			if requireComplete {
				return errors.New("gasPrice not specified")
			}
			args.GasPrice = (*hexutil.Big)(big.NewInt(1_000_000_000))
		}
		if args.GasPrice.ToInt().Sign() <= 0 {
			return errors.New("gasPrice must be non-zero after london fork")
		}
		return nil
	}
	if requireComplete && (args.MaxFeePerGas == nil || args.MaxPriorityFeePerGas == nil) {
		return errors.New("missing maxFeePerGas or maxPriorityFeePerGas")
	}
	if args.MaxPriorityFeePerGas == nil {
		args.MaxPriorityFeePerGas = (*hexutil.Big)(big.NewInt(1_000_000_000))
	}
	if args.MaxFeePerGas == nil {
		baseFee := new(big.Int)
		if head.BaseFee != nil {
			baseFee.Mul(head.BaseFee, big.NewInt(2))
		}
		baseFee.Add(baseFee, args.MaxPriorityFeePerGas.ToInt())
		args.MaxFeePerGas = (*hexutil.Big)(baseFee)
	}
	if args.MaxFeePerGas.ToInt().Sign() <= 0 {
		return errors.New("maxFeePerGas must be non-zero")
	}
	if args.MaxFeePerGas.ToInt().Cmp(args.MaxPriorityFeePerGas.ToInt()) < 0 {
		return fmt.Errorf("maxFeePerGas (%v) < maxPriorityFeePerGas (%v)", args.MaxFeePerGas.ToInt(), args.MaxPriorityFeePerGas.ToInt())
	}
	if txType == types.BlobTxType {
		if args.BlobFeeCap == nil {
			blobFee := new(big.Int).Mul(eip4844.CalcBlobFee(api.node.chain.config, head), big.NewInt(2))
			args.BlobFeeCap = (*hexutil.Big)(blobFee)
		}
		if args.BlobFeeCap.ToInt().Sign() <= 0 {
			return errors.New("maxFeePerBlobGas must be non-zero")
		}
	}
	return nil
}

func (api *ethAPI) prepareBlobSidecar(args *callArgs) error {
	if args.Blobs == nil {
		if args.Commitments != nil || args.Proofs != nil {
			return errors.New("blob commitments or proofs provided without blobs")
		}
		if len(args.BlobHashes) == 0 {
			return errors.New("need at least 1 blob for a blob transaction")
		}
		if len(args.BlobHashes) > params.BlobTxMaxBlobs {
			return fmt.Errorf("too many blobs in transaction (have=%d, max=%d)", len(args.BlobHashes), params.BlobTxMaxBlobs)
		}
		return nil
	}
	if len(args.Blobs) == 0 {
		return errors.New("need at least 1 blob for a blob transaction")
	}
	if len(args.Blobs) > params.BlobTxMaxBlobs {
		return fmt.Errorf("too many blobs in transaction (have=%d, max=%d)", len(args.Blobs), params.BlobTxMaxBlobs)
	}
	if args.BlobHashes != nil && len(args.BlobHashes) != len(args.Blobs) {
		return fmt.Errorf("number of blobs and hashes mismatch (have=%d, want=%d)", len(args.BlobHashes), len(args.Blobs))
	}

	version := types.BlobSidecarVersion0
	head := api.node.chain.blockchain.CurrentBlock()
	if pending := api.node.chain.pendingBlock(); pending != nil {
		head = pending.Header()
	}
	if api.node.chain.config.IsOsaka(head.Number, head.Time) {
		version = types.BlobSidecarVersion1
	}
	proofCount := len(args.Blobs)
	if version == types.BlobSidecarVersion1 {
		proofCount *= kzg4844.CellProofsPerBlob
	}
	if args.Commitments != nil && len(args.Commitments) != len(args.Blobs) {
		return fmt.Errorf("number of blobs and commitments mismatch (have=%d, want=%d)", len(args.Commitments), len(args.Blobs))
	}
	if args.Proofs != nil && len(args.Proofs) != proofCount {
		return fmt.Errorf("number of blobs and proofs mismatch (have=%d, want=%d)", len(args.Proofs), proofCount)
	}
	if (args.Commitments == nil) != (args.Proofs == nil) {
		return errors.New("blob commitments and proofs must be provided together")
	}
	if args.Commitments == nil {
		args.Commitments = make([]kzg4844.Commitment, len(args.Blobs))
		args.Proofs = make([]kzg4844.Proof, 0, proofCount)
		for index := range args.Blobs {
			commitment, err := kzg4844.BlobToCommitment(&args.Blobs[index])
			if err != nil {
				return fmt.Errorf("blobs[%d]: compute commitment: %w", index, err)
			}
			args.Commitments[index] = commitment
			if version == types.BlobSidecarVersion0 {
				proof, err := kzg4844.ComputeBlobProof(&args.Blobs[index], commitment)
				if err != nil {
					return fmt.Errorf("blobs[%d]: compute proof: %w", index, err)
				}
				args.Proofs = append(args.Proofs, proof)
			} else {
				proofs, err := kzg4844.ComputeCellProofs(&args.Blobs[index])
				if err != nil {
					return fmt.Errorf("blobs[%d]: compute cell proofs: %w", index, err)
				}
				args.Proofs = append(args.Proofs, proofs...)
			}
		}
	}
	if version == types.BlobSidecarVersion0 {
		for index := range args.Blobs {
			if err := kzg4844.VerifyBlobProof(&args.Blobs[index], args.Commitments[index], args.Proofs[index]); err != nil {
				return fmt.Errorf("failed to verify blob proof %d: %w", index, err)
			}
		}
	} else if err := kzg4844.VerifyCellProofs(args.Blobs, args.Commitments, args.Proofs); err != nil {
		return fmt.Errorf("failed to verify blob cell proofs: %w", err)
	}

	hashes := make([]common.Hash, len(args.Commitments))
	hasher := sha256.New()
	for index := range args.Commitments {
		hashes[index] = kzg4844.CalcBlobHashV1(hasher, &args.Commitments[index])
	}
	if args.BlobHashes != nil {
		for index := range hashes {
			if hashes[index] != args.BlobHashes[index] {
				return fmt.Errorf("blob hash verification failed (have=%s, want=%s)", args.BlobHashes[index], hashes[index])
			}
		}
	} else {
		args.BlobHashes = hashes
	}
	return nil
}

func makeUnsignedTransaction(txType uint8, args *callArgs, chainID *big.Int) (*types.Transaction, error) {
	value, err := checkedU256("value", args.Value.ToInt())
	if err != nil {
		return nil, err
	}
	accessList := types.AccessList{}
	if args.AccessList != nil {
		accessList = *args.AccessList
	}
	var inner types.TxData
	switch txType {
	case types.LegacyTxType:
		price, err := checkedU256("gasPrice", args.GasPrice.ToInt())
		if err != nil {
			return nil, err
		}
		inner = &types.LegacyTx{Nonce: uint64(*args.Nonce), GasPrice: price.ToBig(), Gas: uint64(*args.Gas), To: args.To, Value: value.ToBig(), Data: args.data()}
	case types.AccessListTxType:
		price, err := checkedU256("gasPrice", args.GasPrice.ToInt())
		if err != nil {
			return nil, err
		}
		inner = &types.AccessListTx{ChainID: new(big.Int).Set(chainID), Nonce: uint64(*args.Nonce), GasPrice: price.ToBig(), Gas: uint64(*args.Gas), To: args.To, Value: value.ToBig(), Data: args.data(), AccessList: accessList}
	case types.DynamicFeeTxType:
		tip, err := checkedU256("maxPriorityFeePerGas", args.MaxPriorityFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		fee, err := checkedU256("maxFeePerGas", args.MaxFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		inner = &types.DynamicFeeTx{ChainID: new(big.Int).Set(chainID), Nonce: uint64(*args.Nonce), GasTipCap: tip.ToBig(), GasFeeCap: fee.ToBig(), Gas: uint64(*args.Gas), To: args.To, Value: value.ToBig(), Data: args.data(), AccessList: accessList}
	case types.BlobTxType:
		chain, err := checkedU256("chainId", chainID)
		if err != nil {
			return nil, err
		}
		tip, err := checkedU256("maxPriorityFeePerGas", args.MaxPriorityFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		fee, err := checkedU256("maxFeePerGas", args.MaxFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		blobFee, err := checkedU256("maxFeePerBlobGas", args.BlobFeeCap.ToInt())
		if err != nil {
			return nil, err
		}
		blob := &types.BlobTx{ChainID: chain, Nonce: uint64(*args.Nonce), GasTipCap: tip, GasFeeCap: fee, Gas: uint64(*args.Gas), To: *args.To, Value: value, Data: args.data(), AccessList: accessList, BlobFeeCap: blobFee, BlobHashes: args.BlobHashes}
		if args.Blobs != nil {
			version := types.BlobSidecarVersion0
			if len(args.Proofs) == len(args.Blobs)*kzg4844.CellProofsPerBlob {
				version = types.BlobSidecarVersion1
			}
			blob.Sidecar = types.NewBlobTxSidecar(version, args.Blobs, args.Commitments, args.Proofs)
		}
		inner = blob
	case types.SetCodeTxType:
		chain, err := checkedU256("chainId", chainID)
		if err != nil {
			return nil, err
		}
		tip, err := checkedU256("maxPriorityFeePerGas", args.MaxPriorityFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		fee, err := checkedU256("maxFeePerGas", args.MaxFeePerGas.ToInt())
		if err != nil {
			return nil, err
		}
		inner = &types.SetCodeTx{ChainID: chain, Nonce: uint64(*args.Nonce), GasTipCap: tip, GasFeeCap: fee, Gas: uint64(*args.Gas), To: *args.To, Value: value, Data: args.data(), AccessList: accessList, AuthList: args.AuthorizationList}
	default:
		return nil, fmt.Errorf("unsupported transaction type 0x%x", txType)
	}
	return types.NewTx(inner), nil
}

func checkedU256(field string, value *big.Int) (*uint256.Int, error) {
	if value == nil || value.Sign() < 0 {
		return nil, fmt.Errorf("%s must be a non-negative quantity", field)
	}
	result, overflow := uint256.FromBig(value)
	if overflow {
		return nil, fmt.Errorf("%s exceeds 256 bits", field)
	}
	return result, nil
}

type accessListResult struct {
	AccessList types.AccessList `json:"accessList"`
	Error      string           `json:"error,omitempty"`
	GasUsed    hexutil.Uint64   `json:"gasUsed"`
}

func (api *ethAPI) CreateAccessList(ctx context.Context, args callArgs, selector *rpc.BlockNumberOrHash) (*accessListResult, error) {
	if err := args.validateData(); err != nil {
		return nil, err
	}
	block := rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
	if selector != nil {
		block = *selector
	}
	header, statedb, err := api.node.resolveState(block)
	if err != nil {
		return nil, err
	}
	from := args.from()
	if args.Nonce == nil {
		nonce := hexutil.Uint64(statedb.GetNonce(from))
		args.Nonce = &nonce
	}
	excluded := map[common.Address]struct{}{from: {}}
	rules := api.node.chain.config.Rules(header.Number, header.Difficulty.Sign() == 0, header.Time)
	for address := range vm.ActivePrecompiledContracts(rules) {
		excluded[address] = struct{}{}
	}
	if args.To != nil {
		excluded[*args.To] = struct{}{}
	} else {
		excluded[crypto.CreateAddress(from, uint64(*args.Nonce))] = struct{}{}
	}
	gas := header.GasLimit
	if args.Gas != nil {
		gas = uint64(*args.Gas)
	}
	if uint64(len(args.AuthorizationList)) > gas/params.CallNewAccountGas {
		return nil, errors.New("insufficient gas to process all authorizations")
	}
	for index := range args.AuthorizationList {
		authorization := &args.AuthorizationList[index]
		if (!authorization.ChainID.IsZero() && authorization.ChainID.CmpBig(api.node.chain.config.ChainID) != 0) || authorization.Nonce+1 < authorization.Nonce {
			continue
		}
		if authority, err := authorization.Authority(); err == nil {
			excluded[authority] = struct{}{}
		}
	}
	accessList := types.AccessList{}
	if args.AccessList != nil {
		accessList = append(accessList, (*args.AccessList)...)
	}
	for range 128 {
		tracer := logger.NewAccessListTracer(accessList, excluded)
		args.AccessList = &accessList
		result, err := api.executeCallAt(ctx, args, header, statedb, 0, nil, tracer.Hooks())
		if err != nil {
			return nil, err
		}
		next := tracer.AccessList()
		if accessListsEqual(accessList, next) {
			response := &accessListResult{AccessList: next, GasUsed: hexutil.Uint64(result.UsedGas)}
			if result.Failed() {
				response.Error = result.Err.Error()
			}
			return response, nil
		}
		accessList = next
	}
	return nil, errors.New("access list did not converge")
}

func accessListsEqual(left, right types.AccessList) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Address != right[index].Address || len(left[index].StorageKeys) != len(right[index].StorageKeys) {
			return false
		}
		for slot := range left[index].StorageKeys {
			if left[index].StorageKeys[slot] != right[index].StorageKeys[slot] {
				return false
			}
		}
	}
	return true
}
