// Code generated from specs/upstream/beacon-api-subset.yaml; DO NOT EDIT.

package ethertest

// BeaconBlockRequest is generated from the locked path and query parameters.
type BeaconBlockRequest struct {
	BlockID string `path:"block_id"`
}

// BeaconDataColumnSidecarsRequest is generated from the locked path and query parameters.
type BeaconDataColumnSidecarsRequest struct {
	BlockID string   `path:"block_id"`
	Indices []string `query:"indices"`
}

// BeaconValidatorsRequest is generated from the locked path and query parameters.
type BeaconValidatorsRequest struct {
	StateID  string   `path:"state_id"`
	IDs      []string `query:"id"`
	Statuses []string `query:"status"`
}

// BeaconValidatorBalancesRequest is generated from the locked path and query parameters.
type BeaconValidatorBalancesRequest struct {
	StateID string   `path:"state_id"`
	IDs     []string `query:"id"`
}

// BeaconEventsRequest is generated from the locked path and query parameters.
type BeaconEventsRequest struct {
	Topics []string `query:"topics"`
}

// BeaconErrorMessage is generated from components.schemas.ErrorMessage.
type BeaconErrorMessage struct {
	Code        int      `json:"code"`
	Message     string   `json:"message"`
	Stacktraces []string `json:"stacktraces,omitempty"`
}

// BeaconDataEnvelope is generated from components.schemas.DataEnvelope.
type BeaconDataEnvelope[T any] struct {
	Data T `json:"data"`
}

// BeaconResponse is generated from components.schemas.Response.
type BeaconResponse[T any] struct {
	ExecutionOptimistic bool `json:"execution_optimistic"`
	Finalized           bool `json:"finalized"`
	Data                T    `json:"data"`
	EthertestTainted    bool `json:"ethertest_tainted,omitempty"`
}

// BeaconVersionedResponse is generated from components.schemas.VersionedResponse.
type BeaconVersionedResponse[T any] struct {
	Version             string `json:"version"`
	ExecutionOptimistic bool   `json:"execution_optimistic"`
	Finalized           bool   `json:"finalized"`
	Data                T      `json:"data"`
	EthertestTainted    bool   `json:"ethertest_tainted,omitempty"`
}

var beaconGeneratedEventTopics = map[string]struct{}{
	"head":                 {},
	"block":                {},
	"chain_reorg":          {},
	"finalized_checkpoint": {},
}

var beaconGeneratedValidatorStatuses = map[string]struct{}{
	"pending_initialized": {},
	"pending_queued":      {},
	"active_ongoing":      {},
	"active_exiting":      {},
	"active_slashed":      {},
	"exited_unslashed":    {},
	"exited_slashed":      {},
	"withdrawal_possible": {},
	"withdrawal_done":     {},
	"pending":             {},
	"active":              {},
	"exited":              {},
	"withdrawal":          {},
}
