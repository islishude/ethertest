package ethertest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/golang/snappy"
)

func TestLockedCKZGProofVector(t *testing.T) {
	data, err := os.ReadFile("testdata/vectors/c-kzg-4844-v2.1.5/verify_kzg_proof/correct_proof_0_0.yaml")
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "'")
		if key == "commitment" || key == "z" || key == "y" || key == "proof" || key == "output" {
			values[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if values["output"] != "true" {
		t.Fatalf("unexpected KZG vector output %q", values["output"])
	}
	commitmentBytes := mustVectorHex(t, values["commitment"], len(kzg4844.Commitment{}))
	pointBytes := mustVectorHex(t, values["z"], len(kzg4844.Point{}))
	claimBytes := mustVectorHex(t, values["y"], len(kzg4844.Claim{}))
	proofBytes := mustVectorHex(t, values["proof"], len(kzg4844.Proof{}))
	var commitment kzg4844.Commitment
	var point kzg4844.Point
	var claim kzg4844.Claim
	var proof kzg4844.Proof
	copy(commitment[:], commitmentBytes)
	copy(point[:], pointBytes)
	copy(claim[:], claimBytes)
	copy(proof[:], proofBytes)
	if err := kzg4844.VerifyProof(commitment, point, claim, proof); err != nil {
		t.Fatalf("c-kzg-4844 v2.1.5 vector rejected: %v", err)
	}
}

func mustVectorHex(t *testing.T, value string, size int) []byte {
	t.Helper()
	decoded, err := hexutil.Decode(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != size {
		t.Fatalf("vector field has %d bytes, want %d", len(decoded), size)
	}
	return decoded
}

func TestLockedConsensusSSZContainerVector(t *testing.T) {
	var vector struct {
		Serialized string `json:"serialized_ssz_snappy"`
		Root       string `json:"root"`
		Value      struct {
			Slot          string `json:"slot"`
			ProposerIndex string `json:"proposer_index"`
			ParentRoot    string `json:"parent_root"`
			StateRoot     string `json:"state_root"`
			BodyRoot      string `json:"body_root"`
		} `json:"value"`
	}
	data, err := os.ReadFile("testdata/vectors/consensus-spec-tests-v1.6.0-beta.0/beacon_block_header/case_0.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	compressed := mustVectorHex(t, vector.Serialized, 115)
	serialized, err := snappy.Decode(nil, compressed)
	if err != nil {
		t.Fatal(err)
	}
	var header phase0.BeaconBlockHeader
	if err := header.UnmarshalSSZ(serialized); err != nil {
		t.Fatal(err)
	}
	wantSlot, err := strconv.ParseUint(vector.Value.Slot, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	wantProposer, err := strconv.ParseUint(vector.Value.ProposerIndex, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(header.Slot) != wantSlot || uint64(header.ProposerIndex) != wantProposer ||
		common.Hash(header.ParentRoot) != common.HexToHash(vector.Value.ParentRoot) ||
		common.Hash(header.StateRoot) != common.HexToHash(vector.Value.StateRoot) ||
		common.Hash(header.BodyRoot) != common.HexToHash(vector.Value.BodyRoot) {
		t.Fatalf("decoded SSZ header does not match locked value: %#v", header)
	}
	root, err := header.HashTreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if common.Hash(root) != common.HexToHash(vector.Root) {
		t.Fatalf("SSZ vector root = %s, want %s", common.Hash(root), vector.Root)
	}
	encoded, err := header.MarshalSSZ()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, serialized) {
		t.Fatal("SSZ vector did not round-trip byte-for-byte")
	}
}

func TestLockedEIP4788TimestampSequences(t *testing.T) {
	var vector struct {
		HistoryBufferLength uint64 `json:"history_buffer_length"`
		Cases               []struct {
			ID    string `json:"id"`
			Start uint64 `json:"start"`
			Step  uint64 `json:"step"`
			Count uint64 `json:"count"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("testdata/vectors/execution-spec-tests-eip4788/timestamp_sequences.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &vector); err != nil {
		t.Fatal(err)
	}
	if vector.HistoryBufferLength != 8191 {
		t.Fatalf("EIP-4788 history buffer length = %d", vector.HistoryBufferLength)
	}
	for _, test := range vector.Cases {
		t.Run(test.ID, func(t *testing.T) {
			if test.Start <= test.Step {
				t.Fatal("vector requires a positive genesis timestamp")
			}
			cfg := testConfig()
			cfg.Mining.Mode = "manual"
			cfg.Chain.GenesisTime = int64(test.Start - test.Step)
			cfg.Chain.SlotDuration = time.Duration(test.Step) * time.Second
			node, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if root := node.chain.blockchain.Genesis().BeaconRoot(); root == nil || *root != (common.Hash{}) {
				t.Fatalf("genesis parent beacon root = %v, want zero", root)
			}
			if err := node.Start(); err != nil {
				t.Fatal(err)
			}
			defer node.Close() //nolint:errcheck
			type storedPair struct {
				timestamp uint64
				root      common.Hash
			}
			expected := make(map[uint64]storedPair)
			for index := uint64(0); index < test.Count; index++ {
				hashes, err := node.Mine(context.Background(), 1, true)
				if err != nil {
					t.Fatal(err)
				}
				block := node.chain.blockchain.GetBlockByHash(hashes[0])
				wantTimestamp := test.Start + index*test.Step
				if block.Time() != wantTimestamp || block.BeaconRoot() == nil {
					t.Fatalf("block %d timestamp/root = %d/%v, want %d/non-nil", index, block.Time(), block.BeaconRoot(), wantTimestamp)
				}
				expected[wantTimestamp%vector.HistoryBufferLength] = storedPair{timestamp: wantTimestamp, root: *block.BeaconRoot()}
			}
			head := node.chain.blockchain.CurrentBlock()
			state, err := node.chain.blockchain.StateAt(head)
			if err != nil {
				t.Fatal(err)
			}
			for index, pair := range expected {
				timestamp := state.GetState(params.BeaconRootsAddress, common.BigToHash(new(big.Int).SetUint64(index)))
				root := state.GetState(params.BeaconRootsAddress, common.BigToHash(new(big.Int).SetUint64(index+vector.HistoryBufferLength)))
				if timestamp != common.BigToHash(new(big.Int).SetUint64(pair.timestamp)) || root != pair.root {
					t.Fatalf("ring index %d = %s/%s, want %d/%s", index, timestamp, root, pair.timestamp, pair.root)
				}
			}
		})
	}
}
