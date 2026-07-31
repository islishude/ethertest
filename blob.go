package ethertest

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/holiman/uint256"
)

const (
	PackedBytesV1HeaderSize = 48
	PackedBytesV1Capacity   = 4096*31 - PackedBytesV1HeaderSize
)

var packedBytesMagic = [8]byte{'E', 'T', 'H', 'T', 'B', 'L', 'B', 1}

// EncodePackedBytesV1 encodes one payload into one canonical EIP-4844 blob.
// Byte zero in every field element remains zero, so every element is canonical.
func EncodePackedBytesV1(payload []byte) (kzg4844.Blob, error) {
	if len(payload) > PackedBytesV1Capacity {
		return kzg4844.Blob{}, fmt.Errorf("payload is %d bytes; maximum is %d", len(payload), PackedBytesV1Capacity)
	}
	stream := make([]byte, PackedBytesV1HeaderSize+len(payload))
	copy(stream[:8], packedBytesMagic[:])
	binary.BigEndian.PutUint64(stream[8:16], uint64(len(payload)))
	sum := sha256.Sum256(payload)
	copy(stream[16:48], sum[:])
	copy(stream[48:], payload)
	var blob kzg4844.Blob
	for source, element := 0, 0; source < len(stream); element++ {
		n := min(31, len(stream)-source)
		copy(blob[element*32+1:element*32+1+n], stream[source:source+n])
		source += n
	}
	return blob, nil
}

func DecodePackedBytesV1(blob kzg4844.Blob) ([]byte, error) {
	stream := make([]byte, 0, 4096*31)
	for element := range 4096 {
		if blob[element*32] != 0 {
			return nil, fmt.Errorf("non-canonical field element %d", element)
		}
		stream = append(stream, blob[element*32+1:element*32+32]...)
	}
	if len(stream) < PackedBytesV1HeaderSize || string(stream[:8]) != string(packedBytesMagic[:]) {
		return nil, errors.New("invalid packed-bytes-v1 header")
	}
	length := binary.BigEndian.Uint64(stream[8:16])
	if length > PackedBytesV1Capacity {
		return nil, errors.New("packed-bytes-v1 length exceeds capacity")
	}
	payload := append([]byte(nil), stream[48:48+length]...)
	sum := sha256.Sum256(payload)
	if string(sum[:]) != string(stream[16:48]) {
		return nil, errors.New("packed-bytes-v1 checksum mismatch")
	}
	return payload, nil
}

type BlobTransactionRequest struct {
	ChainID    *big.Int
	Nonce      uint64
	To         common.Address
	Gas        uint64
	GasTipCap  *big.Int
	GasFeeCap  *big.Int
	BlobFeeCap *big.Int
	Value      *big.Int
	Data       []byte
	Blob       kzg4844.Blob
}

// SignBlobTransaction builds an Osaka sidecar with real KZG commitment and
// 128 cell proofs, then signs the EIP-4844 transaction.
func SignBlobTransaction(request BlobTransactionRequest, key *ecdsa.PrivateKey) (*types.Transaction, error) {
	if request.ChainID == nil || request.GasTipCap == nil || request.GasFeeCap == nil || request.BlobFeeCap == nil {
		return nil, errors.New("chain ID and fee caps are required")
	}
	if request.Gas == 0 {
		request.Gas = 21_000
	}
	if request.Value == nil {
		request.Value = new(big.Int)
	}
	commitment, err := kzg4844.BlobToCommitment(&request.Blob)
	if err != nil {
		return nil, err
	}
	proofs, err := kzg4844.ComputeCellProofs(&request.Blob)
	if err != nil {
		return nil, err
	}
	sidecar := types.NewBlobTxSidecar(
		types.BlobSidecarVersion1,
		[]kzg4844.Blob{request.Blob},
		[]kzg4844.Commitment{commitment},
		proofs,
	)
	tx := types.NewTx(&types.BlobTx{
		ChainID: uint256.MustFromBig(request.ChainID), Nonce: request.Nonce,
		GasTipCap: uint256.MustFromBig(request.GasTipCap), GasFeeCap: uint256.MustFromBig(request.GasFeeCap),
		Gas: request.Gas, To: request.To, Value: uint256.MustFromBig(request.Value),
		Data: request.Data, BlobFeeCap: uint256.MustFromBig(request.BlobFeeCap),
		BlobHashes: sidecar.BlobHashes(), Sidecar: sidecar,
	})
	return types.SignTx(tx, types.LatestSignerForChainID(request.ChainID), key)
}
