package ethertest

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func (api *controlAPI) Capabilities() map[string]any {
	return map[string]any{
		"version": Version, "status": "alpha", "fork": "osaka/fulu", "syntheticFinality": true,
		"finalityControls":     true,
		"authorizationSigning": true,
		"consensusMode":        "synthetic", "beaconApi": "v4-subset", "fullConsensus": false,
		"forkTransitions": []string{"deneb", "electra", "fulu"},
		"blobCodecs":      []string{"canonical-blob", "packed-bytes-v1"},
		"p2p":             false, "engineAPI": false, "javascriptTracers": false,
		"ipc":             true,
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
		"el":            executionAddress, "beacon": beaconAddress, "ipc": api.node.cfg.IPCEndpoint(),
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
