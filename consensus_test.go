package ethertest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	bls "github.com/protolambda/bls12-381-util"
)

func TestDeterministicProposerSignatureVerifies(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	block := node.chain.blockchain.GetBlockByNumber(0)
	header, err := node.consensus.signedHeader(node.chain, block)
	if err != nil {
		t.Fatal(err)
	}
	objectRoot, err := header.Message.HashTreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	domain, err := node.consensus.proposerDomain()
	if err != nil {
		t.Fatal(err)
	}
	signingRoot, err := (&phase0.SigningData{ObjectRoot: objectRoot, Domain: domain}).HashTreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	pubkey := new(bls.Pubkey)
	if err := pubkey.Deserialize(&node.consensus.pubkeys[0]); err != nil {
		t.Fatal(err)
	}
	signatureBytes := [96]byte(header.Signature)
	signature := new(bls.Signature)
	if err := signature.Deserialize(&signatureBytes); err != nil {
		t.Fatal(err)
	}
	if !bls.Verify(pubkey, signingRoot[:], signature) {
		t.Fatal("proposer signature did not verify")
	}
	if _, err := node.consensus.kzgCommitmentsInclusionProof(node.chain, block); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBeaconJSONAndSSZContentNegotiation(t *testing.T) {
	cfg := testConfig()
	cfg.HTTP.Enabled = true
	cfg.HTTP.Address = "127.0.0.1:0"
	cfg.Beacon.Enabled = true
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	endpoints := node.Endpoints()
	if endpoints.Execution == "" || endpoints.Execution != endpoints.Beacon {
		t.Fatalf("shared endpoints = %#v", endpoints)
	}
	endpoint := endpoints.Beacon

	httpRPC, err := rpc.DialHTTP(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer httpRPC.Close()
	var chainID hexutil.Uint64
	if err := httpRPC.Call(&chainID, "eth_chainId"); err != nil {
		t.Fatal(err)
	}
	if uint64(chainID) != cfg.Chain.ChainID {
		t.Fatalf("HTTP RPC chain ID = %d, want %d", chainID, cfg.Chain.ChainID)
	}
	var networkConfig map[string]any
	if err := httpRPC.Call(&networkConfig, "ethertest_networkConfig"); err != nil {
		t.Fatal(err)
	}
	if networkConfig["el"] != cfg.HTTP.Address || networkConfig["beacon"] != cfg.HTTP.Address {
		t.Fatalf("network config endpoints = %#v", networkConfig)
	}

	websocketEndpoint := "ws" + strings.TrimPrefix(endpoint, "http")
	websocketRPC, err := rpc.DialWebsocket(t.Context(), websocketEndpoint, "http://localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer websocketRPC.Close()
	if err := websocketRPC.Call(&chainID, "eth_chainId"); err != nil {
		t.Fatal(err)
	}

	response, err := http.Get(endpoint + "/eth/v1/beacon/headers/head")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Header phase0.SignedBeaconBlockHeader `json:"header"`
			Root   string                         `json:"root"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		_ = response.Body.Close()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	nonzero := false
	for _, value := range envelope.Data.Header.Signature {
		nonzero = nonzero || value != 0
	}
	if !nonzero {
		t.Fatal("proposer signature is zero")
	}
	if len(envelope.Data.Root) != 66 || envelope.Data.Root[:2] != "0x" {
		t.Fatalf("Beacon root is not hex: %q", envelope.Data.Root)
	}

	request, _ := http.NewRequest(http.MethodGet, endpoint+"/eth/v2/beacon/blocks/head", nil)
	request.Header.Set("Accept", "application/octet-stream")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || response.Header.Get("Eth-Consensus-Version") != "fulu" {
		t.Fatalf("unexpected SSZ response status=%d version=%q", response.StatusCode, response.Header.Get("Eth-Consensus-Version"))
	}
	var signed electra.SignedBeaconBlock
	if err := signed.UnmarshalSSZ(data); err != nil {
		t.Fatal(err)
	}
	if signed.Message == nil || signed.Message.Body == nil {
		t.Fatal("decoded SSZ block is incomplete")
	}

	request, _ = http.NewRequest(
		http.MethodPost,
		endpoint+"/eth/v1/not-a-beacon-route",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown Beacon route status = %d, want 404", response.StatusCode)
	}
}

func TestDenebElectraFuluForkBoundaries(t *testing.T) {
	cfg := testConfig()
	cfg.Mining.Mode = "manual"
	cfg.Chain.Forks.PragueEpoch = 1
	cfg.Chain.Forks.OsakaEpoch = 2
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	genesis := node.chain.blockchain.GetBlockByNumber(0)
	signed, err := node.consensus.signedBlock(node.chain, genesis)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := signed.marshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	var denebBlock deneb.SignedBeaconBlock
	if err := denebBlock.UnmarshalSSZ(encoded); err != nil {
		t.Fatal(err)
	}
	if signed.deneb == nil || node.consensus.forkName(0) != "deneb" {
		t.Fatal("genesis did not use Deneb")
	}

	hashes, err := node.Mine(context.Background(), 8, true)
	if err != nil {
		t.Fatal(err)
	}
	prague := node.chain.blockchain.GetBlockByHash(hashes[7])
	signed, err = node.consensus.signedBlock(node.chain, prague)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = signed.marshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	var electraBlock electra.SignedBeaconBlock
	if err := electraBlock.UnmarshalSSZ(encoded); err != nil {
		t.Fatal(err)
	}
	if signed.electra == nil || node.consensus.forkName(8) != "electra" {
		t.Fatal("Prague boundary did not use Electra")
	}

	if _, err := node.Mine(context.Background(), 8, true); err != nil {
		t.Fatal(err)
	}
	if node.consensus.forkName(16) != "fulu" {
		t.Fatal("Osaka boundary did not report Fulu")
	}
}
