package ethertest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/params"
)

func TestDefaultChainIDMatchesGethDev(t *testing.T) {
	want := params.AllDevChainProtocolChanges.ChainID.Uint64()
	cfg := DefaultConfig()
	if DefaultChainID != want || cfg.Chain.ChainID != want || cfg.Chain.NetworkID != want {
		t.Fatalf("default chain/network IDs = %d/%d, constant = %d, want geth dev ID %d",
			cfg.Chain.ChainID, cfg.Chain.NetworkID, DefaultChainID, want)
	}
}

func TestDefaultAccountsMatchAnvil(t *testing.T) {
	accounts, err := DeriveAccounts(DefaultMnemonic, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266",
		"0x70997970C51812dc3A010C7d01b50e0d17dc79C8",
	}
	for i, account := range accounts {
		if account.Address.Hex() != want[i] {
			t.Fatalf("account %d: have %s want %s", i, account.Address.Hex(), want[i])
		}
	}
}

func TestStrictTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(path, []byte("[chain]\nunknown = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected unknown key error")
	}
}

func TestExternalBindingRequiresOptIn(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.Address = "0.0.0.0:8545"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unsafe external binding error")
	}
	cfg.HTTP.AllowUnsafeExternal = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !isLoopbackAddress("127.0.0.1:8545") || !isLoopbackAddress("[::1]:8545") ||
		isLoopbackAddress("0.0.0.0:8545") {
		t.Fatal("loopback listener classification is incorrect")
	}
}

func TestTLSMaterialFailsClosedDuringValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.TLS.CertFile = filepath.Join(t.TempDir(), "missing.crt")
	cfg.HTTP.TLS.KeyFile = filepath.Join(t.TempDir(), "missing.key")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing TLS material to fail validation")
	}
}

func TestBeaconRequiresSharedHTTPListener(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.Enabled = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Beacon without the shared HTTP listener to fail validation")
	}
	cfg.Beacon.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyBeaconListenerConfigurationIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-beacon.toml")
	if err := os.WriteFile(path, []byte("[beacon]\naddress = \"127.0.0.1:5052\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("expected legacy Beacon address to be rejected")
	}

	t.Setenv("ETHERTEST_BEACON_ADDRESS", "127.0.0.1:5052")
	if _, err := ReadConfig(""); err == nil {
		t.Fatal("expected legacy Beacon environment key to be rejected")
	}
}

func TestEnvironmentOverridesAndRejectsUnknownKeys(t *testing.T) {
	t.Setenv("ETHERTEST_CHAIN_ID", "4242")
	t.Setenv("ETHERTEST_SLOT_DURATION", "3s")
	t.Setenv("ETHERTEST_LOG_LEVEL", "debug")
	t.Setenv("ETHERTEST_LOG_PROGRESS_INTERVAL", "15s")
	cfg, err := ReadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Chain.ChainID != 4242 || cfg.Chain.SlotDuration != 3*time.Second ||
		cfg.Log.Level != "debug" || cfg.Log.ProgressInterval != 15*time.Second {
		t.Fatalf("environment overrides not applied: chain=%#v log=%#v", cfg.Chain, cfg.Log)
	}
	t.Setenv("ETHERTEST_UNKNOWN_SETTING", "1")
	if _, err := ReadConfig(""); err == nil {
		t.Fatal("expected unknown ETHERTEST_ key rejection")
	}
}

func TestIPCConfigurationDefaultsAndPathResolution(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.IPC.Enabled || cfg.IPC.Path != "ethertest.ipc" || cfg.IPCEndpoint() != "" {
		t.Fatalf("unexpected IPC defaults: %#v endpoint=%q", cfg.IPC, cfg.IPCEndpoint())
	}

	cfg.IPC.Enabled = true
	endpoint := cfg.IPCEndpoint()
	if runtime.GOOS == "windows" {
		if endpoint != `\\.\pipe\ethertest.ipc` {
			t.Fatalf("Windows IPC endpoint = %q", endpoint)
		}
	} else if endpoint != filepath.Join(os.TempDir(), "ethertest.ipc") {
		t.Fatalf("ephemeral IPC endpoint = %q", endpoint)
	}

	cfg.Storage.Engine = "pebble"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "data")
	if runtime.GOOS != "windows" && cfg.IPCEndpoint() != filepath.Join(cfg.Storage.Path, "ethertest.ipc") {
		t.Fatalf("persistent IPC endpoint = %q", cfg.IPCEndpoint())
	}
	explicit := filepath.Join(t.TempDir(), "custom.ipc")
	cfg.IPC.Path = explicit
	if runtime.GOOS != "windows" && cfg.IPCEndpoint() != explicit {
		t.Fatalf("explicit IPC endpoint = %q, want %q", cfg.IPCEndpoint(), explicit)
	}
}

func TestIPCConfigurationEnvironmentAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipc.toml")
	if err := os.WriteFile(path, []byte("[ipc]\nenabled = true\npath = \"configured.ipc\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileConfig, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fileConfig.IPC.Enabled || fileConfig.IPC.Path != "configured.ipc" {
		t.Fatalf("IPC TOML configuration not applied: %#v", fileConfig.IPC)
	}

	t.Setenv("ETHERTEST_IPC_ENABLED", "true")
	t.Setenv("ETHERTEST_IPC_PATH", "custom.ipc")
	cfg, err := ReadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.IPC.Enabled || cfg.IPC.Path != "custom.ipc" {
		t.Fatalf("IPC environment overrides not applied: %#v", cfg.IPC)
	}
	cfg.IPC.Path = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected enabled IPC without a path to fail validation")
	}
	cfg.IPC.Enabled = false
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disabled IPC should ignore an empty path: %v", err)
	}
}

func TestLogConfigurationValidation(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Log.Level != "info" || cfg.Log.ProgressInterval != 10*time.Second {
		t.Fatalf("unexpected log defaults %#v", cfg.Log)
	}
	cfg.Log.Level = "trace"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid log level rejection")
	}
	cfg.Log.Level = "info"
	cfg.Log.ProgressInterval = time.Second - 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected short progress interval rejection")
	}
}
