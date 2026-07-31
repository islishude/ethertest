package ethertest

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/rpc"
)

type beaconEnvelope struct {
	Data any `json:"data"`
}

type beaconBlobsEnvelope struct {
	ExecutionOptimistic bool         `json:"execution_optimistic"`
	Finalized           bool         `json:"finalized"`
	Data                []deneb.Blob `json:"data"`
}

func (n *Node) beaconHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /eth/v1/node/health", n.beaconHealth)
	mux.HandleFunc("GET /eth/v1/node/syncing", n.beaconSyncing)
	mux.HandleFunc("GET /eth/v1/beacon/genesis", n.beaconGenesis)
	mux.HandleFunc("GET /eth/v1/config/spec", n.beaconSpec)
	mux.HandleFunc("GET /eth/v1/config/fork_schedule", n.beaconForkSchedule)
	mux.HandleFunc("GET /eth/v1/beacon/headers/{block_id}", n.beaconHeader)
	mux.HandleFunc("GET /eth/v1/beacon/blocks/{block_id}", n.beaconBlock)
	mux.HandleFunc("GET /eth/v1/beacon/states/{state_id}/validators", n.beaconValidators)
	mux.HandleFunc("GET /eth/v1/beacon/states/{state_id}/validator_balances", n.beaconValidatorBalances)
	mux.HandleFunc("GET /eth/v1/beacon/states/{state_id}/finality_checkpoints", n.beaconFinalityCheckpoints)
	mux.HandleFunc("GET /eth/v1/beacon/blobs/{block_id}", n.beaconBlobs)
	mux.HandleFunc("GET /eth/v1/beacon/blob_sidecars/{block_id}", n.beaconBlobSidecars)
	mux.HandleFunc("GET /eth/v1/beacon/data_column_sidecars/{block_id}", n.beaconDataColumns)
	mux.HandleFunc("GET /eth/v1/events", n.beaconEvents)
	return mux
}

func writeBeacon(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(beaconEnvelope{Data: value})
}

func (n *Node) beaconHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (n *Node) beaconSyncing(w http.ResponseWriter, _ *http.Request) {
	writeBeacon(w, http.StatusOK, map[string]any{
		"head_slot":     strconv.FormatUint(n.chain.currentSlot(), 10),
		"sync_distance": "0", "is_syncing": false, "is_optimistic": false, "el_offline": false,
	})
}

func (n *Node) beaconGenesis(w http.ResponseWriter, _ *http.Request) {
	genesis := n.chain.blockchain.GetBlockByNumber(0)
	writeBeacon(w, http.StatusOK, map[string]any{
		"genesis_time":                     strconv.FormatInt(n.cfg.Chain.GenesisTime, 10),
		"genesis_validators_root":          n.consensus.genesisValidatorsRoot,
		"genesis_fork_version":             fmt.Sprintf("%#x", n.consensus.forkVersion(0)),
		"ethertest_execution_genesis_hash": genesis.Hash(),
	})
}

func (n *Node) beaconSpec(w http.ResponseWriter, _ *http.Request) {
	writeBeacon(w, http.StatusOK, map[string]string{
		"CONFIG_NAME":                        "ethertest-minimal",
		"PRESET_BASE":                        "minimal",
		"SECONDS_PER_SLOT":                   strconv.FormatUint(uint64(n.cfg.Chain.SlotDuration/time.Second), 10),
		"SLOTS_PER_EPOCH":                    strconv.FormatUint(n.cfg.Chain.SlotsPerEpoch, 10),
		"MIN_GENESIS_ACTIVE_VALIDATOR_COUNT": strconv.FormatUint(n.cfg.Chain.Validators, 10),
		"DEPOSIT_CHAIN_ID":                   strconv.FormatUint(n.cfg.Chain.ChainID, 10),
		"DEPOSIT_NETWORK_ID":                 strconv.FormatUint(n.cfg.Chain.NetworkID, 10),
	})
}

func (n *Node) beaconForkSchedule(w http.ResponseWriter, _ *http.Request) {
	writeBeacon(w, http.StatusOK, []map[string]string{
		{"previous_version": "0x03000000", "current_version": "0x04000000", "epoch": strconv.FormatUint(n.cfg.Chain.Forks.CancunEpoch, 10)},
		{"previous_version": "0x04000000", "current_version": "0x05000000", "epoch": strconv.FormatUint(n.cfg.Chain.Forks.PragueEpoch, 10)},
		{"previous_version": "0x05000000", "current_version": "0x06000000", "epoch": strconv.FormatUint(n.cfg.Chain.Forks.OsakaEpoch, 10)},
	})
}

