package ethertest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"reflect"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
)

// ResolveConfig loads and validates an explicitly configured execution genesis
// and returns the effective chain fields. network_id = 0 remains an inheritance
// marker until a Node has selected either a fresh or persisted genesis.
func ResolveConfig(cfg Config) (Config, error) {
	resolved, _, err := resolveConfiguredGenesis(cfg)
	return resolved, err
}

func resolveConfiguredGenesis(cfg Config) (Config, *core.Genesis, error) {
	var genesis *core.Genesis
	if cfg.Chain.GenesisFile != "" {
		loaded, err := loadExecutionGenesis(cfg.Chain.GenesisFile)
		if err != nil {
			return Config{}, nil, err
		}
		if err := applyExecutionGenesis(&cfg, loaded); err != nil {
			return Config{}, nil, fmt.Errorf("execution genesis %q: %w", cfg.Chain.GenesisFile, err)
		}
		genesis = loaded
	}
	if err := cfg.validateResolved(); err != nil {
		return Config{}, nil, err
	}
	return cfg, genesis, nil
}

func loadExecutionGenesis(path string) (*core.Genesis, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open execution genesis %q: %w", path, err)
	}
	defer file.Close() //nolint:errcheck

	decoder := json.NewDecoder(file)
	var genesis core.Genesis
	if err := decoder.Decode(&genesis); err != nil {
		return nil, fmt.Errorf("decode execution genesis %q: %w", path, err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); {
	case errors.Is(err, io.EOF):
	case err == nil:
		return nil, fmt.Errorf("decode execution genesis %q: multiple JSON values", path)
	default:
		return nil, fmt.Errorf("decode execution genesis %q: trailing data: %w", path, err)
	}
	return &genesis, nil
}

func applyExecutionGenesis(cfg *Config, genesis *core.Genesis) error {
	forks, err := validateExecutionGenesis(*cfg, genesis)
	if err != nil {
		return err
	}
	cfg.Chain.ChainID = genesis.Config.ChainID.Uint64()
	cfg.Chain.GenesisTime = int64(genesis.Timestamp)
	cfg.Chain.GasLimit = genesis.GasLimit
	cfg.Chain.Forks = forks
	return nil
}

