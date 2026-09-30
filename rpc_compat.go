package ethertest

import (
	"context"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

func (api *netAPI) Version() string {
	return strconv.FormatUint(api.node.cfg.Chain.NetworkID, 10)
}
func (api *netAPI) Listening() bool         { return false }
func (api *netAPI) PeerCount() hexutil.Uint { return 0 }
func (*web3API) ClientVersion() string      { return "ethertest/" + Version }
func (*web3API) Sha3(input hexutil.Bytes) hexutil.Bytes {
	return cryptoKeccak(input)
}

func (api *minerAPI) Start(ctx context.Context, _ *int) (bool, error) {
	_, err := api.node.executeWrite(ctx, func(_ *executionChain) (any, error) {
		mode := api.node.resumeMode()
		api.node.setMiningMode(mode)
		api.node.logger.Info("mining mode changed", "event", "mining_mode_changed", "mode", mode)
		return nil, nil
	})
	return err == nil, err
}
func (api *minerAPI) Stop(ctx context.Context) (bool, error) {
	_, err := api.node.executeWrite(ctx, func(_ *executionChain) (any, error) {
		api.node.setMiningMode(miningModeManual)
		api.node.logger.Info("mining mode changed", "event", "mining_mode_changed", "mode", miningModeManual)
		return nil, nil
	})
	return err == nil, err
}

func (api *minerAPI) SetEtherbase(ctx context.Context, address common.Address) (bool, error) {
	err := api.node.SetFeeRecipient(ctx, address)
	return err == nil, err
}

func (api *personalAPI) ListAccounts() []common.Address {
	return api.node.Accounts()
}

func (api *personalAPI) UnlockAccount(address common.Address, _ string, _ *hexutil.Uint64) bool {
	return api.node.wallet.contains(address)
}

func (api *personalAPI) SendTransaction(ctx context.Context, args callArgs, _ string) (common.Hash, error) {
	service := &ethAPI{node: api.node}
	transaction, err := service.buildAndSignTransaction(ctx, args, false)
	if err != nil {
		return common.Hash{}, err
	}
	return api.node.SendTransaction(ctx, transaction)
}

func cryptoKeccak(input []byte) []byte {
	// Kept behind a helper so the public RPC surface does not expose geth's
	// crypto package.
	return common.BytesToHash(keccak(input)).Bytes()
}

func keccak(input []byte) []byte {
	return crypto.Keccak256(input)
}
