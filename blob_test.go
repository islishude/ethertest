package ethertest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/ethdb"
)

type failingBlobDatabase struct{ ethdb.Database }

func (db failingBlobDatabase) Put(key []byte, value []byte) error {
	if bytes.HasPrefix(key, blobNamespace) {
		return errors.New("injected blob database failure")
	}
	return db.Database.Put(key, value)
}

func TestPackedBytesV1RoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("ethertest"), 1000)
	blob, err := EncodePackedBytesV1(payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePackedBytesV1(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatal("decoded payload differs")
	}
}

func TestFuluDataColumnSSZLayout(t *testing.T) {
	var cell kzg4844.Cell
	var commitment kzg4844.Commitment
	var proof kzg4844.Proof
	var inclusion [4][32]byte
	header := make([]byte, 208)
	encoded, err := marshalDataColumnSSZ(7, []kzg4844.Cell{cell},
		[]kzg4844.Commitment{commitment}, []kzg4844.Proof{proof}, header, inclusion)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 356+len(cell)+len(commitment)+len(proof) {
		t.Fatalf("encoded size %d", len(encoded))
	}
	if binary.LittleEndian.Uint64(encoded[:8]) != 7 ||
		binary.LittleEndian.Uint32(encoded[8:12]) != 356 ||
		binary.LittleEndian.Uint32(encoded[12:16]) != uint32(356+len(cell)) {
		t.Fatal("invalid SSZ fixed section")
	}
}

