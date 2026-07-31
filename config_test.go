package ethertest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
}

func TestTLSMaterialFailsClosedDuringValidation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HTTP.TLS.CertFile = filepath.Join(t.TempDir(), "missing.crt")
	cfg.HTTP.TLS.KeyFile = filepath.Join(t.TempDir(), "missing.key")
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing TLS material to fail validation")
	}
}

func TestEnvironmentOverridesAndRejectsUnknownKeys(t *testing.T) {
	t.Setenv("ETHERTEST_CHAIN_ID", "4242")
	t.Setenv("ETHERTEST_SLOT_DURATION", "3s")
	cfg, err := ReadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Chain.ChainID != 4242 || cfg.Chain.SlotDuration != 3*time.Second {
		t.Fatalf("environment overrides not applied: %#v", cfg.Chain)
	}
	t.Setenv("ETHERTEST_UNKNOWN_SETTING", "1")
	if _, err := ReadConfig(""); err == nil {
		t.Fatal("expected unknown ETHERTEST_ key rejection")
	}
}
