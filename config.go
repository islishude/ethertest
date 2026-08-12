package ethertest

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	DefaultMnemonic  = "test test test test test test test test test test test junk"
	DefaultChainID   = uint64(1337)
	miningModeManual = "manual"
)

type Config struct {
	Chain     ChainConfig    `toml:"chain"`
	Accounts  AccountsConfig `toml:"accounts"`
	Mining    MiningConfig   `toml:"mining"`
	HTTP      ListenerConfig `toml:"http"`
	IPC       IPCConfig      `toml:"ipc"`
	Beacon    BeaconConfig   `toml:"beacon"`
	Storage   StorageConfig  `toml:"storage"`
	Events    EventsConfig   `toml:"events"`
	Limits    ResourceLimits `toml:"limits"`
	Log       LogConfig      `toml:"log"`
	DumpState string         `toml:"dump_state"`
}

type ChainConfig struct {
	ChainID       uint64        `toml:"chain_id"`
	NetworkID     uint64        `toml:"network_id"`
	GenesisTime   int64         `toml:"genesis_time"`
	SlotDuration  time.Duration `toml:"slot_duration"`
	SlotsPerEpoch uint64        `toml:"slots_per_epoch"`
	Validators    uint64        `toml:"validators"`
	GasLimit      uint64        `toml:"gas_limit"`
	Forks         ForkConfig    `toml:"forks"`
}

type ForkConfig struct {
	CancunEpoch uint64 `toml:"cancun_epoch"`
	PragueEpoch uint64 `toml:"prague_epoch"`
	OsakaEpoch  uint64 `toml:"osaka_epoch"`
}

type AccountsConfig struct {
	Mnemonic string `toml:"mnemonic"`
	Count    int    `toml:"count"`
	Balance  string `toml:"balance"`
}

type MiningConfig struct {
	Mode          string        `toml:"mode"`
	Interval      time.Duration `toml:"interval"`
	Order         string        `toml:"order"`
	AutoMineEmpty bool          `toml:"auto_mine_empty"`
	FeeRecipient  string        `toml:"fee_recipient"`
}

type TLSConfig struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

type ListenerConfig struct {
	Enabled             bool      `toml:"enabled"`
	Address             string    `toml:"address"`
	CORS                []string  `toml:"cors"`
	AllowUnsafeExternal bool      `toml:"allow_unsafe_external"`
	TLS                 TLSConfig `toml:"tls"`
}

type BeaconConfig struct {
	Enabled bool `toml:"enabled"`
}

type IPCConfig struct {
	Enabled bool   `toml:"enabled"`
	Path    string `toml:"path"`
}

type StorageConfig struct {
	Engine  string `toml:"engine"`
	Path    string `toml:"path"`
	Archive bool   `toml:"archive"`
}

type EventsConfig struct {
	Capacity uint64 `toml:"capacity"`
}

type ResourceLimits struct {
	MaxRequestBytes  int64 `toml:"max_request_bytes"`
	MaxBatchItems    int   `toml:"max_batch_items"`
	MaxResponseBytes int64 `toml:"max_response_bytes"`
}

type LogConfig struct {
	Level            string        `toml:"level"`
	JSON             bool          `toml:"json"`
	ProgressInterval time.Duration `toml:"progress_interval"`
}

func DefaultConfig() Config {
	return Config{
		Chain: ChainConfig{
			ChainID: DefaultChainID, NetworkID: DefaultChainID,
			SlotDuration: 6 * time.Second, SlotsPerEpoch: 8,
			Validators: 64, GasLimit: 30_000_000,
		},
		Accounts: AccountsConfig{
			Mnemonic: DefaultMnemonic, Count: 10, Balance: "10000ether",
		},
		Mining: MiningConfig{Mode: "transaction", Order: "fees"},
		HTTP: ListenerConfig{
			Enabled: true, Address: "127.0.0.1:8545", CORS: []string{"*"},
		},
		IPC:     IPCConfig{Path: "ethertest.ipc"},
		Beacon:  BeaconConfig{Enabled: true},
		Storage: StorageConfig{Engine: "memory", Archive: true},
		Events:  EventsConfig{Capacity: 4096},
		Log:     LogConfig{Level: "info", ProgressInterval: 10 * time.Second},
		Limits: ResourceLimits{
			MaxRequestBytes: 16 << 20, MaxBatchItems: 1000, MaxResponseBytes: 64 << 20,
		},
	}
}