func (n *Node) beaconHeader(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	header, err := n.consensus.signedHeader(n.chain, block)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	root, err := header.Message.HashTreeRoot()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	canonical := n.chain.blockchain.GetCanonicalHash(block.NumberU64()) == block.Hash()
	writeBeacon(w, http.StatusOK, map[string]any{"root": common.Hash(root).Hex(), "canonical": canonical, "header": header})
}

func (n *Node) beaconBlock(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	signed, err := n.consensus.signedBlock(n.chain, block)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		data, err := signed.marshalSSZ()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeacon(w, http.StatusOK, signed.value())
}

func (n *Node) beaconFinalityCheckpoints(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("state_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	head := n.chain.slotOf(block)
	safe, finalized := uint64(0), uint64(0)
	if head > n.cfg.Chain.SlotsPerEpoch {
		safe = head - n.cfg.Chain.SlotsPerEpoch
	}
	if head > 2*n.cfg.Chain.SlotsPerEpoch {
		finalized = head - 2*n.cfg.Chain.SlotsPerEpoch
	}
	safeBlock := n.chain.blockAtOrBeforeSlot(safe)
	finalizedBlock := n.chain.blockAtOrBeforeSlot(finalized)
	safeRoot, _ := n.beaconRoot(safeBlock)
	finalizedRoot, _ := n.beaconRoot(finalizedBlock)
	writeBeacon(w, http.StatusOK, map[string]any{
		"previous_justified":  map[string]string{"epoch": strconv.FormatUint(safe/n.cfg.Chain.SlotsPerEpoch, 10), "root": common.Hash(safeRoot).Hex()},
		"current_justified":   map[string]string{"epoch": strconv.FormatUint(safe/n.cfg.Chain.SlotsPerEpoch, 10), "root": common.Hash(safeRoot).Hex()},
		"finalized":           map[string]string{"epoch": strconv.FormatUint(finalized/n.cfg.Chain.SlotsPerEpoch, 10), "root": common.Hash(finalizedRoot).Hex()},
		"ethertest_synthetic": true,
	})
}

func (n *Node) beaconValidators(w http.ResponseWriter, r *http.Request) {
	if _, err := n.beaconBlockID(r.PathValue("state_id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	validators := make([]map[string]any, len(n.consensus.pubkeys))
	for index := range n.consensus.pubkeys {
		validators[index] = map[string]any{
			"index": strconv.Itoa(index), "balance": "32000000000", "status": "active_ongoing",
			"validator": map[string]any{
				"pubkey":                 hexutil.Encode(n.consensus.pubkeys[index][:]),
				"withdrawal_credentials": hexutil.Encode(n.consensus.withdrawalCredentials[index][:]),
				"effective_balance":      "32000000000", "slashed": false,
				"activation_eligibility_epoch": "0", "activation_epoch": "0",
				"exit_epoch":         strconv.FormatUint(^uint64(0), 10),
				"withdrawable_epoch": strconv.FormatUint(^uint64(0), 10),
			},
		}
	}
	writeBeacon(w, http.StatusOK, validators)
}

func (n *Node) beaconValidatorBalances(w http.ResponseWriter, r *http.Request) {
	if _, err := n.beaconBlockID(r.PathValue("state_id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	balances := make([]map[string]string, len(n.consensus.pubkeys))
	for index := range n.consensus.pubkeys {
		balances[index] = map[string]string{"index": strconv.Itoa(index), "balance": "32000000000"}
	}
	writeBeacon(w, http.StatusOK, balances)
}

func (n *Node) beaconBlobs(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	selected, err := requestedVersionedHashes(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items := make([]deneb.Blob, 0)
	for _, tx := range block.Transactions() {
		sidecar := n.chain.blobSidecar(tx.Hash())
		if sidecar == nil {
			continue
		}
		hashes := sidecar.BlobHashes()
		if len(sidecar.Blobs) != len(hashes) {
			http.Error(w, "stored blob sidecar fields have inconsistent lengths", http.StatusInternalServerError)
			return
		}
		for index, blob := range sidecar.Blobs {
			if len(selected) != 0 {
				if _, exists := selected[hashes[index]]; !exists {
					continue
				}
			}
			items = append(items, deneb.Blob(blob))
		}
	}
	if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		w.Header().Set("Content-Type", "application/octet-stream")
		for index := range items {
			_, _ = w.Write(items[index][:])
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(beaconBlobsEnvelope{
		ExecutionOptimistic: false,
		Finalized:           n.beaconBlockFinalized(block),
		Data:                items,
	})
}

func (n *Node) beaconBlobSidecars(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	signedHeader, err := n.consensus.signedHeader(n.chain, block)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	selected, err := requestedIndices(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	items := make([]*deneb.BlobSidecar, 0)
	globalIndex := uint64(0)
	for _, tx := range block.Transactions() {
		sidecar := n.chain.blobSidecar(tx.Hash())
		if sidecar == nil {
			continue
		}
		for index, blob := range sidecar.Blobs {
			if len(selected) != 0 && !selected[globalIndex] {
				globalIndex++
				continue
			}
			proof, err := kzg4844.ComputeBlobProof(&blob, sidecar.Commitments[index])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			inclusionProof, err := n.consensus.blobCommitmentInclusionProof(n.chain, block, globalIndex)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			convertedProof := make(deneb.KZGCommitmentInclusionProof, len(inclusionProof))
			for proofIndex := range inclusionProof {
				convertedProof[proofIndex] = deneb.KZGCommitmentInclusionProofElement(inclusionProof[proofIndex])
			}
			items = append(items, &deneb.BlobSidecar{
				Index: deneb.BlobIndex(globalIndex), Blob: deneb.Blob(blob),
				KZGCommitment: deneb.KZGCommitment(sidecar.Commitments[index]),
				KZGProof:      deneb.KZGProof(proof), SignedBlockHeader: signedHeader,
				KZGCommitmentInclusionProof: convertedProof,
			})
			globalIndex++
		}
	}
	if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		var output bytes.Buffer
		for _, item := range items {
			encoded, err := item.MarshalSSZ()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			output.Write(encoded)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		_, _ = w.Write(output.Bytes())
		return
	}
	writeBeacon(w, http.StatusOK, items)
}

func (n *Node) beaconDataColumns(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	type storedBlob struct {
		blob       kzg4844.Blob
		commitment kzg4844.Commitment
		proofs     []kzg4844.Proof
	}
	var blobs []storedBlob
	for _, tx := range block.Transactions() {
		sidecar := n.chain.blobSidecar(tx.Hash())
		if sidecar == nil || sidecar.Version != types.BlobSidecarVersion1 {
			continue
		}
		for index := range sidecar.Blobs {
			proofs, err := sidecar.CellProofsAt(index)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			blobs = append(blobs, storedBlob{
				blob: sidecar.Blobs[index], commitment: sidecar.Commitments[index], proofs: proofs,
			})
		}
	}
	if len(blobs) == 0 {
		if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
			w.WriteHeader(http.StatusOK)
			return
		}
		writeBeacon(w, http.StatusOK, []any{})
		return
	}
	fullBlobs := make([]kzg4844.Blob, len(blobs))
	for i := range blobs {
		fullBlobs[i] = blobs[i].blob
	}
	cells, err := kzg4844.ComputeCells(fullBlobs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	selected, err := requestedIndices(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	columns := make([]map[string]any, 0, kzg4844.CellProofsPerBlob)
	encodedColumns := make([][]byte, 0, kzg4844.CellProofsPerBlob)
	inclusionProof, err := n.consensus.kzgCommitmentsInclusionProof(n.chain, block)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	signedHeader, err := n.consensus.signedHeader(n.chain, block)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	headerSSZ, err := signedHeader.MarshalSSZ()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	inclusionJSON := make([]string, len(inclusionProof))
	for index := range inclusionProof {
		inclusionJSON[index] = hexutil.Encode(inclusionProof[index][:])
	}
	for column := range kzg4844.CellProofsPerBlob {
		if len(selected) != 0 && !selected[uint64(column)] {
			continue
		}
		columnCells := make([]string, len(blobs))
		commitments := make([]string, len(blobs))
		proofs := make([]string, len(blobs))
		for blobIndex := range blobs {
			cell := cells[blobIndex*kzg4844.CellProofsPerBlob+column]
			columnCells[blobIndex] = hexutil.Encode(cell[:])
			commitments[blobIndex] = hexutil.Encode(blobs[blobIndex].commitment[:])
			proofs[blobIndex] = hexutil.Encode(blobs[blobIndex].proofs[column][:])
		}
		columns = append(columns, map[string]any{
			"index": strconv.Itoa(column), "column": columnCells,
			"kzg_commitments": commitments, "kzg_proofs": proofs,
			"signed_block_header":             signedHeader,
			"kzg_commitments_inclusion_proof": inclusionJSON,
		})
		columnValues := make([]kzg4844.Cell, len(blobs))
		commitmentValues := make([]kzg4844.Commitment, len(blobs))
		proofValues := make([]kzg4844.Proof, len(blobs))
		for blobIndex := range blobs {
			columnValues[blobIndex] = cells[blobIndex*kzg4844.CellProofsPerBlob+column]
			commitmentValues[blobIndex] = blobs[blobIndex].commitment
			proofValues[blobIndex] = blobs[blobIndex].proofs[column]
		}
		encoded, err := marshalDataColumnSSZ(
			uint64(column), columnValues, commitmentValues, proofValues, headerSSZ, inclusionProof,
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		encodedColumns = append(encodedColumns, encoded)
	}
	if strings.Contains(r.Header.Get("Accept"), "application/octet-stream") {
		var output bytes.Buffer
		offset := 4 * len(encodedColumns)
		for _, encoded := range encodedColumns {
			var value [4]byte
			binary.LittleEndian.PutUint32(value[:], uint32(offset))
			output.Write(value[:])
			offset += len(encoded)
		}
		for _, encoded := range encodedColumns {
			output.Write(encoded)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		_, _ = w.Write(output.Bytes())
		return
	}
	writeBeacon(w, http.StatusOK, columns)
}

func requestedIndices(r *http.Request) (map[uint64]bool, error) {
	values := r.URL.Query()["indices"]
	if len(values) == 0 {
		return nil, nil
	}
	indices := make(map[uint64]bool, len(values))
	for _, value := range values {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid index %q", value)
		}
		indices[parsed] = true
	}
	return indices, nil
}

func requestedVersionedHashes(r *http.Request) (map[common.Hash]struct{}, error) {
	values := r.URL.Query()["versioned_hashes"]
	if len(values) == 0 {
		return nil, nil
	}
	hashes := make(map[common.Hash]struct{}, len(values))
	for _, value := range values {
		if !strings.HasPrefix(value, "0x") || len(value) != 2+2*common.HashLength {
			return nil, fmt.Errorf("invalid versioned hash %q", value)
		}
		var hash common.Hash
		if err := hash.UnmarshalText([]byte(value)); err != nil {
			return nil, fmt.Errorf("invalid versioned hash %q", value)
		}
		if _, exists := hashes[hash]; exists {
			return nil, fmt.Errorf("duplicate versioned hash %q", value)
		}
		hashes[hash] = struct{}{}
	}
	return hashes, nil
}

func (n *Node) beaconBlockFinalized(block *types.Block) bool {
	if n.chain.blockchain.GetCanonicalHash(block.NumberU64()) != block.Hash() {
		return false
	}
	finalizedSlot := uint64(0)
	if currentSlot := n.chain.currentSlot(); currentSlot > 2*n.cfg.Chain.SlotsPerEpoch {
		finalizedSlot = currentSlot - 2*n.cfg.Chain.SlotsPerEpoch
	}
	return n.chain.slotOf(block) <= finalizedSlot
}

func marshalDataColumnSSZ(
	index uint64,
	cells []kzg4844.Cell,
	commitments []kzg4844.Commitment,
	proofs []kzg4844.Proof,
	signedHeader []byte,
	inclusionProof [4][32]byte,
) ([]byte, error) {
	if len(cells) != len(commitments) || len(cells) != len(proofs) {
		return nil, errors.New("data column fields have inconsistent lengths")
	}
	if len(signedHeader) != 208 {
		return nil, fmt.Errorf("signed beacon block header is %d bytes, want 208", len(signedHeader))
	}
	const fixedSize = 8 + 4 + 4 + 4 + 208 + 4*32
	columnSize := len(cells) * len(kzg4844.Cell{})
	commitmentSize := len(commitments) * len(kzg4844.Commitment{})
	output := make([]byte, fixedSize, fixedSize+columnSize+commitmentSize+len(proofs)*len(kzg4844.Proof{}))
	binary.LittleEndian.PutUint64(output[:8], index)
	binary.LittleEndian.PutUint32(output[8:12], fixedSize)
	binary.LittleEndian.PutUint32(output[12:16], uint32(fixedSize+columnSize))
	binary.LittleEndian.PutUint32(output[16:20], uint32(fixedSize+columnSize+commitmentSize))
	copy(output[20:228], signedHeader)
	for proofIndex := range inclusionProof {
		copy(output[228+proofIndex*32:228+(proofIndex+1)*32], inclusionProof[proofIndex][:])
	}
	for cellIndex := range cells {
		output = append(output, cells[cellIndex][:]...)
	}
	for commitmentIndex := range commitments {
		output = append(output, commitments[commitmentIndex][:]...)
	}
	for proofIndex := range proofs {
		output = append(output, proofs[proofIndex][:]...)
	}
	return output, nil
}

// func (n *Node) beaconHeaderValue(block *types.Block) any {
// 	header, err := n.consensus.signedHeader(n.chain, block)
// 	if err != nil {
// 		return nil
// 	}
// 	return header
// }

func (n *Node) beaconEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	revision := Revision(0)
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			http.Error(w, "invalid Last-Event-ID", http.StatusBadRequest)
			return
		}
		revision = Revision(parsed)
	}
	if value := r.URL.Query().Get("last_event_id"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			http.Error(w, "invalid last_event_id", http.StatusBadRequest)
			return
		}
		revision = Revision(parsed)
	}
	events, changed, err := n.events.sinceAndWait(revision)
	if err != nil {
		http.Error(w, `{"code":"EVENT_GAP"}`, http.StatusGone)
		return
	}
	w.WriteHeader(http.StatusOK)
	writeEvents := func(events []Event) {
		for _, event := range events {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Revision, event.Type, data)
			revision = event.Revision
		}
	}
	writeEvents(events)
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			events, changed, err = n.events.sinceAndWait(revision)
			if err != nil {
				_, _ = fmt.Fprint(w, "event: gap\ndata: {\"code\":\"EVENT_GAP\"}\n\n")
				flusher.Flush()
				return
			}
			writeEvents(events)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (n *Node) beaconBlockID(id string) (*types.Block, error) {
	switch id {
	case "head", "safe", "finalized", "genesis":
		var tag rpc.BlockNumber
		switch id {
		case "head":
			tag = rpc.LatestBlockNumber
		case "safe":
			tag = rpc.SafeBlockNumber
		case "finalized":
			tag = rpc.FinalizedBlockNumber
		case "genesis":
			tag = rpc.EarliestBlockNumber
		}
		return n.blockByNumber(tag)
	default:
		if strings.HasPrefix(id, "0x") && len(id) == 66 {
			requested := common.HexToHash(id)
			if block := n.chain.blockchain.GetBlockByHash(requested); block != nil {
				return block, nil
			}
			head := n.chain.blockchain.CurrentBlock().Number.Uint64()
			for number := uint64(0); number <= head; number++ {
				block := n.chain.blockchain.GetBlockByNumber(number)
				root, err := n.beaconRoot(block)
				if err == nil && common.Hash(root) == requested {
					return block, nil
				}
			}
			return nil, errors.New("block not found")
		}
		slot, err := strconv.ParseUint(id, 10, 64)
		if err != nil {
			return nil, errors.New("invalid block ID")
		}
		n.chain.mu.RLock()
		hash, exists := n.chain.blockBySlot[slot]
		n.chain.mu.RUnlock()
		if !exists {
			return nil, errors.New("slot was missed")
		}
		block := n.chain.blockchain.GetBlockByHash(hash)
		if block == nil {
			return nil, errors.New("block not found")
		}
		if n.chain.blockchain.GetCanonicalHash(block.NumberU64()) != block.Hash() {
			return nil, errors.New("slot was removed from the canonical chain")
		}
		return block, nil
	}
}

func (n *Node) beaconRoot(block *types.Block) (phase0.Root, error) {
	signed, err := n.consensus.signedBlock(n.chain, block)
	if err != nil {
		return phase0.Root{}, err
	}
	return signed.messageRoot()
}
