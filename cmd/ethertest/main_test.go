package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/params"
	"github.com/islishude/ethertest"
	"github.com/urfave/cli/v2"
)

func TestJSONLoggerEmitsStableFieldsAndHonorsOff(t *testing.T) {
	var output bytes.Buffer
	cfg := ethertest.DefaultConfig().Log
	cfg.JSON = true
	logger := newLogger(cfg, &output)
	logger.Info("node started", "event", "node_started")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"time", "level", "msg", "event"} {
		if record[field] == nil {
			t.Errorf("missing JSON log field %q in %#v", field, record)
		}
	}

	output.Reset()
	cfg.Level = "off"
	newLogger(cfg, &output).Error("hidden", "event", "hidden")
	if output.Len() != 0 {
		t.Fatalf("off logger emitted %q", output.String())
	}
}

func TestIPCCommandLineConfiguration(t *testing.T) {
	ipcPath := filepath.Join(t.TempDir(), "cli.ipc")
	ctx := commandContext(t, "--ipc", ipcPath, "--no-http")
	cfg, err := effectiveConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IPC.Enabled || cfg.IPC.Path != ipcPath || cfg.HTTP.Enabled || cfg.Beacon.Enabled {
		t.Fatalf("unexpected CLI configuration: IPC=%#v HTTP=%#v Beacon=%#v", cfg.IPC, cfg.HTTP, cfg.Beacon)
	}
	execution, beacon, ipc := configuredEndpoints(cfg)
	if execution != "" || beacon != "" || ipc != cfg.IPCEndpoint() {
		t.Fatalf("configured endpoints = %q, %q, %q", execution, beacon, ipc)
	}

	t.Setenv("ETHERTEST_IPC_ENABLED", "true")
	disabled, err := effectiveConfig(commandContext(t, "--no-ipc"))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.IPC.Enabled {
		t.Fatal("--no-ipc did not override the environment")
	}

	if _, err := effectiveConfig(commandContext(t, "--ipc", ipcPath, "--no-ipc")); err == nil {
		t.Fatal("expected conflicting IPC flags to fail")
	}
}

func TestGenesisCommandLineConfigurationIsAuthoritative(t *testing.T) {
	path := cliGenesisFile(t, 4242)
	cfg, err := effectiveConfig(commandContext(t,
		"--genesis", path,
		"--chain-id", "1",
		"--genesis-time", "1",
		"--network-id", "777",
	))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Chain.GenesisFile != path || cfg.Chain.ChainID != 4242 || cfg.Chain.GenesisTime != 1_800_000_000 ||
		cfg.Chain.NetworkID != 777 || cfg.Chain.Forks.PragueEpoch != 1 || cfg.Chain.Forks.OsakaEpoch != 2 {
		t.Fatalf("CLI genesis configuration = %#v", cfg.Chain)
	}

	var printed bytes.Buffer
	if err := writeEffectiveConfig(&printed, cfg); err != nil {
		t.Fatal(err)
	}
	var printedConfig ethertest.Config
	if _, err := toml.Decode(printed.String(), &printedConfig); err != nil {
		t.Fatal(err)
	}
	if printedConfig.Chain.ChainID != 4242 || printedConfig.Chain.NetworkID != 777 ||
		printedConfig.Chain.GasLimit != 30_000_000 || printedConfig.Chain.Forks.PragueEpoch != 1 {
		t.Fatalf("printed effective chain configuration = %#v", printedConfig.Chain)
	}
	summary := networkDescription(cfg)
	forkEpochs, ok := summary["forkEpochs"].(map[string]uint64)
	if !ok || summary["chainId"] != uint64(4242) || summary["networkId"] != uint64(777) ||
		summary["gasLimit"] != uint64(30_000_000) || summary["fork"] != "cancun/deneb" ||
		forkEpochs["prague"] != 1 || forkEpochs["osaka"] != 2 {
		t.Fatalf("network description = %#v", summary)
	}

	if _, err := effectiveConfig(commandContext(t, "--genesis", filepath.Join(t.TempDir(), "missing.json"))); err == nil {
		t.Fatal("config validation accepted an unreadable genesis path")
	}
}

func TestGenesisFileConfigurationPrecedenceEndsAtCLI(t *testing.T) {
	tomlGenesis := cliGenesisFile(t, 4100)
	envGenesis := cliGenesisFile(t, 4200)
	cliGenesis := cliGenesisFile(t, 4300)
	configPath := filepath.Join(t.TempDir(), "ethertest.toml")
	if err := os.WriteFile(configPath, fmt.Appendf(nil, "[chain]\ngenesis = %q\n", tomlGenesis), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ETHERTEST_GENESIS", envGenesis)
	cfg, err := effectiveConfig(commandContext(t, "--config", configPath, "--genesis", cliGenesis))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Chain.GenesisFile != cliGenesis || cfg.Chain.ChainID != 4300 {
		t.Fatalf("CLI did not win genesis precedence: %#v", cfg.Chain)
	}
}

func cliGenesisFile(t *testing.T, chainID uint64) string {
	t.Helper()
	genesisTime := uint64(1_800_000_000)
	epochSeconds := uint64(48)
	pragueTime, osakaTime := genesisTime+epochSeconds, genesisTime+2*epochSeconds
	zeroBlock := func() *big.Int { return new(big.Int) }
	zeroTime := uint64(0)
	chainConfig := &params.ChainConfig{
		ChainID:                 new(big.Int).SetUint64(chainID),
		HomesteadBlock:          zeroBlock(),
		EIP150Block:             zeroBlock(),
		EIP155Block:             zeroBlock(),
		EIP158Block:             zeroBlock(),
		ByzantiumBlock:          zeroBlock(),
		ConstantinopleBlock:     zeroBlock(),
		PetersburgBlock:         zeroBlock(),
		IstanbulBlock:           zeroBlock(),
		MuirGlacierBlock:        zeroBlock(),
		BerlinBlock:             zeroBlock(),
		LondonBlock:             zeroBlock(),
		ArrowGlacierBlock:       zeroBlock(),
		GrayGlacierBlock:        zeroBlock(),
		ShanghaiTime:            &zeroTime,
		CancunTime:              &zeroTime,
		PragueTime:              &pragueTime,
		OsakaTime:               &osakaTime,
		TerminalTotalDifficulty: zeroBlock(),
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3_338_477},
			Prague: &params.BlobConfig{Target: 6, Max: 9, UpdateFraction: 5_007_716},
		},
	}
	genesis := core.DeveloperGenesisBlock(30_000_000, nil)
	genesis.Config = chainConfig
	genesis.Timestamp = genesisTime
	encoded, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "genesis.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func commandContext(t *testing.T, arguments ...string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("ethertest-test", flag.ContinueOnError)
	for _, cliFlag := range commonFlags() {
		if err := cliFlag.Apply(set); err != nil {
			t.Fatal(err)
		}
	}
	if err := set.Parse(arguments); err != nil {
		t.Fatal(err)
	}
	return cli.NewContext(cli.NewApp(), set, nil)
}

func TestJSONModeSuppressesDevelopmentAccounts(t *testing.T) {
	var output bytes.Buffer
	cfg := ethertest.DefaultConfig()
	cfg.Log.JSON = true
	printDevelopmentAccounts(&output, cfg)
	if output.Len() != 0 {
		t.Fatalf("JSON logging exposed human account output: %q", output.String())
	}

	cfg.Log.JSON = false
	printDevelopmentAccounts(&output, cfg)
	if !strings.Contains(output.String(), "Unlocked development accounts") {
		t.Fatal("human startup output omitted development accounts")
	}
}
