package ethertest

//go:generate go run ./internal/beaconcontractgen

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
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

// beaconBlobsEnvelope remains the concrete test fixture type for the blobs
// response; the handler emits the equivalent generated BeaconResponse shape.
type beaconBlobsEnvelope struct {
	ExecutionOptimistic bool         `json:"execution_optimistic"`
	Finalized           bool         `json:"finalized"`
	Data                []deneb.Blob `json:"data"`
	EthertestTainted    bool         `json:"ethertest_tainted,omitempty"`
}

func (n *Node) beaconHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /eth/v1/node/health", n.beaconHealth)
	mux.HandleFunc("GET /eth/v1/node/syncing", n.beaconSyncing)
	mux.HandleFunc("GET /eth/v1/beacon/genesis", n.beaconGenesis)
	mux.HandleFunc("GET /eth/v1/config/spec", n.beaconSpec)
	mux.HandleFunc("GET /eth/v1/config/fork_schedule", n.beaconForkSchedule)
	mux.HandleFunc("GET /eth/v1/beacon/headers/{block_id}", n.beaconHeader)
	mux.HandleFunc("GET /eth/v2/beacon/blocks/{block_id}", n.beaconBlock)
	mux.HandleFunc("GET /eth/v1/beacon/states/{state_id}/validators", n.beaconValidators)
	mux.HandleFunc("GET /eth/v1/beacon/states/{state_id}/validator_balances", n.beaconValidatorBalances)
	mux.HandleFunc("GET /eth/v1/beacon/states/{state_id}/finality_checkpoints", n.beaconFinalityCheckpoints)
	mux.HandleFunc("GET /eth/v1/beacon/blobs/{block_id}", n.beaconBlobs)
	mux.HandleFunc("GET /eth/v1/beacon/blob_sidecars/{block_id}", n.beaconBlobSidecars)
	mux.HandleFunc("GET /eth/v1/debug/beacon/data_column_sidecars/{block_id}", n.beaconDataColumns)
	mux.HandleFunc("GET /eth/v1/events", n.beaconEvents)
	return mux
}

func writeBeacon(w http.ResponseWriter, status int, value any) {
	writeBeaconJSON(w, status, BeaconDataEnvelope[any]{Data: value})
}

func writeBeaconJSON(w http.ResponseWriter, status int, value any) {
	setSyntheticConsensusHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeBeaconError(w http.ResponseWriter, status int, err error) {
	setSyntheticConsensusHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(BeaconErrorMessage{Code: status, Message: err.Error()})
}

func setSyntheticConsensusHeaders(w http.ResponseWriter) {
	w.Header().Set("Ethertest-Consensus-Mode", "synthetic")
}

func (n *Node) beaconResponse(block *types.Block, value any) BeaconResponse[any] {
	tainted := n.beaconLineageTainted(block)
	return BeaconResponse[any]{
		ExecutionOptimistic: tainted,
		Finalized:           n.beaconBlockFinalized(block),
		Data:                value,
		EthertestTainted:    tainted,
	}
}

func (n *Node) beaconVersionedResponse(block *types.Block, value any) BeaconVersionedResponse[any] {
	tainted := n.beaconLineageTainted(block)
	return BeaconVersionedResponse[any]{
		Version:             n.consensus.forkName(n.chain.slotOf(block)),
		ExecutionOptimistic: tainted,
		Finalized:           n.beaconBlockFinalized(block),
		Data:                value,
		EthertestTainted:    tainted,
	}
}

func (n *Node) beaconLineageTainted(block *types.Block) bool {
	if block == nil {
		return true
	}
	safety, err := n.BlockSafety(block.Hash())
	return err != nil || safety.Tainted
}

func beaconBlockIDStatus(err error) int {
	if strings.Contains(strings.ToLower(err.Error()), "invalid") {
		return http.StatusBadRequest
	}
	return http.StatusNotFound
}

func beaconWantsSSZ(r *http.Request) (bool, error) {
	accept := strings.TrimSpace(r.Header.Get("Accept"))
	if accept == "" {
		return false, nil
	}
	jsonQuality, sszQuality := -1.0, -1.0
	for item := range strings.SplitSeq(accept, ",") {
		mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(item))
		if err != nil {
			return false, errors.New("invalid Accept header")
		}
		quality := 1.0
		if raw := parameters["q"]; raw != "" {
			quality, err = strconv.ParseFloat(raw, 64)
			if err != nil || quality < 0 || quality > 1 {
				return false, errors.New("invalid Accept quality")
			}
		}
		switch mediaType {
		case "application/json", "*/*", "application/*":
			if quality > jsonQuality {
				jsonQuality = quality
			}
		case "application/octet-stream":
			if quality > sszQuality {
				sszQuality = quality
			}
		}
	}
	if jsonQuality <= 0 && sszQuality <= 0 {
		return false, errors.New("accepted media type not supported")
	}
	return sszQuality > jsonQuality, nil
}

