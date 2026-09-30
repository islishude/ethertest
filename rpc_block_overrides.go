package ethertest

import (
	"bytes"
	"encoding/json"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// callBlockOverrides is intentionally narrower than simulationBlockOverrides:
// withdrawals cannot be applied by a single read-only EVM call.
type callBlockOverrides struct {
	Number        *hexutil.Uint64 `json:"number"`
	Time          *hexutil.Uint64 `json:"time"`
	GasLimit      *hexutil.Uint64 `json:"gasLimit"`
	FeeRecipient  *common.Address `json:"feeRecipient"`
	PrevRandao    *common.Hash    `json:"prevRandao"`
	BaseFeePerGas *hexutil.Big    `json:"baseFeePerGas"`
	BlobBaseFee   *hexutil.Big    `json:"blobBaseFee"`
}

func (o *callBlockOverrides) UnmarshalJSON(data []byte) error {
	type plain callBlockOverrides
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(o))
}
func (o *callBlockOverrides) header(original *types.Header) (*types.Header, error) {
	if o == nil {
		return original, nil
	}
	for name, value := range map[string]*hexutil.Big{"baseFeePerGas": o.BaseFeePerGas, "blobBaseFee": o.BlobBaseFee} {
		if value != nil {
			if _, err := checkedU256(name, (*big.Int)(value)); err != nil {
				return nil, &invalidParamsError{message: err.Error()}
			}
		}
	}
	header := types.CopyHeader(original)
	if o.Number != nil {
		header.Number = new(big.Int).SetUint64(uint64(*o.Number))
	}
	if o.Time != nil {
		header.Time = uint64(*o.Time)
	}
	if o.GasLimit != nil {
		header.GasLimit = uint64(*o.GasLimit)
	}
	if o.FeeRecipient != nil {
		header.Coinbase = *o.FeeRecipient
	}
	if o.PrevRandao != nil {
		header.MixDigest = *o.PrevRandao
	}
	if o.BaseFeePerGas != nil {
		header.BaseFee = new(big.Int).Set((*big.Int)(o.BaseFeePerGas))
	}
	return header, nil
}
func (o *callBlockOverrides) apply(ctx *vm.BlockContext) {
	if o != nil && o.BlobBaseFee != nil {
		ctx.BlobBaseFee = new(big.Int).Set((*big.Int)(o.BlobBaseFee))
	}
}
