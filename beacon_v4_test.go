package ethertest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/ethereum/go-ethereum/common"
)

func TestBeaconV4BlockContractAndRemovedAliases(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler := node.beaconHandler()
	defer node.Close() //nolint:errcheck

	for _, path := range []string{
		"/eth/v1/beacon/blocks/genesis",
		"/eth/v1/beacon/data_column_sidecars/genesis",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("removed alias %s returned %d", path, response.Code)
		}
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/genesis", nil))
	if response.Code != http.StatusOK || response.Header().Get("Eth-Consensus-Version") != "fulu" || response.Header().Get("Ethertest-Consensus-Mode") != "synthetic" {
		t.Fatalf("block response status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var envelope struct {
		Version             string                    `json:"version"`
		ExecutionOptimistic *bool                     `json:"execution_optimistic"`
		Finalized           *bool                     `json:"finalized"`
		Data                electra.SignedBeaconBlock `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Version != "fulu" || envelope.ExecutionOptimistic == nil || *envelope.ExecutionOptimistic || envelope.Finalized == nil || !*envelope.Finalized || envelope.Data.Message == nil {
		t.Fatalf("invalid v4 envelope %#v", envelope)
	}

	sszRequest := httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/genesis", nil)
	sszRequest.Header.Set("Accept", "application/octet-stream;q=1, application/json;q=0.5")
	sszResponse := httptest.NewRecorder()
	handler.ServeHTTP(sszResponse, sszRequest)
	if sszResponse.Code != http.StatusOK || sszResponse.Header().Get("Content-Type") != "application/octet-stream" || sszResponse.Header().Get("Ethertest-Consensus-Mode") != "synthetic" {
		t.Fatalf("SSZ response status=%d headers=%v", sszResponse.Code, sszResponse.Header())
	}
	var decoded electra.SignedBeaconBlock
	if err := decoded.UnmarshalSSZ(sszResponse.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	jsonRoot, err := envelope.Data.Message.HashTreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	sszRoot, err := decoded.Message.HashTreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if jsonRoot != sszRoot {
		t.Fatalf("JSON root %s differs from SSZ root %s", common.Hash(jsonRoot), common.Hash(sszRoot))
	}

	notAcceptable := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/genesis", nil)
	request.Header.Set("Accept", "text/plain")
	handler.ServeHTTP(notAcceptable, request)
	assertBeaconError(t, notAcceptable, http.StatusNotAcceptable)
	badID := httptest.NewRecorder()
	handler.ServeHTTP(badID, httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/current", nil))
	assertBeaconError(t, badID, http.StatusBadRequest)
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/0x"+strings.Repeat("11", 32), nil))
	assertBeaconError(t, missing, http.StatusNotFound)
}

func TestBeaconTaintedEnvelopeAndValidatorFilters(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	zero := new(big.Int)
	if _, err := node.ApplyControl(context.Background(), ControlChanges{node.chain.accounts[0].Address: {Balance: zero}}); err != nil {
		t.Fatal(err)
	}
	handler := node.beaconHandler()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/eth/v2/beacon/blocks/head", nil))
	var envelope map[string]any
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["execution_optimistic"] != true || envelope["ethertest_tainted"] != true {
		t.Fatalf("tainted envelope = %#v", envelope)
	}

	validators := httptest.NewRecorder()
	handler.ServeHTTP(validators, httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/states/head/validators?id=1&status=active", nil))
	if validators.Code != http.StatusOK {
		t.Fatalf("filtered validators status=%d body=%s", validators.Code, validators.Body.String())
	}
	var filtered struct {
		ExecutionOptimistic bool `json:"execution_optimistic"`
		Data                []struct {
			Index string `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(validators.Body).Decode(&filtered); err != nil {
		t.Fatal(err)
	}
	if !filtered.ExecutionOptimistic || len(filtered.Data) != 1 || filtered.Data[0].Index != "1" {
		t.Fatalf("filtered validators = %#v", filtered)
	}

	exited := httptest.NewRecorder()
	handler.ServeHTTP(exited, httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/states/head/validators?status=exited", nil))
	var noMatches struct {
		Data []any `json:"data"`
	}
	if err := json.NewDecoder(exited.Body).Decode(&noMatches); err != nil || len(noMatches.Data) != 0 {
		t.Fatalf("nonmatching status response=%s err=%v", exited.Body.String(), err)
	}
	for _, path := range []string{
		"/eth/v1/beacon/states/head/validators?status=weather",
		"/eth/v1/beacon/states/head/validators?id=1&id=1",
		"/eth/v1/beacon/states/head/validator_balances?status=active",
	} {
		invalid := httptest.NewRecorder()
		handler.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, path, nil))
		assertBeaconError(t, invalid, http.StatusBadRequest)
	}
}

func TestBeaconSSETopicsReplayAndGap(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	handler := node.beaconHandler()
	for _, path := range []string{"/eth/v1/events", "/eth/v1/events?topics=attestation"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		assertBeaconError(t, response, http.StatusBadRequest)
	}
	if _, err := node.Mine(context.Background(), 1, true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	first := readSSERecord(t, server.URL+"/eth/v1/events?topics=block&topics=head", "0")
	if !strings.Contains(first, "id: 8\n") || !strings.Contains(first, "event: block\n") || strings.Contains(first, "block_number") {
		t.Fatalf("first standard SSE record = %q", first)
	}
	second := readSSERecord(t, server.URL+"/eth/v1/events?topics=block&topics=head", "8")
	if !strings.Contains(second, "id: 9\n") || !strings.Contains(second, "event: head\n") || !strings.Contains(second, `"state":"0x`) {
		t.Fatalf("resumed standard SSE record = %q", second)
	}

	gapCfg := testConfig()
	gapCfg.Mining.Mode = "manual"
	gapCfg.Events.Capacity = 1
	gapNode, err := New(gapCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := gapNode.Start(); err != nil {
		t.Fatal(err)
	}
	defer gapNode.Close() //nolint:errcheck
	if _, err := gapNode.Mine(context.Background(), 2, true); err != nil {
		t.Fatal(err)
	}
	gapRequest := httptest.NewRequest(http.MethodGet, "/eth/v1/events?topics=block", nil)
	gapRequest.Header.Set("Last-Event-ID", "8")
	gapResponse := httptest.NewRecorder()
	gapNode.beaconHandler().ServeHTTP(gapResponse, gapRequest)
	assertBeaconError(t, gapResponse, http.StatusGone)
}

func readSSERecord(t *testing.T, endpoint, lastID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", lastID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("SSE status=%d body=%s", response.StatusCode, data)
	}
	reader := bufio.NewReader(response.Body)
	var record strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		record.WriteString(line)
		if line == "\n" {
			return record.String()
		}
	}
}

func assertBeaconError(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Beacon error status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var body BeaconErrorMessage
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code != status || body.Message == "" {
		t.Fatalf("Beacon error body = %#v", body)
	}
}

func TestRequestedBeaconTopicsUsesStandardQueryShape(t *testing.T) {
	request := &http.Request{URL: &url.URL{RawQuery: "topics=head&topics=block"}}
	topics, err := requestedBeaconTopics(request)
	if err != nil || !topics["head"] || !topics["block"] || len(topics) != 2 {
		t.Fatalf("topics = %#v, %v", topics, err)
	}
}
