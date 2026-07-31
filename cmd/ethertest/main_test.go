package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/islishude/ethertest"
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
