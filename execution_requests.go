package ethertest

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

const (
	executionRequestDeposit byte = iota
	executionRequestWithdrawal
	executionRequestConsolidation

	maxDepositRequestsPerPayload       = 8192
	maxWithdrawalRequestsPerPayload    = 16
	maxConsolidationRequestsPerPayload = 2

	depositRequestSize       = 192
	withdrawalRequestSize    = 76
	consolidationRequestSize = 116

	executionRequestQueueFormat  = 1
	executionRequestRecordFormat = 1
)

var (
	ErrDepositRequestQueueFull       = errors.New("deposit execution request queue already has 8192 entries")
	ErrWithdrawalRequestQueueFull    = errors.New("withdrawal execution request queue already has 16 entries")
	ErrConsolidationRequestQueueFull = errors.New("consolidation execution request queue already has 2 entries")
	ErrExecutionRequestIDOverflow    = errors.New("execution request ID overflow")

	executionRequestQueueKey    = []byte("ethertest/execution-requests/pending")
	executionRequestBlockPrefix = []byte("ethertest/execution-requests/block/")
)

// ExecutionDepositRequest is an EIP-6110 deposit request queued for an Electra
// execution_requests container. It is a synthetic control object and
// does not run a BeaconState deposit transition.
type ExecutionDepositRequest struct {
	Pubkey                [48]byte
	WithdrawalCredentials [32]byte
	Amount                uint64
	Signature             [96]byte
	Index                 uint64
}

// ExecutionWithdrawalRequest is an EIP-7002 withdrawal request queued for an
// Electra execution_requests container. Amount is denominated in Gwei;
// zero retains the protocol's full-exit meaning.
type ExecutionWithdrawalRequest struct {
	SourceAddress   common.Address
	ValidatorPubkey [48]byte
	Amount          uint64
}

// ExecutionConsolidationRequest is an EIP-7251 consolidation request queued
// for an Electra execution_requests container.
type ExecutionConsolidationRequest struct {
	SourceAddress common.Address
	SourcePubkey  [48]byte
	TargetPubkey  [48]byte
}

type queuedExecutionRequest struct {
	ID   uint64 `json:"id"`
	Data []byte `json:"data"`
}

type executionRequestControlSet struct {
	Deposits       []queuedExecutionRequest `json:"deposits"`
	Withdrawals    []queuedExecutionRequest `json:"withdrawals"`
	Consolidations []queuedExecutionRequest `json:"consolidations"`
}

type executionRequestQueue struct {
	Format int    `json:"format"`
	NextID uint64 `json:"next_id"`
	executionRequestControlSet
}

type storedExecutionRequestRecord struct {
	Format         int                        `json:"format"`
	NativeRequests [][]byte                   `json:"native_requests"`
	Requests       [][]byte                   `json:"requests"`
	Controls       executionRequestControlSet `json:"controls"`
}

type preparedExecutionRequestBlock struct {
	Block      *types.Block
	Requests   *electra.ExecutionRequests
	Record     storedExecutionRequestRecord
	Remaining  executionRequestQueue
	Controlled bool
}

func newExecutionRequestQueue() executionRequestQueue {
	return executionRequestQueue{Format: executionRequestQueueFormat, NextID: 1}
}

func (q executionRequestQueue) clone() executionRequestQueue {
	q.Deposits = cloneQueuedExecutionRequests(q.Deposits)
	q.Withdrawals = cloneQueuedExecutionRequests(q.Withdrawals)
	q.Consolidations = cloneQueuedExecutionRequests(q.Consolidations)
	return q
}

func (q executionRequestQueue) empty() bool {
	return len(q.Deposits) == 0 && len(q.Withdrawals) == 0 && len(q.Consolidations) == 0
}

func (q executionRequestQueue) enqueue(requestType byte, data []byte) (executionRequestQueue, error) {
	proposed := q.clone()
	var target *[]queuedExecutionRequest
	var size, limit int
	var fullError error
	switch requestType {
	case executionRequestDeposit:
		target, size, limit, fullError = &proposed.Deposits, depositRequestSize, maxDepositRequestsPerPayload, ErrDepositRequestQueueFull
	case executionRequestWithdrawal:
		target, size, limit, fullError = &proposed.Withdrawals, withdrawalRequestSize, maxWithdrawalRequestsPerPayload, ErrWithdrawalRequestQueueFull
	case executionRequestConsolidation:
		target, size, limit, fullError = &proposed.Consolidations, consolidationRequestSize, maxConsolidationRequestsPerPayload, ErrConsolidationRequestQueueFull
	default:
		return executionRequestQueue{}, fmt.Errorf("unsupported execution request type 0x%02x", requestType)
	}
	if len(data) != size {
		return executionRequestQueue{}, fmt.Errorf("execution request type 0x%02x has length %d, want %d", requestType, len(data), size)
	}
	if len(*target) >= limit {
		return executionRequestQueue{}, fullError
	}
	if proposed.NextID == 0 || proposed.NextID == math.MaxUint64 {
		return executionRequestQueue{}, ErrExecutionRequestIDOverflow
	}
	*target = append(*target, queuedExecutionRequest{ID: proposed.NextID, Data: bytes.Clone(data)})
	proposed.NextID++
	return proposed, nil
}