// ReadConfig applies defaults, a strict TOML file, then ETHERTEST_* variables.
// It intentionally leaves validation to the caller so CLI overrides can resolve
// a conflict introduced by an earlier layer.
func ReadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		meta, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, err
		}
		if undecoded := meta.Undecoded(); len(undecoded) != 0 {
			return Config{}, fmt.Errorf("unknown TOML keys: %v", undecoded)
		}
	}
	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadConfig(path string) (Config, error) {
	cfg, err := ReadConfig(path)
	if err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.Chain.ChainID == 0 || c.Chain.NetworkID == 0 {
		return errors.New("chain and network IDs must be non-zero")
	}
	if c.Chain.GenesisTime < 0 {
		return errors.New("genesis time must be non-negative")
	}
	if c.Chain.SlotDuration <= 0 || c.Chain.SlotsPerEpoch == 0 {
		return errors.New("slot duration and slots per epoch must be positive")
	}
	if c.Chain.GasLimit == 0 {
		return errors.New("gas limit must be positive")
	}
	if c.Chain.SlotDuration%time.Second != 0 {
		return errors.New("slot duration must be a whole number of seconds")
	}
	if c.Chain.Validators != 64 {
		return errors.New("v0.1 supports exactly the minimal preset's 64 validators")
	}
	if c.Chain.Forks.CancunEpoch > c.Chain.Forks.PragueEpoch ||
		c.Chain.Forks.PragueEpoch > c.Chain.Forks.OsakaEpoch {
		return errors.New("fork epochs must satisfy Cancun <= Prague <= Osaka")
	}
	if c.Chain.Forks.CancunEpoch != 0 {
		return errors.New("v0.1 chains must start at Cancun/Deneb (cancun_epoch = 0)")
	}
	if c.Accounts.Count < 1 || c.Accounts.Count > 1024 {
		return errors.New("accounts.count must be between 1 and 1024")
	}
	if c.Mining.Mode != "transaction" && c.Mining.Mode != "interval" && c.Mining.Mode != miningModeManual {
		return fmt.Errorf("invalid mining.mode %q", c.Mining.Mode)
	}
	if c.Mining.Order != "fees" && c.Mining.Order != "fifo" {
		return fmt.Errorf("invalid mining.order %q", c.Mining.Order)
	}
	if c.Mining.Mode == "interval" && c.Mining.Interval <= 0 {
		return errors.New("mining.interval must be positive in interval mode")
	}
	if c.Storage.Engine != "memory" && c.Storage.Engine != "pebble" {
		return fmt.Errorf("unsupported storage.engine %q", c.Storage.Engine)
	}
	if c.Storage.Engine == "pebble" && c.Storage.Path == "" {
		return errors.New("storage.path is required for Pebble")
	}
	if c.Limits.MaxRequestBytes <= 0 || c.Limits.MaxResponseBytes <= 0 ||
		c.Limits.MaxBatchItems <= 0 || c.Events.Capacity == 0 {
		return errors.New("resource and event limits must be positive")
	}
	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error", "off":
	default:
		return fmt.Errorf("invalid log.level %q", c.Log.Level)
	}
	if c.Log.ProgressInterval < time.Second {
		return errors.New("log.progress_interval must be at least 1s")
	}
	if c.IPC.Enabled && c.IPC.Path == "" {
		return errors.New("ipc.path is required when IPC is enabled")
	}
	if c.Beacon.Enabled && !c.HTTP.Enabled {
		return errors.New("beacon.enabled requires http.enabled")
	}
	if !c.HTTP.Enabled {
		return nil
	}
	host, _, err := net.SplitHostPort(c.HTTP.Address)
	if err != nil {
		return fmt.Errorf("http.address: %w", err)
	}
	if !isLoopbackHost(host) && !c.HTTP.AllowUnsafeExternal {
		return errors.New("http.address is non-loopback; set allow_unsafe_external explicitly")
	}
	if (c.HTTP.TLS.CertFile == "") != (c.HTTP.TLS.KeyFile == "") {
		return errors.New("http TLS requires both cert_file and key_file")
	}
	if c.HTTP.TLS.CertFile != "" {
		if _, err := tls.LoadX509KeyPair(c.HTTP.TLS.CertFile, c.HTTP.TLS.KeyFile); err != nil {
			return fmt.Errorf("http TLS certificate/key: %w", err)
		}
	}
	return nil
}

