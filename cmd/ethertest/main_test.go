package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"path/filepath"
	"strings"
	"testing"

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