func cloneQueuedExecutionRequests(items []queuedExecutionRequest) []queuedExecutionRequest {
	result := make([]queuedExecutionRequest, len(items))
	for index, item := range items {
		result[index] = queuedExecutionRequest{ID: item.ID, Data: bytes.Clone(item.Data)}
	}
	return result
}

func cloneExecutionRequestBytes(requests [][]byte) [][]byte {
	if requests == nil {
		return nil
	}
	result := make([][]byte, len(requests))
	for index, request := range requests {
		result[index] = bytes.Clone(request)
	}
	return result
}

func executionRequestRecordKey(hash common.Hash) []byte {
	return hashKey(executionRequestBlockPrefix, hash)
}

func executionRequestQueuePut(queue executionRequestQueue) (journalKV, error) {
	if err := validateExecutionRequestQueue(queue); err != nil {
		return journalKV{}, err
	}
	encoded, err := json.Marshal(queue)
	return journalKV{Key: append([]byte(nil), executionRequestQueueKey...), Value: encoded}, err
}

func executionRequestRecordPut(hash common.Hash, record storedExecutionRequestRecord) (journalKV, error) {
	if err := validateExecutionRequestRecord(record); err != nil {
		return journalKV{}, err
	}
	encoded, err := json.Marshal(record)
	return journalKV{Key: executionRequestRecordKey(hash), Value: encoded}, err
}

func loadExecutionRequestQueue(chain *executionChain) (executionRequestQueue, error) {
	exists, err := chain.db.Has(executionRequestQueueKey)
	if err != nil {
		return executionRequestQueue{}, err
	}
	if !exists {
		return newExecutionRequestQueue(), nil
	}
	encoded, err := chain.db.Get(executionRequestQueueKey)
	if err != nil {
		return executionRequestQueue{}, err
	}
	var queue executionRequestQueue
	if err := json.Unmarshal(encoded, &queue); err != nil {
		return executionRequestQueue{}, fmt.Errorf("decode execution request queue: %w", err)
	}
	if err := validateExecutionRequestQueue(queue); err != nil {
		return executionRequestQueue{}, fmt.Errorf("validate execution request queue: %w", err)
	}
	return queue, nil
}

func loadExecutionRequestRecord(chain *executionChain, hash common.Hash) (storedExecutionRequestRecord, bool, error) {
	key := executionRequestRecordKey(hash)
	exists, err := chain.db.Has(key)
	if err != nil || !exists {
		return storedExecutionRequestRecord{}, exists, err
	}
	encoded, err := chain.db.Get(key)
	if err != nil {
		return storedExecutionRequestRecord{}, false, err
	}
	var record storedExecutionRequestRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return storedExecutionRequestRecord{}, false, fmt.Errorf("decode execution request record %s: %w", hash, err)
	}
	if record.Format != executionRequestRecordFormat {
		return storedExecutionRequestRecord{}, false, fmt.Errorf("unsupported execution request record format %d", record.Format)
	}
	if err := validateExecutionRequestRecord(record); err != nil {
		return storedExecutionRequestRecord{}, false, fmt.Errorf("validate execution request record %s: %w", hash, err)
	}
	return record, true, nil
}

func validateExecutionRequestRecord(record storedExecutionRequestRecord) error {
	if record.Requests != nil && record.NativeRequests == nil {
		return errors.New("prague execution request record has no native request list")
	}
	native, err := parseExecutionRequests(record.NativeRequests)
	if err != nil {
		return err
	}
	if _, err := parseExecutionRequests(record.Requests); err != nil {
		return err
	}
	if err := validateExecutionRequestControlSet(record.Controls); err != nil {
		return err
	}
	combined, err := appendExecutionRequestControls(native, record.Controls)
	if err != nil {
		return err
	}
	combinedBytes, err := marshalExecutionRequests(combined)
	if err != nil {
		return err
	}
	if !equalExecutionRequestBytes(combinedBytes, record.Requests) {
		return errors.New("native execution requests and recorded controls do not reconstruct the full request list")
	}
	return nil
}

func executionRequestEntries(requests [][]byte, requestType byte) ([][]byte, error) {
	size := 0
	switch requestType {
	case executionRequestDeposit:
		size = depositRequestSize
	case executionRequestWithdrawal:
		size = withdrawalRequestSize
	case executionRequestConsolidation:
		size = consolidationRequestSize
	default:
		return nil, fmt.Errorf("unsupported execution request type 0x%02x", requestType)
	}
	for _, request := range requests {
		if len(request) == 0 || request[0] != requestType {
			continue
		}
		payload := request[1:]
		if len(payload)%size != 0 {
			return nil, fmt.Errorf("type 0x%02x payload has length %d", requestType, len(payload))
		}
		entries := make([][]byte, 0, len(payload)/size)
		for offset := 0; offset < len(payload); offset += size {
			entries = append(entries, bytes.Clone(payload[offset:offset+size]))
		}
		return entries, nil
	}
	return nil, nil
}