func validateExecutionGenesis(cfg Config, genesis *core.Genesis) (ForkConfig, error) {
	if genesis == nil {
		return ForkConfig{}, errors.New("genesis is nil")
	}
	if cfg.Chain.SlotDuration <= 0 || cfg.Chain.SlotDuration%time.Second != 0 || cfg.Chain.SlotsPerEpoch == 0 {
		return ForkConfig{}, errors.New("slot duration must be positive whole seconds and slots per epoch must be positive")
	}
	if genesis.Config == nil {
		return ForkConfig{}, errors.New("missing config")
	}
	chainConfig := genesis.Config
	if chainConfig.ChainID == nil || chainConfig.ChainID.Sign() <= 0 {
		return ForkConfig{}, errors.New("config.chainId must be positive")
	}
	if chainConfig.ChainID.BitLen() > 64 {
		return ForkConfig{}, errors.New("config.chainId exceeds uint64")
	}
	if genesis.Timestamp > uint64(math.MaxInt64) {
		return ForkConfig{}, errors.New("timestamp exceeds int64")
	}
	if genesis.Number != 0 {
		return ForkConfig{}, errors.New("genesis block number must be zero")
	}
	if genesis.GasLimit == 0 {
		return ForkConfig{}, errors.New("gasLimit must be positive")
	}
	if genesis.Difficulty == nil || genesis.Difficulty.Sign() != 0 {
		return ForkConfig{}, errors.New("difficulty must be zero for a proof-of-stake genesis")
	}
	if chainConfig.Ethash != nil || chainConfig.Clique != nil {
		return ForkConfig{}, errors.New("ethash and clique consensus configurations are unsupported")
	}
	if chainConfig.TerminalTotalDifficulty == nil || chainConfig.TerminalTotalDifficulty.Sign() != 0 {
		return ForkConfig{}, errors.New("terminalTotalDifficulty must be zero")
	}
	if err := chainConfig.CheckConfigForkOrder(); err != nil {
		return ForkConfig{}, fmt.Errorf("invalid fork order: %w", err)
	}
	if err := validateGenesisBlockForks(chainConfig); err != nil {
		return ForkConfig{}, err
	}
	zeroBlock := new(big.Int)
	if !chainConfig.IsLondon(zeroBlock) {
		return ForkConfig{}, errors.New("london must be active at genesis")
	}
	if chainConfig.ShanghaiTime == nil || *chainConfig.ShanghaiTime > genesis.Timestamp {
		return ForkConfig{}, errors.New("shanghai must be active at genesis")
	}
	if chainConfig.CancunTime == nil || *chainConfig.CancunTime > genesis.Timestamp {
		return ForkConfig{}, errors.New("cancun must be active at genesis")
	}
	if chainConfig.PragueTime == nil || chainConfig.OsakaTime == nil {
		return ForkConfig{}, errors.New("both Prague and Osaka activation times are required")
	}
	if chainConfig.EnableUBTAtGenesis || chainConfig.UBTTime != nil ||
		chainConfig.BPO1Time != nil || chainConfig.BPO2Time != nil ||
		chainConfig.BPO3Time != nil || chainConfig.BPO4Time != nil ||
		chainConfig.BPO5Time != nil || chainConfig.AmsterdamTime != nil ||
		chainConfig.BogotaTime != nil {
		return ForkConfig{}, errors.New("post-Osaka forks are outside the v0.1 protocol surface")
	}
	if !matchesRepositoryBlobSchedule(chainConfig.BlobScheduleConfig) {
		return ForkConfig{}, errors.New("blobSchedule must match ethertest's pinned Cancun and Prague parameters")
	}
	if err := validateGenesisSystemContracts(genesis.Alloc); err != nil {
		return ForkConfig{}, err
	}

	cancunEpoch, err := executionForkEpoch("Cancun", genesis.Timestamp, *chainConfig.CancunTime, cfg)
	if err != nil {
		return ForkConfig{}, err
	}
	pragueEpoch, err := executionForkEpoch("Prague", genesis.Timestamp, *chainConfig.PragueTime, cfg)
	if err != nil {
		return ForkConfig{}, err
	}
	osakaEpoch, err := executionForkEpoch("Osaka", genesis.Timestamp, *chainConfig.OsakaTime, cfg)
	if err != nil {
		return ForkConfig{}, err
	}
	forks := ForkConfig{CancunEpoch: cancunEpoch, PragueEpoch: pragueEpoch, OsakaEpoch: osakaEpoch}
	if forks.CancunEpoch != 0 || forks.CancunEpoch > forks.PragueEpoch || forks.PragueEpoch > forks.OsakaEpoch {
		return ForkConfig{}, errors.New("fork epochs must satisfy Cancun = 0 <= Prague <= Osaka")
	}
	return forks, nil
}

func validateGenesisBlockForks(config *params.ChainConfig) error {
	forks := []struct {
		name     string
		block    *big.Int
		required bool
	}{
		{"homesteadBlock", config.HomesteadBlock, true},
		{"daoForkBlock", config.DAOForkBlock, false},
		{"eip150Block", config.EIP150Block, true},
		{"eip155Block", config.EIP155Block, true},
		{"eip158Block", config.EIP158Block, true},
		{"byzantiumBlock", config.ByzantiumBlock, true},
		{"constantinopleBlock", config.ConstantinopleBlock, true},
		{"petersburgBlock", config.PetersburgBlock, true},
		{"istanbulBlock", config.IstanbulBlock, true},
		{"muirGlacierBlock", config.MuirGlacierBlock, false},
		{"berlinBlock", config.BerlinBlock, true},
		{"londonBlock", config.LondonBlock, true},
		{"arrowGlacierBlock", config.ArrowGlacierBlock, false},
		{"grayGlacierBlock", config.GrayGlacierBlock, false},
		{"mergeNetsplitBlock", config.MergeNetsplitBlock, false},
	}
	for _, fork := range forks {
		if fork.block == nil {
			if fork.required {
				return fmt.Errorf("%s must be active at genesis", fork.name)
			}
			continue
		}
		if fork.block.Sign() != 0 {
			return fmt.Errorf("%s must be zero when configured", fork.name)
		}
	}
	return nil
}