func TestOsakaBlobTransactionRejectsBadProofAndMinesValidSidecar(t *testing.T) {
	cfg := testConfig()
	node, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck
	accounts, err := DeriveAccounts(DefaultMnemonic, 1)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := EncodePackedBytesV1([]byte("blob transaction"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := SignBlobTransaction(BlobTransactionRequest{
		ChainID: new(big.Int).SetUint64(DefaultChainID), Nonce: 0,
		To: common.Address{}, Gas: 100_000,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		BlobFeeCap: big.NewInt(1_000_000_000), Blob: blob,
	}, accounts[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	bad := tx.BlobTxSidecar().Copy()
	bad.Proofs[0][0] ^= 1
	if _, err := node.SendTransaction(context.Background(), tx.WithBlobTxSidecar(bad)); err == nil {
		t.Fatal("expected invalid KZG proof rejection")
	}
	if _, err := node.SendTransaction(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if len(node.chain.blockchain.GetBlockByNumber(1).Transactions()[0].BlobHashes()) != 1 {
		t.Fatal("mined block is missing blob commitment")
	}
	if node.chain.blobs[tx.Hash()] == nil {
		t.Fatal("blob sidecar was not retained")
	}

	request := httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/blob_sidecars/head?indices=0", nil)
	response := httptest.NewRecorder()
	node.beaconHandler().ServeHTTP(response, request)
	var envelope struct {
		Data []*deneb.BlobSidecar `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 1 || len(envelope.Data[0].KZGCommitmentInclusionProof) != 17 {
		t.Fatalf("unexpected blob sidecars %#v", envelope.Data)
	}
	if err := kzg4844.VerifyBlobProof(
		(*kzg4844.Blob)(&envelope.Data[0].Blob),
		kzg4844.Commitment(envelope.Data[0].KZGCommitment),
		kzg4844.Proof(envelope.Data[0].KZGProof),
	); err != nil {
		t.Fatal(err)
	}
	if got := blobSidecarBodyRoot(envelope.Data[0]); got != envelope.Data[0].SignedBlockHeader.Message.BodyRoot {
		t.Fatalf("blob inclusion proof produced %x, want %x", got, envelope.Data[0].SignedBlockHeader.Message.BodyRoot)
	}

	request = httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/blob_sidecars/head", nil)
	request.Header.Set("Accept", "application/octet-stream")
	response = httptest.NewRecorder()
	node.beaconHandler().ServeHTTP(response, request)
	encoded, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header().Get("Eth-Consensus-Version") == "" {
		t.Fatal("SSZ sidecar response is missing Eth-Consensus-Version")
	}
	var decoded deneb.BlobSidecar
	if err := decoded.UnmarshalSSZ(encoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Index != 0 || decoded.KZGCommitment != envelope.Data[0].KZGCommitment {
		t.Fatal("SSZ sidecar differs from JSON sidecar")
	}
}

func TestBlobDatabaseFailureLeavesNoPoolResidue(t *testing.T) {
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
	blob, err := EncodePackedBytesV1([]byte("database failure"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := SignBlobTransaction(BlobTransactionRequest{
		ChainID: new(big.Int).SetUint64(cfg.Chain.ChainID), Nonce: 0,
		To: node.Accounts()[1], Gas: 100_000,
		GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
		BlobFeeCap: big.NewInt(1_000_000_000), Blob: blob,
	}, testWalletAccount(t, node, 0).PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	original := node.chain.db
	node.chain.db = failingBlobDatabase{Database: original}
	if _, err := node.SendTransaction(context.Background(), tx); err == nil || !strings.Contains(err.Error(), "injected blob database failure") {
		t.Fatalf("blob database error = %v", err)
	}
	node.chain.mu.RLock()
	poolCount, arrivalCount, blobCount := 0, len(node.chain.arrival), len(node.chain.blobs)
	for _, byNonce := range node.chain.pending {
		poolCount += len(byNonce)
	}
	node.chain.mu.RUnlock()
	if poolCount != 0 || arrivalCount != 0 || blobCount != 0 {
		t.Fatalf("pool residue pending/arrival/blob = %d/%d/%d", poolCount, arrivalCount, blobCount)
	}
	key := append(append([]byte(nil), blobNamespace...), tx.Hash().Bytes()...)
	if exists, err := original.Has(key); err != nil || exists {
		t.Fatalf("blob database residue exists=%v err=%v", exists, err)
	}
}

func TestBeaconBlobsFiltersByVersionedHashAndPreservesBlockOrder(t *testing.T) {
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

	accounts, err := DeriveAccounts(DefaultMnemonic, 1)
	if err != nil {
		t.Fatal(err)
	}
	payloads := [][]byte{[]byte("first blob"), []byte("second blob")}
	blobs := make([]kzg4844.Blob, len(payloads))
	hashes := make([]common.Hash, len(payloads))
	txHashes := make([]common.Hash, len(payloads))
	for index, payload := range payloads {
		blobs[index], err = EncodePackedBytesV1(payload)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := SignBlobTransaction(BlobTransactionRequest{
			ChainID: new(big.Int).SetUint64(DefaultChainID), Nonce: uint64(index),
			To: common.Address{}, Gas: 100_000,
			GasTipCap: big.NewInt(1_000_000_000), GasFeeCap: big.NewInt(3_000_000_000),
			BlobFeeCap: big.NewInt(1_000_000_000), Blob: blobs[index],
		}, accounts[0].PrivateKey)
		if err != nil {
			t.Fatal(err)
		}
		txHashes[index] = tx.Hash()
		hashes[index] = tx.BlobTxSidecar().BlobHashes()[0]
		if _, err := node.SendTransaction(context.Background(), tx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := node.Mine(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}

	handler := node.beaconHandler()
	getJSON := func(target string) beaconBlobsEnvelope {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s returned %d: %s", target, response.Code, response.Body.String())
		}
		var envelope beaconBlobsEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope
	}
	assertBlobs := func(got []deneb.Blob, want ...kzg4844.Blob) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %d blobs, want %d", len(got), len(want))
		}
		for index := range want {
			if !bytes.Equal(got[index][:], want[index][:]) {
				t.Fatalf("blob %d differs", index)
			}
		}
	}

	envelope := getJSON("/eth/v1/beacon/blobs/head")
	if envelope.ExecutionOptimistic || envelope.Finalized {
		t.Fatalf("unexpected blob metadata: %#v", envelope)
	}
	assertBlobs(envelope.Data, blobs[0], blobs[1])
	genesisEnvelope := getJSON("/eth/v1/beacon/blobs/genesis")
	if genesisEnvelope.ExecutionOptimistic || !genesisEnvelope.Finalized {
		t.Fatalf("unexpected genesis blob metadata: %#v", genesisEnvelope)
	}
	assertBlobs(genesisEnvelope.Data)

	envelope = getJSON("/eth/v1/beacon/blobs/head?versioned_hashes=" +
		hashes[1].Hex() + "&versioned_hashes=" + hashes[0].Hex())
	assertBlobs(envelope.Data, blobs[0], blobs[1])

	var unknown common.Hash
	unknown[0] = 1
	unknown[len(unknown)-1] = 0xff
	envelope = getJSON("/eth/v1/beacon/blobs/head?versioned_hashes=" +
		unknown.Hex() + "&versioned_hashes=" + hashes[1].Hex())
	assertBlobs(envelope.Data, blobs[1])
	envelope = getJSON("/eth/v1/beacon/blobs/head?versioned_hashes=" + unknown.Hex())
	assertBlobs(envelope.Data)

	request := httptest.NewRequest(
		http.MethodGet,
		"/eth/v1/beacon/blobs/head?versioned_hashes="+hashes[1].Hex(),
		nil,
	)
	request.Header.Set("Accept", "application/octet-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("SSZ response status=%d content-type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	if !bytes.Equal(response.Body.Bytes(), blobs[1][:]) {
		t.Fatal("filtered SSZ blob differs")
	}

	request = httptest.NewRequest(
		http.MethodGet,
		"/eth/v1/beacon/blobs/head?versioned_hashes="+unknown.Hex(),
		nil,
	)
	request.Header.Set("Accept", "application/octet-stream")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("empty SSZ response status=%d size=%d", response.Code, response.Body.Len())
	}

	node.chain.mu.Lock()
	corrupt := node.chain.blobs[txHashes[0]].Copy()
	corrupt.Commitments = nil
	node.chain.blobs[txHashes[0]] = corrupt
	node.chain.mu.Unlock()
	request = httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/blobs/head", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt sidecar returned %d, want 500", response.Code)
	}
}

func TestBeaconBlobsRejectsInvalidVersionedHashes(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	valid := common.Hash{1}.Hex()
	tests := []string{
		"versioned_hashes=",
		"versioned_hashes=" + strings.TrimPrefix(valid, "0x"),
		"versioned_hashes=0x01",
		"versioned_hashes=0x" + strings.Repeat("g", 64),
		"versioned_hashes=" + valid + "&versioned_hashes=" + valid,
		"versioned_hashes=" + valid + "," + valid,
	}
	handler := node.beaconHandler()
	for _, query := range tests {
		request := httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/blobs/genesis?"+query, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("%q returned %d, want 400", query, response.Code)
		}
	}
}

func TestBeaconServeMuxPatternsAreExactAndPopulatePathValues(t *testing.T) {
	node, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := node.Start(); err != nil {
		t.Fatal(err)
	}
	defer node.Close() //nolint:errcheck

	handler := node.beaconHandler()
	genesis := node.chain.blockchain.Genesis()
	root, err := node.beaconRoot(genesis)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"genesis", "0", common.Hash(root).Hex()} {
		request := httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/headers/"+id, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("header ID %q returned %d: %s", id, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{
		"/eth/v2/beacon/blocks/genesis",
		"/eth/v1/beacon/states/genesis/validators",
		"/eth/v1/beacon/states/genesis/validator_balances",
		"/eth/v1/beacon/states/genesis/finality_checkpoints",
		"/eth/v1/beacon/blobs/genesis",
		"/eth/v1/beacon/blob_sidecars/genesis",
		"/eth/v1/debug/beacon/data_column_sidecars/genesis",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Errorf("%s returned %d: %s", path, response.Code, response.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/eth/v1/beacon/headers/genesis/extra", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("extra path returned %d, want 404", response.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/eth/v1/beacon/headers/genesis", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST returned %d, want 405", response.Code)
	}
}

func blobSidecarBodyRoot(sidecar *deneb.BlobSidecar) [32]byte {
	var first, second [32]byte
	copy(first[:], sidecar.KZGCommitment[:32])
	copy(second[:16], sidecar.KZGCommitment[32:])
	root := hashPair(first, second)
	index := uint64(sidecar.Index)
	for level := range 12 {
		sibling := [32]byte(sidecar.KZGCommitmentInclusionProof[level])
		if index&1 == 0 {
			root = hashPair(root, sibling)
		} else {
			root = hashPair(sibling, root)
		}
		index >>= 1
	}
	root = hashPair(root, [32]byte(sidecar.KZGCommitmentInclusionProof[12]))
	index = 11
	for level := 13; level < 17; level++ {
		sibling := [32]byte(sidecar.KZGCommitmentInclusionProof[level])
		if index&1 == 0 {
			root = hashPair(root, sibling)
		} else {
			root = hashPair(sibling, root)
		}
		index >>= 1
	}
	return root
}

func TestPackedBytesV1RejectsCorruption(t *testing.T) {
	blob, err := EncodePackedBytesV1([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	blob[52] ^= 1
	if _, err := DecodePackedBytesV1(blob); err == nil {
		t.Fatal("expected checksum error")
	}
}