func encodeExecutionDepositRequest(request ExecutionDepositRequest) []byte {
	result := make([]byte, depositRequestSize)
	copy(result[:48], request.Pubkey[:])
	copy(result[48:80], request.WithdrawalCredentials[:])
	binary.LittleEndian.PutUint64(result[80:88], request.Amount)
	copy(result[88:184], request.Signature[:])
	binary.LittleEndian.PutUint64(result[184:192], request.Index)
	return result
}

func encodeExecutionWithdrawalRequest(request ExecutionWithdrawalRequest) []byte {
	result := make([]byte, withdrawalRequestSize)
	copy(result[:20], request.SourceAddress[:])
	copy(result[20:68], request.ValidatorPubkey[:])
	binary.LittleEndian.PutUint64(result[68:76], request.Amount)
	return result
}

func encodeExecutionConsolidationRequest(request ExecutionConsolidationRequest) []byte {
	result := make([]byte, consolidationRequestSize)
	copy(result[:20], request.SourceAddress[:])
	copy(result[20:68], request.SourcePubkey[:])
	copy(result[68:116], request.TargetPubkey[:])
	return result
}

func decodeDepositRequest(data []byte) (*electra.DepositRequest, error) {
	if len(data) != depositRequestSize {
		return nil, fmt.Errorf("deposit request has length %d, want %d", len(data), depositRequestSize)
	}
	request := &electra.DepositRequest{WithdrawalCredentials: bytes.Clone(data[48:80])}
	copy(request.Pubkey[:], data[:48])
	request.Amount = phase0.Gwei(binary.LittleEndian.Uint64(data[80:88]))
	copy(request.Signature[:], data[88:184])
	request.Index = binary.LittleEndian.Uint64(data[184:192])
	return request, nil
}

func decodeWithdrawalRequest(data []byte) (*electra.WithdrawalRequest, error) {
	if len(data) != withdrawalRequestSize {
		return nil, fmt.Errorf("withdrawal request has length %d, want %d", len(data), withdrawalRequestSize)
	}
	request := new(electra.WithdrawalRequest)
	copy(request.SourceAddress[:], data[:20])
	copy(request.ValidatorPubkey[:], data[20:68])
	request.Amount = phase0.Gwei(binary.LittleEndian.Uint64(data[68:76]))
	return request, nil
}

func decodeConsolidationRequest(data []byte) (*electra.ConsolidationRequest, error) {
	if len(data) != consolidationRequestSize {
		return nil, fmt.Errorf("consolidation request has length %d, want %d", len(data), consolidationRequestSize)
	}
	request := new(electra.ConsolidationRequest)
	copy(request.SourceAddress[:], data[:20])
	copy(request.SourcePubkey[:], data[20:68])
	copy(request.TargetPubkey[:], data[68:116])
	return request, nil
}

func parseExecutionRequests(requests [][]byte) (*electra.ExecutionRequests, error) {
	if requests == nil {
		return nil, nil
	}
	result := &electra.ExecutionRequests{
		Deposits:       []*electra.DepositRequest{},
		Withdrawals:    []*electra.WithdrawalRequest{},
		Consolidations: []*electra.ConsolidationRequest{},
	}
	previousType := -1
	for index, request := range requests {
		if len(request) <= 1 {
			return nil, fmt.Errorf("execution request item %d has no payload", index)
		}
		requestType := int(request[0])
		if requestType > int(executionRequestConsolidation) {
			return nil, fmt.Errorf("unsupported execution request type 0x%02x", request[0])
		}
		if requestType <= previousType {
			return nil, errors.New("execution request types are not strictly increasing")
		}
		previousType = requestType
		payload := request[1:]
		switch byte(requestType) {
		case executionRequestDeposit:
			if len(payload)%depositRequestSize != 0 {
				return nil, fmt.Errorf("deposit request payload has length %d", len(payload))
			}
			for offset := 0; offset < len(payload); offset += depositRequestSize {
				value, err := decodeDepositRequest(payload[offset : offset+depositRequestSize])
				if err != nil {
					return nil, err
				}
				result.Deposits = append(result.Deposits, value)
			}
		case executionRequestWithdrawal:
			if len(payload)%withdrawalRequestSize != 0 {
				return nil, fmt.Errorf("withdrawal request payload has length %d", len(payload))
			}
			for offset := 0; offset < len(payload); offset += withdrawalRequestSize {
				value, err := decodeWithdrawalRequest(payload[offset : offset+withdrawalRequestSize])
				if err != nil {
					return nil, err
				}
				result.Withdrawals = append(result.Withdrawals, value)
			}
		case executionRequestConsolidation:
			if len(payload)%consolidationRequestSize != 0 {
				return nil, fmt.Errorf("consolidation request payload has length %d", len(payload))
			}
			for offset := 0; offset < len(payload); offset += consolidationRequestSize {
				value, err := decodeConsolidationRequest(payload[offset : offset+consolidationRequestSize])
				if err != nil {
					return nil, err
				}
				result.Consolidations = append(result.Consolidations, value)
			}
		}
	}
	if len(result.Deposits) > maxDepositRequestsPerPayload {
		return nil, fmt.Errorf("block has %d deposit requests, max %d", len(result.Deposits), maxDepositRequestsPerPayload)
	}
	if len(result.Withdrawals) > maxWithdrawalRequestsPerPayload {
		return nil, fmt.Errorf("block has %d withdrawal requests, max %d", len(result.Withdrawals), maxWithdrawalRequestsPerPayload)
	}
	if len(result.Consolidations) > maxConsolidationRequestsPerPayload {
		return nil, fmt.Errorf("block has %d consolidation requests, max %d", len(result.Consolidations), maxConsolidationRequestsPerPayload)
	}
	return result, nil
}