func executionForkEpoch(name string, genesisTime, activationTime uint64, cfg Config) (uint64, error) {
	if activationTime <= genesisTime {
		return 0, nil
	}
	slotSeconds := uint64(cfg.Chain.SlotDuration / time.Second)
	if cfg.Chain.SlotsPerEpoch > math.MaxUint64/slotSeconds {
		return 0, errors.New("slot duration and slots per epoch overflow")
	}
	epochSeconds := slotSeconds * cfg.Chain.SlotsPerEpoch
	delta := activationTime - genesisTime
	if delta%epochSeconds != 0 {
		return 0, fmt.Errorf("%s activation time %d is not aligned to an epoch boundary from genesis %d", name, activationTime, genesisTime)
	}
	return delta / epochSeconds, nil
}

func repositoryBlobSchedule() *params.BlobScheduleConfig {
	return &params.BlobScheduleConfig{
		Cancun: &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3_338_477},
		Prague: &params.BlobConfig{Target: 6, Max: 9, UpdateFraction: 5_007_716},
	}
}

func matchesRepositoryBlobSchedule(schedule *params.BlobScheduleConfig) bool {
	want := repositoryBlobSchedule()
	return schedule != nil && reflect.DeepEqual(schedule.Cancun, want.Cancun) &&
		reflect.DeepEqual(schedule.Prague, want.Prague) && schedule.BPO1 == nil &&
		schedule.BPO2 == nil && schedule.BPO3 == nil && schedule.BPO4 == nil && schedule.BPO5 == nil
}

func validateGenesisSystemContracts(alloc types.GenesisAlloc) error {
	for _, contract := range []struct {
		name    string
		address common.Address
		code    []byte
	}{
		{"EIP-4788 beacon roots", params.BeaconRootsAddress, params.BeaconRootsCode},
		{"EIP-2935 history storage", params.HistoryStorageAddress, params.HistoryStorageCode},
		{"EIP-7002 withdrawal queue", params.WithdrawalQueueAddress, params.WithdrawalQueueCode},
		{"EIP-7251 consolidation queue", params.ConsolidationQueueAddress, params.ConsolidationQueueCode},
	} {
		account, exists := alloc[contract.address]
		if !exists {
			return fmt.Errorf("missing %s system contract at %s", contract.name, contract.address)
		}
		if account.Nonce != 1 || account.Balance == nil || account.Balance.Sign() != 0 ||
			!bytes.Equal(account.Code, contract.code) || len(account.Storage) != 0 {
			return fmt.Errorf("invalid %s system contract account at %s", contract.name, contract.address)
		}
	}
	return nil
}

func executionGenesisHash(genesis *core.Genesis) (hash common.Hash, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("calculate genesis hash: %v", recovered)
			hash = common.Hash{}
		}
	}()
	return genesis.ToBlock().Hash(), nil
}

func readPersistedExecutionGenesis(database ethdb.Database) (*core.Genesis, error) {
	genesis, err := core.ReadGenesis(database)
	if err != nil {
		return nil, err
	}
	hash := rawdb.ReadCanonicalHash(database, 0)
	block := rawdb.ReadBlock(database, hash, 0)
	if block == nil {
		return nil, errors.New("genesis block is missing")
	}
	genesis.Number = block.NumberU64()
	genesis.GasUsed = block.GasUsed()
	genesis.ParentHash = block.ParentHash()
	return genesis, nil
}

func compareExecutionGenesis(stored, supplied *core.Genesis) error {
	storedHash, err := executionGenesisHash(stored)
	if err != nil {
		return err
	}
	suppliedHash, err := executionGenesisHash(supplied)
	if err != nil {
		return err
	}
	if storedHash != suppliedHash {
		return fmt.Errorf("genesis hash mismatch: stored %s, supplied %s", storedHash, suppliedHash)
	}
	if !reflect.DeepEqual(stored.Config, supplied.Config) {
		return errors.New("genesis ChainConfig does not match the stored configuration")
	}
	return nil
}
