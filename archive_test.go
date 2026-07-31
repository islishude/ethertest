package ethertest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateArchiveRoundTripAndCorruption(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Close()
	})
	path := filepath.Join(t.TempDir(), "state.tar.zst")
	if err := node.DumpState(path); err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectState(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != StateArchiveFormat || manifest.Secrets || manifest.Tainted {
		t.Fatalf("unexpected manifest %#v", manifest)
	}
	destination := filepath.Join(t.TempDir(), "pebble")
	if err := LoadState(path, destination); err != nil {
		t.Fatal(err)
	}
	loadedConfig := testConfig()
	loadedConfig.Storage.Engine = "pebble"
	loadedConfig.Storage.Path = destination
	loaded, err := New(loadedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.chain.blockchain.CurrentBlock().Hash(); got.Hex() != manifest.HeadHash {
		t.Fatalf("loaded head %s, want %s", got, manifest.HeadHash)
	}
	if err := loaded.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff
	bad := filepath.Join(t.TempDir(), "bad.tar.zst")
	if err := os.WriteFile(bad, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectState(bad); err == nil {
		t.Fatal("expected corrupt archive rejection")
	}
}

func TestCloseDumpsStateAfterStoppingWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shutdown.tar.zst")
	cfg := testConfig()
	cfg.DumpState = path
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := InspectState(path)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.HeadNumber != 0 || manifest.GenesisHash != manifest.HeadHash {
		t.Fatalf("unexpected shutdown manifest %#v", manifest)
	}
}