func marshalExecutionRequests(requests *electra.ExecutionRequests) ([][]byte, error) {
	if requests == nil {
		return nil, nil
	}
	result := make([][]byte, 0, 3)
	if len(requests.Deposits) != 0 {
		item := []byte{executionRequestDeposit}
		for index, request := range requests.Deposits {
			if request == nil || len(request.WithdrawalCredentials) != 32 {
				return nil, fmt.Errorf("deposit request %d is invalid", index)
			}
			value := ExecutionDepositRequest{Amount: uint64(request.Amount), Index: request.Index}
			copy(value.Pubkey[:], request.Pubkey[:])
			copy(value.WithdrawalCredentials[:], request.WithdrawalCredentials)
			copy(value.Signature[:], request.Signature[:])
			item = append(item, encodeExecutionDepositRequest(value)...)
		}
		result = append(result, item)
	}
	if len(requests.Withdrawals) != 0 {
		item := []byte{executionRequestWithdrawal}
		for index, request := range requests.Withdrawals {
			if request == nil {
				return nil, fmt.Errorf("withdrawal request %d is invalid", index)
			}
			value := ExecutionWithdrawalRequest{
				SourceAddress: common.Address(request.SourceAddress), Amount: uint64(request.Amount),
			}
			copy(value.ValidatorPubkey[:], request.ValidatorPubkey[:])
			item = append(item, encodeExecutionWithdrawalRequest(value)...)
		}
		result = append(result, item)
	}
	if len(requests.Consolidations) != 0 {
		item := []byte{executionRequestConsolidation}
		for index, request := range requests.Consolidations {
			if request == nil {
				return nil, fmt.Errorf("consolidation request %d is invalid", index)
			}
			value := ExecutionConsolidationRequest{SourceAddress: common.Address(request.SourceAddress)}
			copy(value.SourcePubkey[:], request.SourcePubkey[:])
			copy(value.TargetPubkey[:], request.TargetPubkey[:])
			item = append(item, encodeExecutionConsolidationRequest(value)...)
		}
		result = append(result, item)
	}
	return result, nil
}

func verifyExecutionRequestsHash(block *types.Block, requests [][]byte) error {
	requestsHash := block.RequestsHash()
	if requests == nil {
		if requestsHash != nil {
			return fmt.Errorf("pre-Prague block %s has requests hash %s", block.Hash(), *requestsHash)
		}
		return nil
	}
	calculated := types.CalcRequestsHash(requests)
	if requestsHash == nil {
		return fmt.Errorf("prague block %s has no requests hash", block.Hash())
	}
	if *requestsHash != calculated {
		return fmt.Errorf("block %s requests hash mismatch: header %s, calculated %s", block.Hash(), *requestsHash, calculated)
	}
	return nil
}

func equalExecutionRequestBytes(left, right [][]byte) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func appendExecutionRequestControls(
	requests *electra.ExecutionRequests,
	controls executionRequestControlSet,
) (*electra.ExecutionRequests, error) {
	if requests == nil {
		if len(controls.Deposits)+len(controls.Withdrawals)+len(controls.Consolidations) != 0 {
			return nil, errors.New("pre-Prague block cannot contain execution request controls")
		}
		return nil, nil
	}
	combined := cloneElectraExecutionRequests(requests)
	for _, item := range controls.Deposits {
		value, err := decodeDepositRequest(item.Data)
		if err != nil {
			return nil, err
		}
		combined.Deposits = append(combined.Deposits, value)
	}
	for _, item := range controls.Withdrawals {
		value, err := decodeWithdrawalRequest(item.Data)
		if err != nil {
			return nil, err
		}
		combined.Withdrawals = append(combined.Withdrawals, value)
	}
	for _, item := range controls.Consolidations {
		value, err := decodeConsolidationRequest(item.Data)
		if err != nil {
			return nil, err
		}
		combined.Consolidations = append(combined.Consolidations, value)
	}
	return combined, nil
}

