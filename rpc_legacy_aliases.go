package ethertest

// Deprecated compatibility extensions: this explicit list freezes the old
// aliases. New ethertest controls must not leak into these namespaces.
import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

func (api *anvilAPI) Capabilities() map[string]any {
	return (&controlAPI{api.node}).Capabilities()
}

func (api *anvilAPI) SafetyStatus() SafetyStatus {
	return (&controlAPI{api.node}).SafetyStatus()
}

func (api *anvilAPI) BlockSafety(hash common.Hash) (BlockSafety, error) {
	return (&controlAPI{api.node}).BlockSafety(hash)
}

func (api *anvilAPI) MineEmpty(ctx context.Context, count *hexutil.Uint64) ([]common.Hash, error) {
	return (&controlAPI{api.node}).MineEmpty(ctx, count)
}

func (api *anvilAPI) MissSlots(ctx context.Context, count hexutil.Uint64) ([]uint64, error) {
	return (&controlAPI{api.node}).MissSlots(ctx, count)
}

func (api *anvilAPI) Snapshot(ctx context.Context) (hexutil.Uint64, error) {
	return (&controlAPI{api.node}).Snapshot(ctx)
}

func (api *anvilAPI) Revert(ctx context.Context, id hexutil.Uint64) (bool, error) {
	return (&controlAPI{api.node}).Revert(ctx, id)
}

func (api *anvilAPI) Checkpoint(ctx context.Context, name string) (bool, error) {
	return (&controlAPI{api.node}).Checkpoint(ctx, name)
}

func (api *anvilAPI) Restore(ctx context.Context, name string) (bool, error) {
	return (&controlAPI{api.node}).Restore(ctx, name)
}

func (api *anvilAPI) BranchCreate(ctx context.Context, name string, number hexutil.Uint64) (bool, error) {
	return (&controlAPI{api.node}).BranchCreate(ctx, name, number)
}

func (api *anvilAPI) BranchMine(ctx context.Context, name string, count hexutil.Uint64) ([]common.Hash, error) {
	return (&controlAPI{api.node}).BranchMine(ctx, name, count)
}

func (api *anvilAPI) BranchSwitch(ctx context.Context, name string) (bool, error) {
	return (&controlAPI{api.node}).BranchSwitch(ctx, name)
}

func (api *anvilAPI) NetworkConfig() map[string]any {
	return (&controlAPI{api.node}).NetworkConfig()
}

func (api *anvilAPI) SetFeeRecipient(ctx context.Context, address common.Address) (bool, error) {
	return (&controlAPI{api.node}).SetFeeRecipient(ctx, address)
}

func (api *evmAPI) Capabilities() map[string]any {
	return (&controlAPI{api.node}).Capabilities()
}

func (api *evmAPI) SafetyStatus() SafetyStatus {
	return (&controlAPI{api.node}).SafetyStatus()
}

func (api *evmAPI) BlockSafety(hash common.Hash) (BlockSafety, error) {
	return (&controlAPI{api.node}).BlockSafety(hash)
}

func (api *evmAPI) MineEmpty(ctx context.Context, count *hexutil.Uint64) ([]common.Hash, error) {
	return (&controlAPI{api.node}).MineEmpty(ctx, count)
}

func (api *evmAPI) MissSlots(ctx context.Context, count hexutil.Uint64) ([]uint64, error) {
	return (&controlAPI{api.node}).MissSlots(ctx, count)
}

func (api *evmAPI) Snapshot(ctx context.Context) (hexutil.Uint64, error) {
	return (&controlAPI{api.node}).Snapshot(ctx)
}

func (api *evmAPI) Revert(ctx context.Context, id hexutil.Uint64) (bool, error) {
	return (&controlAPI{api.node}).Revert(ctx, id)
}

func (api *evmAPI) Checkpoint(ctx context.Context, name string) (bool, error) {
	return (&controlAPI{api.node}).Checkpoint(ctx, name)
}

func (api *evmAPI) Restore(ctx context.Context, name string) (bool, error) {
	return (&controlAPI{api.node}).Restore(ctx, name)
}

func (api *evmAPI) BranchCreate(ctx context.Context, name string, number hexutil.Uint64) (bool, error) {
	return (&controlAPI{api.node}).BranchCreate(ctx, name, number)
}

func (api *evmAPI) BranchMine(ctx context.Context, name string, count hexutil.Uint64) ([]common.Hash, error) {
	return (&controlAPI{api.node}).BranchMine(ctx, name, count)
}

func (api *evmAPI) BranchSwitch(ctx context.Context, name string) (bool, error) {
	return (&controlAPI{api.node}).BranchSwitch(ctx, name)
}

func (api *evmAPI) NetworkConfig() map[string]any {
	return (&controlAPI{api.node}).NetworkConfig()
}

func (api *evmAPI) SetFeeRecipient(ctx context.Context, address common.Address) (bool, error) {
	return (&controlAPI{api.node}).SetFeeRecipient(ctx, address)
}

func (api *evmAPI) SetBalance(ctx context.Context, address common.Address, balance hexutil.Big) (common.Hash, error) {
	return (&controlAPI{api.node}).SetBalance(ctx, address, balance)
}

func (api *evmAPI) SetCode(ctx context.Context, address common.Address, code hexutil.Bytes) (common.Hash, error) {
	return (&controlAPI{api.node}).SetCode(ctx, address, code)
}

func (api *evmAPI) SetNonce(ctx context.Context, address common.Address, nonce hexutil.Uint64) (common.Hash, error) {
	return (&controlAPI{api.node}).SetNonce(ctx, address, nonce)
}

func (api *evmAPI) SetStorageAt(ctx context.Context, address common.Address, key, value common.Hash) (common.Hash, error) {
	return (&controlAPI{api.node}).SetStorageAt(ctx, address, key, value)
}