// IPCEndpoint resolves the configured socket or named-pipe endpoint. A simple
// name follows geth semantics: it is placed in persistent storage when one is
// configured, otherwise in the system temporary directory.
func (c Config) IPCEndpoint() string {
	if !c.IPC.Enabled || c.IPC.Path == "" {
		return ""
	}
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(c.IPC.Path, `\\.\pipe\`) {
			return c.IPC.Path
		}
		return `\\.\pipe\` + c.IPC.Path
	}
	if filepath.Base(c.IPC.Path) != c.IPC.Path {
		return c.IPC.Path
	}
	if c.Storage.Engine == "pebble" && c.Storage.Path != "" {
		return filepath.Join(c.Storage.Path, c.IPC.Path)
	}
	return filepath.Join(os.TempDir(), c.IPC.Path)
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && isLoopbackHost(host)
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

func applyEnv(c *Config) error {
	type setter struct {
		key string
		set func(string) error
	}
	uint := func(dst *uint64) func(string) error {
		return func(v string) error {
			n, err := strconv.ParseUint(v, 10, 64)
			if err == nil {
				*dst = n
			}
			return err
		}
	}
	boolean := func(dst *bool) func(string) error {
		return func(v string) error {
			n, err := strconv.ParseBool(v)
			if err == nil {
				*dst = n
			}
			return err
		}
	}
	integer := func(dst *int) func(string) error {
		return func(v string) error {
			n, err := strconv.Atoi(v)
			if err == nil {
				*dst = n
			}
			return err
		}
	}
	int64Value := func(dst *int64) func(string) error {
		return func(v string) error {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				*dst = n
			}
			return err
		}
	}
	duration := func(dst *time.Duration) func(string) error {
		return func(v string) error {
			parsed, err := time.ParseDuration(v)
			if err == nil {
				*dst = parsed
			}
			return err
		}
	}
	setters := []setter{
		{"CHAIN_ID", uint(&c.Chain.ChainID)},
		{"NETWORK_ID", uint(&c.Chain.NetworkID)},
		{"GENESIS_TIME", func(v string) error { n, e := strconv.ParseInt(v, 10, 64); c.Chain.GenesisTime = n; return e }},
		{"SLOT_DURATION", duration(&c.Chain.SlotDuration)},
		{"SLOTS_PER_EPOCH", uint(&c.Chain.SlotsPerEpoch)},
		{"VALIDATORS", uint(&c.Chain.Validators)},
		{"GAS_LIMIT", uint(&c.Chain.GasLimit)},
		{"CANCUN_EPOCH", uint(&c.Chain.Forks.CancunEpoch)},
		{"PRAGUE_EPOCH", uint(&c.Chain.Forks.PragueEpoch)},
		{"OSAKA_EPOCH", uint(&c.Chain.Forks.OsakaEpoch)},
		{"HTTP_ADDRESS", func(v string) error { c.HTTP.Address = v; return nil }},
		{"HTTP_ENABLED", boolean(&c.HTTP.Enabled)},
		{"HTTP_CORS", func(v string) error { c.HTTP.CORS = strings.Split(v, ","); return nil }},
		{"HTTP_TLS_CERT_FILE", func(v string) error { c.HTTP.TLS.CertFile = v; return nil }},
		{"HTTP_TLS_KEY_FILE", func(v string) error { c.HTTP.TLS.KeyFile = v; return nil }},
		{"IPC_ENABLED", boolean(&c.IPC.Enabled)},
		{"IPC_PATH", func(v string) error { c.IPC.Path = v; return nil }},
		{"BEACON_ENABLED", boolean(&c.Beacon.Enabled)},
		{"ALLOW_UNSAFE_EXTERNAL", func(v string) error {
			n, err := strconv.ParseBool(v)
			c.HTTP.AllowUnsafeExternal = n
			return err
		}},
		{"MNEMONIC", func(v string) error { c.Accounts.Mnemonic = v; return nil }},
		{"ACCOUNT_COUNT", integer(&c.Accounts.Count)},
		{"ACCOUNT_BALANCE", func(v string) error { c.Accounts.Balance = v; return nil }},
		{"MINING_MODE", func(v string) error { c.Mining.Mode = strings.ToLower(v); return nil }},
		{"MINING_INTERVAL", duration(&c.Mining.Interval)},
		{"MINING_ORDER", func(v string) error { c.Mining.Order = strings.ToLower(v); return nil }},
		{"AUTO_MINE_EMPTY", boolean(&c.Mining.AutoMineEmpty)},
		{"FEE_RECIPIENT", func(v string) error { c.Mining.FeeRecipient = v; return nil }},
		{"STORAGE_ENGINE", func(v string) error { c.Storage.Engine = strings.ToLower(v); return nil }},
		{"STORAGE_PATH", func(v string) error { c.Storage.Path = v; return nil }},
		{"STORAGE_ARCHIVE", boolean(&c.Storage.Archive)},
		{"EVENT_CAPACITY", uint(&c.Events.Capacity)},
		{"MAX_REQUEST_BYTES", int64Value(&c.Limits.MaxRequestBytes)},
		{"MAX_BATCH_ITEMS", integer(&c.Limits.MaxBatchItems)},
		{"MAX_RESPONSE_BYTES", int64Value(&c.Limits.MaxResponseBytes)},
		{"LOG_LEVEL", func(v string) error { c.Log.Level = strings.ToLower(v); return nil }},
		{"LOG_JSON", boolean(&c.Log.JSON)},
		{"LOG_PROGRESS_INTERVAL", duration(&c.Log.ProgressInterval)},
		{"DUMP_STATE", func(v string) error { c.DumpState = v; return nil }},
	}
	known := make(map[string]struct{}, len(setters))
	for _, item := range setters {
		key := "ETHERTEST_" + item.key
		known[key] = struct{}{}
		if value, ok := os.LookupEnv(key); ok {
			if err := item.set(value); err != nil {
				return fmt.Errorf("ETHERTEST_%s: %w", item.key, err)
			}
		}
	}
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(key, "ETHERTEST_") {
			if _, ok := known[key]; !ok {
				return fmt.Errorf("unknown environment key %s", key)
			}
		}
	}
	return nil
}