func prepareExecutionRequestBlock(
	block *types.Block,
	native [][]byte,
	queue executionRequestQueue,
) (preparedExecutionRequestBlock, error) {
	if err := verifyExecutionRequestsHash(block, native); err != nil {
		return preparedExecutionRequestBlock{}, err
	}
	parsed, err := parseExecutionRequests(native)
	if err != nil {
		return preparedExecutionRequestBlock{}, err
	}
	roundTrip, err := marshalExecutionRequests(parsed)
	if err != nil {
		return preparedExecutionRequestBlock{}, err
	}
	if !equalExecutionRequestBytes(roundTrip, native) {
		return preparedExecutionRequestBlock{}, errors.New("native execution requests changed during decoding")
	}
	remaining := queue.clone()
	prepared := preparedExecutionRequestBlock{
		Block: block, Requests: parsed, Remaining: remaining,
		Record: storedExecutionRequestRecord{
			Format: executionRequestRecordFormat, NativeRequests: cloneExecutionRequestBytes(native),
			Requests: cloneExecutionRequestBytes(native),
		},
	}
	if native == nil {
		return prepared, nil
	}
	depositCount := min(maxDepositRequestsPerPayload-len(parsed.Deposits), len(remaining.Deposits))
	withdrawalCount := min(maxWithdrawalRequestsPerPayload-len(parsed.Withdrawals), len(remaining.Withdrawals))
	consolidationCount := min(maxConsolidationRequestsPerPayload-len(parsed.Consolidations), len(remaining.Consolidations))
	prepared.Record.Controls = executionRequestControlSet{
		Deposits:       cloneQueuedExecutionRequests(remaining.Deposits[:depositCount]),
		Withdrawals:    cloneQueuedExecutionRequests(remaining.Withdrawals[:withdrawalCount]),
		Consolidations: cloneQueuedExecutionRequests(remaining.Consolidations[:consolidationCount]),
	}
	remaining.Deposits = cloneQueuedExecutionRequests(remaining.Deposits[depositCount:])
	remaining.Withdrawals = cloneQueuedExecutionRequests(remaining.Withdrawals[withdrawalCount:])
	remaining.Consolidations = cloneQueuedExecutionRequests(remaining.Consolidations[consolidationCount:])
	prepared.Remaining = remaining
	parsed, err = appendExecutionRequestControls(parsed, prepared.Record.Controls)
	if err != nil {
		return preparedExecutionRequestBlock{}, err
	}
	prepared.Requests = parsed
	combined, err := marshalExecutionRequests(parsed)
	if err != nil {
		return preparedExecutionRequestBlock{}, err
	}
	if len(prepared.Record.Controls.Deposits)+len(prepared.Record.Controls.Withdrawals)+len(prepared.Record.Controls.Consolidations) == 0 {
		return prepared, nil
	}
	prepared.Controlled = true
	prepared.Record.Requests = cloneExecutionRequestBytes(combined)
	header := block.Header()
	hash := types.CalcRequestsHash(combined)
	header.RequestsHash = &hash
	prepared.Block = block.WithSeal(header)
	return prepared, nil
}

// AddDepositRequest persistently queues an EIP-6110-shaped synthetic request
// for the next canonical Prague-or-later block with remaining deposit capacity.
func (n *Node) AddDepositRequest(ctx context.Context, request ExecutionDepositRequest) error {
	return n.addExecutionRequest(ctx, executionRequestDeposit, encodeExecutionDepositRequest(request))
}

// AddWithdrawalRequest persistently queues an EIP-7002-shaped synthetic
// request for the next canonical Prague-or-later block with remaining capacity.
func (n *Node) AddWithdrawalRequest(ctx context.Context, request ExecutionWithdrawalRequest) error {
	return n.addExecutionRequest(ctx, executionRequestWithdrawal, encodeExecutionWithdrawalRequest(request))
}

// AddConsolidationRequest persistently queues an EIP-7251-shaped synthetic
// request for the next canonical Prague-or-later block with remaining capacity.
func (n *Node) AddConsolidationRequest(ctx context.Context, request ExecutionConsolidationRequest) error {
	return n.addExecutionRequest(ctx, executionRequestConsolidation, encodeExecutionConsolidationRequest(request))
}

func (n *Node) addExecutionRequest(ctx context.Context, requestType byte, data []byte) error {
	_, err := n.execute(ctx, func(chain *executionChain) (any, error) {
		proposed, err := n.pendingExecutionRequests.enqueue(requestType, data)
		if err != nil {
			return nil, err
		}
		candidate, err := n.pendingCandidate(chain, proposed)
		if err != nil {
			return nil, err
		}
		queueMutation, err := executionRequestQueuePut(proposed)
		if err != nil {
			return nil, err
		}
		if err := n.commitAuxiliary(chain, []journalKV{queueMutation}, nil, nil, func() {
			n.pendingExecutionRequests = proposed
			chain.setPendingView(candidate.block, candidate.state, candidate.receipts)
		}); err != nil {
			return nil, err
		}
		return nil, nil
	})
	return err
}