func (n *Node) beaconHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (n *Node) beaconSyncing(w http.ResponseWriter, _ *http.Request) {
	head := n.chain.blockchain.GetBlockByHash(n.chain.blockchain.CurrentBlock().Hash())
	writeBeacon(w, http.StatusOK, map[string]any{
		"head_slot":     strconv.FormatUint(n.chain.currentSlot(), 10),
		"sync_distance": "0", "is_syncing": false,
		"is_optimistic": n.beaconLineageTainted(head), "el_offline": false,
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
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	header, err := n.consensus.signedHeader(n.chain, block)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	root, err := header.Message.HashTreeRoot()
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	canonical := n.chain.blockchain.GetCanonicalHash(block.NumberU64()) == block.Hash()
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconResponse(block, map[string]any{
		"root": common.Hash(root).Hex(), "canonical": canonical, "header": header,
	}))
}

func (n *Node) beaconBlock(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	signed, err := n.consensus.signedBlock(n.chain, block)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	ssz, err := beaconWantsSSZ(r)
	if err != nil {
		writeBeaconError(w, http.StatusNotAcceptable, err)
		return
	}
	if ssz {
		data, err := signed.marshalSSZ()
		if err != nil {
			writeBeaconError(w, http.StatusInternalServerError, err)
			return
		}
		setSyntheticConsensusHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconVersionedResponse(block, signed.value()))
}

func (n *Node) beaconFinalityCheckpoints(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("state_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	finality := n.resolveSyntheticFinality(n.chain.slotOf(block))
	safeRoot, _ := n.beaconRoot(finality.Safe)
	finalizedRoot, _ := n.beaconRoot(finality.Finalized)
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconResponse(block, map[string]any{
		"previous_justified": map[string]string{"epoch": strconv.FormatUint(finality.SafeSlot/n.cfg.Chain.SlotsPerEpoch, 10), "root": common.Hash(safeRoot).Hex()},
		"current_justified":  map[string]string{"epoch": strconv.FormatUint(finality.SafeSlot/n.cfg.Chain.SlotsPerEpoch, 10), "root": common.Hash(safeRoot).Hex()},
		"finalized":          map[string]string{"epoch": strconv.FormatUint(finality.FinalizedSlot/n.cfg.Chain.SlotsPerEpoch, 10), "root": common.Hash(finalizedRoot).Hex()},
	}))
}

func (n *Node) beaconValidators(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("state_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	selected, status, err := n.requestedValidators(r, true)
	if err != nil {
		writeBeaconError(w, status, err)
		return
	}
	validators := make([]map[string]any, 0, len(n.consensus.pubkeys))
	for index := range n.consensus.pubkeys {
		if selected != nil && !selected[index] {
			continue
		}
		validators = append(validators, map[string]any{
			"index": strconv.Itoa(index), "balance": "32000000000", "status": "active_ongoing",
			"validator": map[string]any{
				"pubkey":                 hexutil.Encode(n.consensus.pubkeys[index][:]),
				"withdrawal_credentials": hexutil.Encode(n.consensus.withdrawalCredentials[index][:]),
				"effective_balance":      "32000000000", "slashed": false,
				"activation_eligibility_epoch": "0", "activation_epoch": "0",
				"exit_epoch":         strconv.FormatUint(^uint64(0), 10),
				"withdrawable_epoch": strconv.FormatUint(^uint64(0), 10),
			},
		})
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconResponse(block, validators))
}

func (n *Node) beaconValidatorBalances(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("state_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	selected, status, err := n.requestedValidators(r, false)
	if err != nil {
		writeBeaconError(w, status, err)
		return
	}
	balances := make([]map[string]string, 0, len(n.consensus.pubkeys))
	for index := range n.consensus.pubkeys {
		if selected != nil && !selected[index] {
			continue
		}
		balances = append(balances, map[string]string{"index": strconv.Itoa(index), "balance": "32000000000"})
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconResponse(block, balances))
}

func (n *Node) requestedValidators(r *http.Request, allowStatus bool) (map[int]bool, int, error) {
	ids := r.URL.Query()["id"]
	if len(ids) > 64 {
		return nil, http.StatusRequestURITooLong, errors.New("too many validator IDs in request")
	}
	selected := make(map[int]bool)
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			return nil, http.StatusBadRequest, fmt.Errorf("duplicate validator ID %q", id)
		}
		seen[id] = struct{}{}
		if index, err := strconv.ParseUint(id, 10, 64); err == nil {
			if index < uint64(len(n.consensus.pubkeys)) {
				selected[int(index)] = true
			}
			continue
		}
		decoded, err := hexutil.Decode(id)
		if err != nil || len(decoded) != phase0.PublicKeyLength {
			return nil, http.StatusBadRequest, fmt.Errorf("invalid validator ID %q", id)
		}
		for index := range n.consensus.pubkeys {
			if bytes.Equal(decoded, n.consensus.pubkeys[index][:]) {
				selected[index] = true
				break
			}
		}
	}
	if len(ids) == 0 {
		selected = nil
	}
	statuses := r.URL.Query()["status"]
	if !allowStatus && len(statuses) != 0 {
		return nil, http.StatusBadRequest, errors.New("status filter is not supported by validator_balances")
	}
	if len(statuses) != 0 {
		matches := false
		seenStatus := make(map[string]struct{}, len(statuses))
		for _, value := range statuses {
			if _, allowed := beaconGeneratedValidatorStatuses[value]; !allowed {
				return nil, http.StatusBadRequest, fmt.Errorf("invalid validator status %q", value)
			}
			if _, duplicate := seenStatus[value]; duplicate {
				return nil, http.StatusBadRequest, fmt.Errorf("duplicate validator status %q", value)
			}
			seenStatus[value] = struct{}{}
			matches = matches || value == "active" || value == "active_ongoing"
		}
		if !matches {
			return map[int]bool{}, http.StatusOK, nil
		}
	}
	return selected, http.StatusOK, nil
}

func (n *Node) beaconBlobs(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	ssz, err := beaconWantsSSZ(r)
	if err != nil {
		writeBeaconError(w, http.StatusNotAcceptable, err)
		return
	}
	selected, err := requestedVersionedHashes(r)
	if err != nil {
		writeBeaconError(w, http.StatusBadRequest, err)
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
			writeBeaconError(w, http.StatusInternalServerError, errors.New("stored blob sidecar fields have inconsistent lengths"))
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
	if ssz {
		setSyntheticConsensusHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		for index := range items {
			_, _ = w.Write(items[index][:])
		}
		return
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconResponse(block, items))
}

func (n *Node) beaconBlobSidecars(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	ssz, err := beaconWantsSSZ(r)
	if err != nil {
		writeBeaconError(w, http.StatusNotAcceptable, err)
		return
	}
	signedHeader, err := n.consensus.signedHeader(n.chain, block)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	selected, err := requestedIndices(r)
	if err != nil {
		writeBeaconError(w, http.StatusBadRequest, err)
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
				writeBeaconError(w, http.StatusInternalServerError, err)
				return
			}
			inclusionProof, err := n.consensus.blobCommitmentInclusionProof(n.chain, block, globalIndex)
			if err != nil {
				writeBeaconError(w, http.StatusInternalServerError, err)
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
	if ssz {
		var output bytes.Buffer
		for _, item := range items {
			encoded, err := item.MarshalSSZ()
			if err != nil {
				writeBeaconError(w, http.StatusInternalServerError, err)
				return
			}
			output.Write(encoded)
		}
		setSyntheticConsensusHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		_, _ = w.Write(output.Bytes())
		return
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconResponse(block, items))
}

func (n *Node) beaconDataColumns(w http.ResponseWriter, r *http.Request) {
	block, err := n.beaconBlockID(r.PathValue("block_id"))
	if err != nil {
		writeBeaconError(w, beaconBlockIDStatus(err), err)
		return
	}
	if n.consensus.forkName(n.chain.slotOf(block)) != "fulu" {
		writeBeaconError(w, http.StatusNotFound, errors.New("data column sidecars are unavailable before Fulu"))
		return
	}
	ssz, err := beaconWantsSSZ(r)
	if err != nil {
		writeBeaconError(w, http.StatusNotAcceptable, err)
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
				writeBeaconError(w, http.StatusInternalServerError, err)
				return
			}
			blobs = append(blobs, storedBlob{
				blob: sidecar.Blobs[index], commitment: sidecar.Commitments[index], proofs: proofs,
			})
		}
	}
	if len(blobs) == 0 {
		if ssz {
			setSyntheticConsensusHeaders(w)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		writeBeaconJSON(w, http.StatusOK, n.beaconVersionedResponse(block, []any{}))
		return
	}
	fullBlobs := make([]kzg4844.Blob, len(blobs))
	for i := range blobs {
		fullBlobs[i] = blobs[i].blob
	}
	cells, err := kzg4844.ComputeCells(fullBlobs)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	selected, err := requestedIndices(r)
	if err != nil {
		writeBeaconError(w, http.StatusBadRequest, err)
		return
	}
	columns := make([]map[string]any, 0, kzg4844.CellProofsPerBlob)
	encodedColumns := make([][]byte, 0, kzg4844.CellProofsPerBlob)
	inclusionProof, err := n.consensus.kzgCommitmentsInclusionProof(n.chain, block)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	signedHeader, err := n.consensus.signedHeader(n.chain, block)
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
		return
	}
	headerSSZ, err := signedHeader.MarshalSSZ()
	if err != nil {
		writeBeaconError(w, http.StatusInternalServerError, err)
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
			writeBeaconError(w, http.StatusInternalServerError, err)
			return
		}
		encodedColumns = append(encodedColumns, encoded)
	}
	if ssz {
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
		setSyntheticConsensusHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
		_, _ = w.Write(output.Bytes())
		return
	}
	w.Header().Set("Eth-Consensus-Version", n.consensus.forkName(n.chain.slotOf(block)))
	writeBeaconJSON(w, http.StatusOK, n.beaconVersionedResponse(block, columns))
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
		if indices[parsed] {
			return nil, fmt.Errorf("duplicate index %q", value)
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
	return n.chain.slotOf(block) <= n.resolveSyntheticFinality(n.chain.currentSlot()).FinalizedSlot
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

func (n *Node) beaconEvents(w http.ResponseWriter, r *http.Request) {
	topics, err := requestedBeaconTopics(r)
	if err != nil {
		writeBeaconError(w, http.StatusBadRequest, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeBeaconError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	revision, lastWireID := Revision(0), uint64(0)
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			writeBeaconError(w, http.StatusBadRequest, errors.New("invalid Last-Event-ID"))
			return
		}
		lastWireID = parsed
		wireRevision := Revision(parsed / 8)
		if wireRevision > 0 {
			revision = wireRevision - 1
		}
	} else {
		revision = n.Revision()
		lastWireID = uint64(revision)*8 + 7
	}
	events, changed, err := n.events.sinceAndWait(revision)
	if err != nil {
		writeBeaconError(w, http.StatusGone, ErrEventGap)
		return
	}
	setSyntheticConsensusHeaders(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	writeEvents := func(events []Event) {
		for _, event := range events {
			for _, message := range n.beaconEventMessages(event, topics) {
				wireID := uint64(event.Revision)*8 + message.ordinal
				if wireID <= lastWireID {
					continue
				}
				data, _ := json.Marshal(message.data)
				_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", wireID, message.topic, data)
				lastWireID = wireID
			}
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

type beaconEventMessage struct {
	topic   string
	ordinal uint64
	data    any
}

func requestedBeaconTopics(r *http.Request) (map[string]bool, error) {
	values, present := r.URL.Query()["topics"]
	if !present || len(values) == 0 {
		return nil, errors.New("topics query parameter is required")
	}
	result := make(map[string]bool, len(values))
	for _, value := range values {
		for topic := range strings.SplitSeq(value, ",") {
			if _, supported := beaconGeneratedEventTopics[topic]; !supported {
				return nil, fmt.Errorf("invalid or unsupported event topic %q", topic)
			}
			if result[topic] {
				return nil, fmt.Errorf("duplicate event topic %q", topic)
			}
			result[topic] = true
		}
	}
	return result, nil
}

func (n *Node) beaconEventMessages(event Event, topics map[string]bool) []beaconEventMessage {
	var messages []beaconEventMessage
	switch event.Type {
	case "block", "control_block":
		if event.Removed {
			return nil
		}
		block := n.chain.blockchain.GetBlockByHash(event.BlockHash)
		if block == nil {
			return nil
		}
		root, err := n.beaconRoot(block)
		if err != nil {
			return nil
		}
		optimistic := n.beaconLineageTainted(block)
		if topics["block"] {
			messages = append(messages, beaconEventMessage{topic: "block", ordinal: 0, data: map[string]any{
				"slot": strconv.FormatUint(event.Slot, 10), "block": common.Hash(root).Hex(),
				"execution_optimistic": optimistic,
			}})
		}
		if topics["head"] {
			header, err := n.consensus.signedHeader(n.chain, block)
			if err != nil {
				return messages
			}
			dependentRoot := common.Hash(root).Hex()
			messages = append(messages, beaconEventMessage{topic: "head", ordinal: 1, data: map[string]any{
				"slot": strconv.FormatUint(event.Slot, 10), "block": common.Hash(root).Hex(),
				"state":                        common.Hash(header.Message.StateRoot).Hex(),
				"epoch_transition":             event.Slot%n.cfg.Chain.SlotsPerEpoch == 0,
				"previous_duty_dependent_root": dependentRoot,
				"current_duty_dependent_root":  dependentRoot,
				"execution_optimistic":         optimistic,
			}})
		}
	case "chain_reorg":
		if !topics["chain_reorg"] {
			return nil
		}
		oldBlock := n.chain.blockchain.GetBlockByHash(event.OldHead)
		newBlock := n.chain.blockchain.GetBlockByHash(event.NewHead)
		if oldBlock == nil || newBlock == nil {
			return nil
		}
		oldRoot, oldErr := n.beaconRoot(oldBlock)
		newRoot, newErr := n.beaconRoot(newBlock)
		oldHeader, oldHeaderErr := n.consensus.signedHeader(n.chain, oldBlock)
		newHeader, newHeaderErr := n.consensus.signedHeader(n.chain, newBlock)
		if oldErr != nil || newErr != nil || oldHeaderErr != nil || newHeaderErr != nil {
			return nil
		}
		messages = append(messages, beaconEventMessage{topic: "chain_reorg", ordinal: 2, data: map[string]any{
			"slot": strconv.FormatUint(event.Slot, 10), "depth": strconv.FormatUint(event.Depth, 10),
			"old_head_block": common.Hash(oldRoot).Hex(), "new_head_block": common.Hash(newRoot).Hex(),
			"old_head_state":       common.Hash(oldHeader.Message.StateRoot).Hex(),
			"new_head_state":       common.Hash(newHeader.Message.StateRoot).Hex(),
			"epoch":                strconv.FormatUint(event.Slot/n.cfg.Chain.SlotsPerEpoch, 10),
			"execution_optimistic": n.beaconLineageTainted(newBlock),
		}})
	case "finalized_checkpoint":
		if !topics["finalized_checkpoint"] {
			return nil
		}
		block := n.chain.blockchain.GetBlockByHash(event.BlockHash)
		if block == nil {
			return nil
		}
		root, rootErr := n.beaconRoot(block)
		header, headerErr := n.consensus.signedHeader(n.chain, block)
		if rootErr != nil || headerErr != nil {
			return nil
		}
		messages = append(messages, beaconEventMessage{topic: "finalized_checkpoint", ordinal: 3, data: map[string]any{
			"block": common.Hash(root).Hex(), "state": common.Hash(header.Message.StateRoot).Hex(),
			"epoch":                strconv.FormatUint(event.Slot/n.cfg.Chain.SlotsPerEpoch, 10),
			"execution_optimistic": n.beaconLineageTainted(block),
		}})
	}
	return messages
}

func (n *Node) beaconBlockID(id string) (*types.Block, error) {
	switch id {
	case "head", "justified", "finalized", "genesis":
		var tag rpc.BlockNumber
		switch id {
		case "head":
			tag = rpc.LatestBlockNumber
		case "justified":
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
			n.chain.mu.RLock()
			hashes := make([]common.Hash, 0, len(n.chain.slotByHash))
			for hash := range n.chain.slotByHash {
				hashes = append(hashes, hash)
			}
			n.chain.mu.RUnlock()
			for _, hash := range hashes {
				block := n.chain.blockchain.GetBlockByHash(hash)
				if block == nil {
					continue
				}
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
		hash, exists := n.chain.canonicalBlockBySlot[slot]
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
