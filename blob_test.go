package ethertest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
)

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
	node.beaconBlobs(response, request)
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
	node.beaconBlobs(response, request)
	encoded, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded deneb.BlobSidecar
	if err := decoded.UnmarshalSSZ(encoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Index != 0 || decoded.KZGCommitment != envelope.Data[0].KZGCommitment {
		t.Fatal("SSZ sidecar differs from JSON sidecar")
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