func (n *Node) pendingCandidate(chain *executionChain, queue executionRequestQueue) (*pendingView, error) {
	parentHeader := chain.blockchain.CurrentBlock()
	parent := chain.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	projection, err := n.consensus.ensureProjection(chain, parent)
	if err != nil {
		return nil, err
	}
	block, receipts, _, native, err := chain.buildBlock(
		uint64(n.cfg.Chain.SlotDuration/time.Second), false, common.Hash(projection.Root), n.pendingWithdrawals,
	)
	if err != nil {
		return nil, err
	}
	prepared, err := prepareExecutionRequestBlock(block, native, queue)
	if err != nil {
		return nil, err
	}
	statedb, err := chain.blockchain.StateAt(prepared.Block.Header())
	if err != nil {
		return nil, err
	}
	return &pendingView{block: prepared.Block, state: statedb, receipts: receipts}, nil
}

func validateExecutionRequestControlSet(controls executionRequestControlSet) error {
	seen := make(map[uint64]struct{}, len(controls.Deposits)+len(controls.Withdrawals)+len(controls.Consolidations))
	for _, group := range []struct {
		name  string
		size  int
		items []queuedExecutionRequest
	}{
		{name: "deposit", size: depositRequestSize, items: controls.Deposits},
		{name: "withdrawal", size: withdrawalRequestSize, items: controls.Withdrawals},
		{name: "consolidation", size: consolidationRequestSize, items: controls.Consolidations},
	} {
		previousID := uint64(0)
		for index, item := range group.items {
			if item.ID == 0 {
				return fmt.Errorf("%s control %d has zero ID", group.name, index)
			}
			if len(item.Data) != group.size {
				return fmt.Errorf("%s control %d has length %d, want %d", group.name, index, len(item.Data), group.size)
			}
			if _, duplicate := seen[item.ID]; duplicate {
				return fmt.Errorf("execution request control ID %d is duplicated", item.ID)
			}
			if item.ID <= previousID {
				return fmt.Errorf("%s control IDs are not strictly increasing", group.name)
			}
			seen[item.ID] = struct{}{}
			previousID = item.ID
		}
	}
	return nil
}

func validateExecutionRequestQueue(queue executionRequestQueue) error {
	if queue.Format != executionRequestQueueFormat {
		return fmt.Errorf("unsupported execution request queue format %d", queue.Format)
	}
	if err := validateExecutionRequestControlSet(queue.executionRequestControlSet); err != nil {
		return err
	}
	maxID := uint64(0)
	for _, items := range [][]queuedExecutionRequest{queue.Deposits, queue.Withdrawals, queue.Consolidations} {
		for _, item := range items {
			maxID = max(maxID, item.ID)
		}
	}
	if queue.NextID == 0 || queue.NextID <= maxID {
		return fmt.Errorf("execution request next ID %d does not exceed pending ID %d", queue.NextID, maxID)
	}
	return nil
}

func reconcileExecutionRequestQueue(
	chain *executionChain,
	queue executionRequestQueue,
	oldPath []*types.Block,
	newPath []*types.Block,
) (executionRequestQueue, error) {
	result := queue.clone()
	mapsByType := []map[uint64]queuedExecutionRequest{
		make(map[uint64]queuedExecutionRequest), make(map[uint64]queuedExecutionRequest), make(map[uint64]queuedExecutionRequest),
	}
	type typedControl struct {
		RequestType int
		Data        []byte
	}
	known := make(map[uint64]typedControl)
	remember := func(requestType int, item queuedExecutionRequest) error {
		if existing, exists := known[item.ID]; exists {
			if existing.RequestType != requestType || !bytes.Equal(existing.Data, item.Data) {
				return fmt.Errorf("execution request control ID %d has conflicting values", item.ID)
			}
			return nil
		}
		known[item.ID] = typedControl{RequestType: requestType, Data: bytes.Clone(item.Data)}
		return nil
	}
	for requestType, items := range [][]queuedExecutionRequest{result.Deposits, result.Withdrawals, result.Consolidations} {
		for _, item := range items {
			if err := remember(requestType, item); err != nil {
				return executionRequestQueue{}, err
			}
			mapsByType[requestType][item.ID] = item
		}
	}
	add := func(controls executionRequestControlSet) error {
		for requestType, items := range [][]queuedExecutionRequest{controls.Deposits, controls.Withdrawals, controls.Consolidations} {
			for _, item := range items {
				if err := remember(requestType, item); err != nil {
					return err
				}
				mapsByType[requestType][item.ID] = queuedExecutionRequest{ID: item.ID, Data: bytes.Clone(item.Data)}
			}
		}
		return nil
	}
	for _, block := range slices.Backward(oldPath) {
		record, exists, err := loadExecutionRequestRecord(chain, block.Hash())
		if err != nil {
			return executionRequestQueue{}, err
		}
		if exists {
			if err := add(record.Controls); err != nil {
				return executionRequestQueue{}, err
			}
		}
	}
	consumedNew := make(map[uint64]struct{})
	for _, block := range slices.Backward(newPath) {
		record, exists, err := loadExecutionRequestRecord(chain, block.Hash())
		if err != nil {
			return executionRequestQueue{}, err
		}
		if !exists {
			continue
		}
		for requestType, items := range [][]queuedExecutionRequest{record.Controls.Deposits, record.Controls.Withdrawals, record.Controls.Consolidations} {
			for _, item := range items {
				if err := remember(requestType, item); err != nil {
					return executionRequestQueue{}, err
				}
				if _, duplicate := consumedNew[item.ID]; duplicate {
					return executionRequestQueue{}, fmt.Errorf("new canonical path consumes execution request control ID %d more than once", item.ID)
				}
				if _, available := mapsByType[requestType][item.ID]; !available {
					return executionRequestQueue{}, fmt.Errorf("new canonical path consumes unavailable execution request control ID %d", item.ID)
				}
				consumedNew[item.ID] = struct{}{}
				delete(mapsByType[requestType], item.ID)
			}
		}
	}
	toSortedSlice := func(values map[uint64]queuedExecutionRequest) []queuedExecutionRequest {
		items := make([]queuedExecutionRequest, 0, len(values))
		for _, item := range values {
			items = append(items, queuedExecutionRequest{ID: item.ID, Data: bytes.Clone(item.Data)})
		}
		slices.SortFunc(items, func(left, right queuedExecutionRequest) int {
			return cmp.Compare(left.ID, right.ID)
		})
		return items
	}
	result.Deposits = toSortedSlice(mapsByType[executionRequestDeposit])
	result.Withdrawals = toSortedSlice(mapsByType[executionRequestWithdrawal])
	result.Consolidations = toSortedSlice(mapsByType[executionRequestConsolidation])
	if err := validateExecutionRequestControlSet(result.executionRequestControlSet); err != nil {
		return executionRequestQueue{}, err
	}
	return result, nil
}

