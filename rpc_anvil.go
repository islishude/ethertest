package ethertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

type anvilAPI struct{ node *Node }
type evmAPI struct{ node *Node }

// anvilNumber accepts unsigned JSON integers and hexadecimal strings without
// passing through float64 (which loses precision for balances and timestamps).
type anvilNumber big.Int

func (n *anvilNumber) UnmarshalJSON(data []byte) error {
	text := string(data)
	quoted := len(data) > 0 && data[0] == '"'
	base := 10
	if quoted {
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		if len(text) >= 2 && text[0] == '0' {
			switch text[1] {
			case 'x', 'X':
				base = 16
				text = text[2:]
			case 'o', 'O':
				base = 8
				text = text[2:]
			case 'b', 'B':
				base = 2
				text = text[2:]
			}
		}
		text = strings.ReplaceAll(text, "_", "")
		// ruint's string decoder accepts an empty digit sequence as zero.
		if text == "" {
			text = "0"
		}
	}
	if text == "" {
		return errors.New("empty unsigned quantity")
	}
	for _, c := range text {
		digit := int(c - '0')
		if c >= 'a' && c <= 'f' {
			digit = int(c-'a') + 10
		}
		if c >= 'A' && c <= 'F' {
			digit = int(c-'A') + 10
		}
		if digit < 0 || digit >= base {
			return errors.New("invalid unsigned quantity")
		}
	}
	value, ok := new(big.Int).SetString(text, base)
	if !ok || value.BitLen() > 256 {
		return errors.New("quantity exceeds uint256")
	}
	if !quoted && !value.IsUint64() {
		return errors.New("JSON integer exceeds uint64; use a string for uint256")
	}
	*n = anvilNumber(*value)
	return nil
}

func (n *anvilNumber) uint64() (uint64, error) {
	if !(*big.Int)(n).IsUint64() {
		return 0, &invalidParamsError{message: "quantity exceeds uint64"}
	}
	return (*big.Int)(n).Uint64(), nil
}

type evmMineOptions struct {
	Timestamp *anvilNumber `json:"timestamp"`
	Blocks    *anvilNumber `json:"blocks"`
}

func (o *evmMineOptions) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '{' {
		type options evmMineOptions
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode((*options)(o)); err != nil {
			return err
		}
		if o.Timestamp == nil {
			return errors.New("mining options require timestamp")
		}
		return nil
	}
	o.Timestamp = new(anvilNumber)
	return json.Unmarshal(data, o.Timestamp)
}

func (api *anvilAPI) SetBalance(ctx context.Context, address common.Address, balance anvilNumber) error {
	_, err := api.node.ApplyControl(ctx, ControlChanges{address: {Balance: new(big.Int).Set((*big.Int)(&balance))}})
	return err
}
func (api *anvilAPI) SetCode(ctx context.Context, address common.Address, code hexutil.Bytes) error {
	value := []byte(code)
	_, err := api.node.ApplyControl(ctx, ControlChanges{address: {Code: &value}})
	return err
}
func (api *anvilAPI) SetNonce(ctx context.Context, address common.Address, nonce anvilNumber) error {
	value, err := nonce.uint64()
	if err != nil {
		return err
	}
	_, err = api.node.ApplyControl(ctx, ControlChanges{address: {Nonce: &value}})
	return err
}
func (api *anvilAPI) SetStorageAt(ctx context.Context, address common.Address, key anvilNumber, value common.Hash) (bool, error) {
	storage := map[common.Hash]common.Hash{common.BigToHash((*big.Int)(&key)): value}
	_, err := api.node.ApplyControl(ctx, ControlChanges{address: {StorageDiff: &storage}})
	return err == nil, err
}
func (api *anvilAPI) Mine(ctx context.Context, count, interval *anvilNumber) error {
	n := uint64(1)
	if count != nil {
		var err error
		n, err = count.uint64()
		if err != nil {
			return err
		}
	}
	var seconds *uint64
	if interval != nil {
		v, err := interval.uint64()
		if err != nil {
			return err
		}
		seconds = &v
	}
	return api.node.mineCompatible(ctx, n, nil, seconds)
}
func (api *evmAPI) Mine(ctx context.Context, options *evmMineOptions) (string, error) {
	n := uint64(1)
	var timestamp *uint64
	if options != nil {
		if options.Blocks != nil {
			var err error
			n, err = options.Blocks.uint64()
			if err != nil {
				return "", err
			}
		}
		if options.Timestamp != nil {
			v, err := options.Timestamp.uint64()
			if err != nil {
				return "", err
			}
			timestamp = &v
		}
	}
	if err := api.node.mineCompatible(ctx, n, timestamp, nil); err != nil {
		return "", err
	}
	return "0x0", nil
}
func (api *anvilAPI) GetAutomine() bool { return api.node.Automine() }
func (api *anvilAPI) SetAutomine(ctx context.Context, enabled bool) error {
	return api.node.SetAutomine(ctx, enabled)
}
func (api *evmAPI) SetAutomine(ctx context.Context, enabled bool) error {
	return api.node.SetAutomine(ctx, enabled)
}
func (api *anvilAPI) GetIntervalMining() *uint64 {
	interval := api.node.IntervalMining()
	if interval == 0 {
		return nil
	}
	seconds := uint64(interval / time.Second)
	return &seconds
}
func (api *anvilAPI) SetIntervalMining(ctx context.Context, seconds uint64) error {
	return api.node.setIntervalMiningSeconds(ctx, seconds)
}
func (api *evmAPI) SetIntervalMining(ctx context.Context, seconds uint64) error {
	return api.node.setIntervalMiningSeconds(ctx, seconds)
}
func (api *anvilAPI) DropTransaction(ctx context.Context, hash common.Hash) (*common.Hash, error) {
	dropped, err := api.node.DropTransaction(ctx, hash)
	if err != nil || !dropped {
		return nil, err
	}
	return &hash, nil
}
func (api *anvilAPI) DropAllTransactions(ctx context.Context) error {
	return api.node.DropAllTransactions(ctx)
}
func (api *anvilAPI) RemovePoolTransactions(ctx context.Context, address common.Address) error {
	return api.node.RemovePoolTransactions(ctx, address)
}
func (api *anvilAPI) SetCoinbase(ctx context.Context, address common.Address) error {
	return api.node.SetFeeRecipient(ctx, address)
}
func (api *anvilAPI) GetGenesisTime() uint64 { return uint64(api.node.cfg.Chain.GenesisTime) }
