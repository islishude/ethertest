package ethertest

import (
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
)

const blobBundleFormat = 1

type blobBundle struct {
	Format      uint8
	Blobs       []kzg4844.Blob
	Commitments []kzg4844.Commitment
	BlobProofs  []kzg4844.Proof
	CellProofs  []kzg4844.Proof
}

func newBlobBundle(sidecar *types.BlobTxSidecar) (*blobBundle, error) {
	if sidecar == nil || len(sidecar.Blobs) == 0 || len(sidecar.Blobs) != len(sidecar.Commitments) {
		return nil, errors.New("malformed blob sidecar")
	}
	bundle := &blobBundle{
		Format: blobBundleFormat, Blobs: append([]kzg4844.Blob(nil), sidecar.Blobs...),
		Commitments: append([]kzg4844.Commitment(nil), sidecar.Commitments...),
	}
	switch sidecar.Version {
	case types.BlobSidecarVersion0:
		if len(sidecar.Proofs) != len(sidecar.Blobs) {
			return nil, errors.New("malformed version 0 sidecar")
		}
		bundle.BlobProofs = append([]kzg4844.Proof(nil), sidecar.Proofs...)
		for index := range bundle.Blobs {
			proofs, err := kzg4844.ComputeCellProofs(&bundle.Blobs[index])
			if err != nil {
				return nil, err
			}
			bundle.CellProofs = append(bundle.CellProofs, proofs...)
		}
	case types.BlobSidecarVersion1:
		if len(sidecar.Proofs) != len(sidecar.Blobs)*kzg4844.CellProofsPerBlob {
			return nil, errors.New("malformed version 1 sidecar")
		}
		bundle.CellProofs = append([]kzg4844.Proof(nil), sidecar.Proofs...)
		for index := range bundle.Blobs {
			proof, err := kzg4844.ComputeBlobProof(&bundle.Blobs[index], bundle.Commitments[index])
			if err != nil {
				return nil, err
			}
			bundle.BlobProofs = append(bundle.BlobProofs, proof)
		}
	default:
		return nil, fmt.Errorf("unsupported sidecar version %d", sidecar.Version)
	}
	if err := bundle.validate(); err != nil {
		return nil, err
	}
	return bundle, nil
}

func (bundle *blobBundle) validate() error {
	if bundle == nil || bundle.Format != blobBundleFormat || len(bundle.Blobs) == 0 ||
		len(bundle.Blobs) != len(bundle.Commitments) || len(bundle.BlobProofs) != len(bundle.Blobs) ||
		len(bundle.CellProofs) != len(bundle.Blobs)*kzg4844.CellProofsPerBlob {
		return errors.New("invalid blob bundle")
	}
	for index := range bundle.Blobs {
		if err := kzg4844.VerifyBlobProof(&bundle.Blobs[index], bundle.Commitments[index], bundle.BlobProofs[index]); err != nil {
			return err
		}
	}
	return kzg4844.VerifyCellProofs(bundle.Blobs, bundle.Commitments, bundle.CellProofs)
}

func (bundle *blobBundle) copy() *blobBundle {
	if bundle == nil {
		return nil
	}
	return &blobBundle{
		Format: bundle.Format, Blobs: append([]kzg4844.Blob(nil), bundle.Blobs...),
		Commitments: append([]kzg4844.Commitment(nil), bundle.Commitments...),
		BlobProofs:  append([]kzg4844.Proof(nil), bundle.BlobProofs...),
		CellProofs:  append([]kzg4844.Proof(nil), bundle.CellProofs...),
	}
}

func (bundle *blobBundle) Copy() *blobBundle { return bundle.copy() }

func (bundle *blobBundle) sidecar(version byte) *types.BlobTxSidecar {
	if bundle == nil || bundle.validate() != nil {
		return nil
	}
	proofs := bundle.BlobProofs
	if version == types.BlobSidecarVersion1 {
		proofs = bundle.CellProofs
	} else if version != types.BlobSidecarVersion0 {
		return nil
	}
	return types.NewBlobTxSidecar(
		version, append([]kzg4844.Blob(nil), bundle.Blobs...),
		append([]kzg4844.Commitment(nil), bundle.Commitments...), append([]kzg4844.Proof(nil), proofs...),
	)
}