func executionRequestsFromConsensusBlock(block *consensusBlock) ([][]byte, error) {
	if block == nil {
		return nil, errors.New("beacon projection is missing")
	}
	if block.deneb != nil {
		return nil, nil
	}
	if block.electra == nil || block.electra.Message == nil || block.electra.Message.Body == nil ||
		block.electra.Message.Body.ExecutionRequests == nil {
		return nil, errors.New("electra Beacon projection has no execution requests")
	}
	return marshalExecutionRequests(block.electra.Message.Body.ExecutionRequests)
}

func deriveNativeExecutionRequests(
	chain *executionChain,
	block *types.Block,
	projectionRequests [][]byte,
) ([][]byte, error) {
	if len(projectionRequests) == 0 {
		return cloneExecutionRequestBytes(projectionRequests), nil
	}
	requests, err := executeNativeExecutionRequests(chain, block)
	if err != nil {
		return nil, err
	}
	if !equalExecutionRequestBytes(requests, projectionRequests) {
		return nil, fmt.Errorf(
			"block %s Beacon execution requests cannot be recovered as native output; control IDs are unavailable",
			block.Hash(),
		)
	}
	return requests, nil
}

func executeNativeExecutionRequests(chain *executionChain, block *types.Block) ([][]byte, error) {
	if block.NumberU64() == 0 {
		return nil, errors.New("genesis cannot contain execution requests")
	}
	parent := chain.blockchain.GetBlockByHash(block.ParentHash())
	if parent == nil {
		return nil, fmt.Errorf("execution parent %s is missing", block.ParentHash())
	}
	statedb, err := chain.blockchain.StateAt(parent.Header())
	if err != nil {
		return nil, fmt.Errorf("open parent state for block %s: %w", block.Hash(), err)
	}
	result, err := chain.blockchain.Processor().Process(context.Background(), block, statedb, nil, vm.Config{}, nil)
	if err != nil {
		return nil, fmt.Errorf("re-execute block %s to recover native execution requests: %w", block.Hash(), err)
	}
	return cloneExecutionRequestBytes(result.Requests), nil
}

func validateExecutionRequestMetadata(n *Node, chain *executionChain) error {
	if err := validateExecutionRequestControlSet(n.pendingExecutionRequests.executionRequestControlSet); err != nil {
		return err
	}
	type typedControl struct {
		RequestType int
		Data        []byte
	}
	storedIDs := make(map[uint64]typedControl)
	remember := func(requestType int, item queuedExecutionRequest) error {
		if existing, exists := storedIDs[item.ID]; exists {
			if existing.RequestType != requestType || !bytes.Equal(existing.Data, item.Data) {
				return fmt.Errorf("execution request control ID %d has conflicting stored values", item.ID)
			}
			return nil
		}
		storedIDs[item.ID] = typedControl{RequestType: requestType, Data: bytes.Clone(item.Data)}
		return nil
	}
	pendingIDs := make(map[uint64]struct{})
	maxID := uint64(0)
	for requestType, items := range [][]queuedExecutionRequest{
		n.pendingExecutionRequests.Deposits, n.pendingExecutionRequests.Withdrawals, n.pendingExecutionRequests.Consolidations,
	} {
		for _, item := range items {
			if err := remember(requestType, item); err != nil {
				return err
			}
			pendingIDs[item.ID] = struct{}{}
			maxID = max(maxID, item.ID)
		}
	}
	chain.mu.RLock()
	hashes := make([]common.Hash, 0, len(chain.slotByHash))
	for hash := range chain.slotByHash {
		hashes = append(hashes, hash)
	}
	chain.mu.RUnlock()
	canonicalIDs := make(map[uint64]common.Hash)
	derivedRecords := make([]journalKV, 0)
	for _, hash := range hashes {
		block := chain.blockchain.GetBlockByHash(hash)
		if block == nil {
			return fmt.Errorf("execution request metadata references missing block %s", hash)
		}
		projected, _, exists, err := loadProjection(chain, hash)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("execution block %s has no Beacon projection", hash)
		}
		projectionRequests, err := executionRequestsFromConsensusBlock(projected)
		if err != nil {
			return fmt.Errorf("read execution requests from projection %s: %w", hash, err)
		}
		record, recorded, err := loadExecutionRequestRecord(chain, hash)
		if err != nil {
			return err
		}
		var requests [][]byte
		if recorded {
			if !equalExecutionRequestBytes(record.Requests, projectionRequests) {
				return fmt.Errorf("execution request record %s does not match Beacon projection", hash)
			}
			if len(record.Requests) != 0 {
				nativeRequests, err := executeNativeExecutionRequests(chain, block)
				if err != nil {
					return err
				}
				if !equalExecutionRequestBytes(nativeRequests, record.NativeRequests) {
					return fmt.Errorf("execution request record %s does not match re-executed native output", hash)
				}
			}
			requests = record.Requests
		} else {
			requests, err = deriveNativeExecutionRequests(chain, block, projectionRequests)
			if err != nil {
				return err
			}
			record = storedExecutionRequestRecord{
				Format: executionRequestRecordFormat, NativeRequests: requests, Requests: requests,
			}
			mutation, err := executionRequestRecordPut(hash, record)
			if err != nil {
				return err
			}
			derivedRecords = append(derivedRecords, mutation)
		}
		if err := verifyExecutionRequestsHash(block, requests); err != nil {
			return err
		}
		if recorded {
			for requestType, items := range [][]queuedExecutionRequest{
				record.Controls.Deposits, record.Controls.Withdrawals, record.Controls.Consolidations,
			} {
				for _, item := range items {
					if err := remember(requestType, item); err != nil {
						return err
					}
					maxID = max(maxID, item.ID)
				}
			}
		}
		if !recorded || chain.blockchain.GetCanonicalHash(block.NumberU64()) != hash {
			continue
		}
		for _, items := range [][]queuedExecutionRequest{record.Controls.Deposits, record.Controls.Withdrawals, record.Controls.Consolidations} {
			for _, item := range items {
				if _, pending := pendingIDs[item.ID]; pending {
					return fmt.Errorf("canonical execution request control ID %d is still pending", item.ID)
				}
				if previous, duplicate := canonicalIDs[item.ID]; duplicate {
					return fmt.Errorf("canonical blocks %s and %s both consume execution request control ID %d", previous, hash, item.ID)
				}
				canonicalIDs[item.ID] = hash
			}
		}
	}
	if n.pendingExecutionRequests.NextID == 0 || n.pendingExecutionRequests.NextID <= maxID {
		return fmt.Errorf("execution request next ID %d does not exceed stored ID %d", n.pendingExecutionRequests.NextID, maxID)
	}
	if len(derivedRecords) != 0 {
		batch := chain.db.NewBatch()
		for _, mutation := range derivedRecords {
			if err := batch.Put(mutation.Key, mutation.Value); err != nil {
				return err
			}
		}
		if err := batch.Write(); err != nil {
			return fmt.Errorf("persist derived execution request records: %w", err)
		}
	}
	return nil
}

func cloneElectraExecutionRequests(requests *electra.ExecutionRequests) *electra.ExecutionRequests {
	if requests == nil {
		return &electra.ExecutionRequests{
			Deposits: []*electra.DepositRequest{}, Withdrawals: []*electra.WithdrawalRequest{},
			Consolidations: []*electra.ConsolidationRequest{},
		}
	}
	cloned := &electra.ExecutionRequests{
		Deposits:       make([]*electra.DepositRequest, len(requests.Deposits)),
		Withdrawals:    make([]*electra.WithdrawalRequest, len(requests.Withdrawals)),
		Consolidations: make([]*electra.ConsolidationRequest, len(requests.Consolidations)),
	}
	for index, request := range requests.Deposits {
		value := *request
		value.WithdrawalCredentials = bytes.Clone(request.WithdrawalCredentials)
		cloned.Deposits[index] = &value
	}
	for index, request := range requests.Withdrawals {
		value := *request
		cloned.Withdrawals[index] = &value
	}
	for index, request := range requests.Consolidations {
		value := *request
		cloned.Consolidations[index] = &value
	}
	return cloned
}
