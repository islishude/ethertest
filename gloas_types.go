package ethertest

// The Gloas projection types below are the consumed subset of consensus-specs
// v1.7.0-beta.2. ssz-index selects EIP-7688 progressive containers; progressive
// lists deliberately do not use Electra's binary list merkleization.
import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/deneb"
	"github.com/attestantio/go-eth2-client/spec/electra"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/holiman/uint256"
	dynssz "github.com/pk910/dynamic-ssz"
)

var reflectGloasBloom = reflect.TypeFor[gloasLogsBloom]()

var gloasSSZ = dynssz.NewDynSsz(nil)

type executionRequests struct {
	Deposits        []*electra.DepositRequest       `json:"deposits" ssz-index:"0" ssz-type:"progressive-list"`
	Withdrawals     []*electra.WithdrawalRequest    `json:"withdrawals" ssz-index:"1" ssz-type:"progressive-list"`
	Consolidations  []*electra.ConsolidationRequest `json:"consolidations" ssz-index:"2" ssz-type:"progressive-list"`
	BuilderDeposits []*gloasBuilderDepositRequest   `json:"builder_deposits" ssz-index:"3" ssz-type:"progressive-list"`
	BuilderExits    []*gloasBuilderExitRequest      `json:"builder_exits" ssz-index:"4" ssz-type:"progressive-list"`
}

func (r *executionRequests) electra() *electra.ExecutionRequests {
	return &electra.ExecutionRequests{Deposits: r.Deposits, Withdrawals: r.Withdrawals, Consolidations: r.Consolidations}
}
func requestsFromElectra(r *electra.ExecutionRequests) *executionRequests {
	if r == nil {
		return nil
	}
	return &executionRequests{Deposits: r.Deposits, Withdrawals: r.Withdrawals, Consolidations: r.Consolidations, BuilderDeposits: []*gloasBuilderDepositRequest{}, BuilderExits: []*gloasBuilderExitRequest{}}
}

type gloasBuilderDepositRequest struct {
	Pubkey                phase0.BLSPubKey    `json:"pubkey"`
	WithdrawalCredentials phase0.Root         `json:"withdrawal_credentials"`
	Amount                phase0.Gwei         `json:"amount,string"`
	Signature             phase0.BLSSignature `json:"signature"`
}
type gloasBuilderExitRequest struct {
	SourceAddress bellatrix.ExecutionAddress `json:"source_address"`
	Pubkey        phase0.BLSPubKey           `json:"pubkey"`
}

type gloasLogsBloom [256]byte

func (v gloasLogsBloom) MarshalJSON() ([]byte, error) { return json.Marshal(hexutil.Encode(v[:])) }
func (v *gloasLogsBloom) UnmarshalJSON(b []byte) error {
	return hexutil.UnmarshalFixedJSON(reflectGloasBloom, b, v[:])
}

type gloasExecutionPayload struct {
	ParentHash      phase0.Hash32              `json:"parent_hash" ssz-index:"0"`
	FeeRecipient    bellatrix.ExecutionAddress `json:"fee_recipient" ssz-index:"1"`
	StateRoot       phase0.Root                `json:"state_root" ssz-index:"2"`
	ReceiptsRoot    phase0.Root                `json:"receipts_root" ssz-index:"3"`
	LogsBloom       gloasLogsBloom             `json:"logs_bloom" ssz-index:"4"`
	PrevRandao      phase0.Root                `json:"prev_randao" ssz-index:"5"`
	BlockNumber     uint64                     `json:"block_number,string" ssz-index:"6"`
	GasLimit        uint64                     `json:"gas_limit,string" ssz-index:"7"`
	GasUsed         uint64                     `json:"gas_used,string" ssz-index:"8"`
	Timestamp       uint64                     `json:"timestamp,string" ssz-index:"9"`
	ExtraData       hexutil.Bytes              `json:"extra_data" ssz-index:"10" ssz-max:"32"`
	BaseFeePerGas   *uint256.Int               `json:"base_fee_per_gas" ssz-index:"11" ssz-type:"uint256"`
	BlockHash       phase0.Hash32              `json:"block_hash" ssz-index:"12"`
	Transactions    []hexutil.Bytes            `json:"transactions" ssz-index:"13" ssz-type:"progressive-list,progressive-list"`
	Withdrawals     []*capella.Withdrawal      `json:"withdrawals" ssz-index:"14" ssz-type:"progressive-list"`
	BlobGasUsed     uint64                     `json:"blob_gas_used,string" ssz-index:"15"`
	ExcessBlobGas   uint64                     `json:"excess_blob_gas,string" ssz-index:"16"`
	BlockAccessList hexutil.Bytes              `json:"block_access_list" ssz-index:"17" ssz-type:"progressive-list"`
	SlotNumber      uint64                     `json:"slot_number,string" ssz-index:"18"`
}
type gloasPayloadBid struct {
	ParentBlockHash       phase0.Hash32              `json:"parent_block_hash" ssz-index:"0"`
	ParentBlockRoot       phase0.Root                `json:"parent_block_root" ssz-index:"1"`
	BlockHash             phase0.Hash32              `json:"block_hash" ssz-index:"2"`
	PrevRandao            phase0.Root                `json:"prev_randao" ssz-index:"3"`
	FeeRecipient          bellatrix.ExecutionAddress `json:"fee_recipient" ssz-index:"4"`
	GasLimit              uint64                     `json:"gas_limit,string" ssz-index:"5"`
	BuilderIndex          uint64                     `json:"builder_index,string" ssz-index:"6"`
	Slot                  phase0.Slot                `json:"slot,string" ssz-index:"7"`
	Value                 phase0.Gwei                `json:"value,string" ssz-index:"8"`
	ExecutionPayment      phase0.Gwei                `json:"execution_payment,string" ssz-index:"9"`
	BlobKZGCommitments    []deneb.KZGCommitment      `json:"blob_kzg_commitments" ssz-index:"10" ssz-type:"progressive-list"`
	ExecutionRequestsRoot phase0.Root                `json:"execution_requests_root" ssz-index:"11"`
}
type gloasSignedPayloadBid struct {
	Message   *gloasPayloadBid    `json:"message"`
	Signature phase0.BLSSignature `json:"signature"`
}
type gloasPayloadEnvelope struct {
	Payload               *gloasExecutionPayload `json:"payload" ssz-index:"0"`
	ExecutionRequests     *executionRequests     `json:"execution_requests" ssz-index:"1"`
	BuilderIndex          uint64                 `json:"builder_index,string" ssz-index:"2"`
	BeaconBlockRoot       phase0.Root            `json:"beacon_block_root" ssz-index:"3"`
	ParentBeaconBlockRoot phase0.Root            `json:"parent_beacon_block_root" ssz-index:"4"`
}
type gloasSignedPayloadEnvelope struct {
	Message   *gloasPayloadEnvelope `json:"message"`
	Signature phase0.BLSSignature   `json:"signature"`
}
type gloasIndexedAttestation struct {
	AttestingIndices []gloasValidatorIndex   `json:"attesting_indices" ssz-index:"0" ssz-type:"progressive-list"`
	Data             *phase0.AttestationData `json:"data" ssz-index:"1"`
	Signature        phase0.BLSSignature     `json:"signature" ssz-index:"2"`
}
type gloasAttesterSlashing struct {
	Attestation1 *gloasIndexedAttestation `json:"attestation_1"`
	Attestation2 *gloasIndexedAttestation `json:"attestation_2"`
}
type gloasAttestation struct {
	AggregationBits hexutil.Bytes           `json:"aggregation_bits" ssz-index:"0" ssz-type:"progressive-bitlist"`
	Data            *phase0.AttestationData `json:"data" ssz-index:"1"`
	Signature       phase0.BLSSignature     `json:"signature" ssz-index:"2"`
	CommitteeBits   hexutil.Bytes           `json:"committee_bits" ssz-index:"3" ssz-size:"8"`
}
type gloasPayloadAttestationData struct {
	BeaconBlockRoot   phase0.Root `json:"beacon_block_root"`
	Slot              phase0.Slot `json:"slot,string"`
	PayloadPresent    bool        `json:"payload_present"`
	BlobDataAvailable bool        `json:"blob_data_available"`
}
type gloasPayloadAttestation struct {
	AggregationBits hexutil.Bytes                `json:"aggregation_bits" ssz-index:"0" ssz-type:"bitvector" ssz-size:"2"`
	Data            *gloasPayloadAttestationData `json:"data" ssz-index:"1"`
	Signature       phase0.BLSSignature          `json:"signature" ssz-index:"2"`
}
type gloasBeaconBody struct {
	RANDAOReveal              phase0.BLSSignature                   `json:"randao_reveal" ssz-index:"0"`
	ETH1Data                  *phase0.ETH1Data                      `json:"eth1_data" ssz-index:"1"`
	Graffiti                  phase0.Root                           `json:"graffiti" ssz-index:"2"`
	ProposerSlashings         []*phase0.ProposerSlashing            `json:"proposer_slashings" ssz-index:"3" ssz-type:"progressive-list"`
	AttesterSlashings         []*gloasAttesterSlashing              `json:"attester_slashings" ssz-index:"4" ssz-type:"progressive-list"`
	Attestations              []*gloasAttestation                   `json:"attestations" ssz-index:"5" ssz-type:"progressive-list"`
	Deposits                  []*phase0.Deposit                     `json:"deposits" ssz-index:"6" ssz-type:"progressive-list"`
	VoluntaryExits            []*phase0.SignedVoluntaryExit         `json:"voluntary_exits" ssz-index:"7" ssz-type:"progressive-list"`
	SyncAggregate             *altair.SyncAggregate                 `json:"sync_aggregate" ssz-index:"8"`
	BLSToExecutionChanges     []*capella.SignedBLSToExecutionChange `json:"bls_to_execution_changes" ssz-index:"9" ssz-type:"progressive-list"`
	SignedExecutionPayloadBid *gloasSignedPayloadBid                `json:"signed_execution_payload_bid" ssz-index:"10"`
	PayloadAttestations       []*gloasPayloadAttestation            `json:"payload_attestations" ssz-index:"11" ssz-type:"progressive-list"`
	ParentExecutionRequests   *executionRequests                    `json:"parent_execution_requests" ssz-index:"12"`
}
type gloasBeaconBlock struct {
	Slot          phase0.Slot           `json:"slot,string"`
	ProposerIndex phase0.ValidatorIndex `json:"proposer_index,string"`
	ParentRoot    phase0.Root           `json:"parent_root"`
	StateRoot     phase0.Root           `json:"state_root"`
	Body          *gloasBeaconBody      `json:"body"`
}
type gloasSignedBeaconBlock struct {
	Message   *gloasBeaconBlock   `json:"message"`
	Signature phase0.BLSSignature `json:"signature"`
}
type gloasDataColumn struct {
	Index           uint64           `json:"index,string"`
	Column          [][2048]byte     `json:"column" ssz-type:"progressive-list"`
	KZGProofs       []deneb.KZGProof `json:"kzg_proofs" ssz-type:"progressive-list"`
	Slot            phase0.Slot      `json:"slot,string"`
	BeaconBlockRoot phase0.Root      `json:"beacon_block_root"`
}

type gloasColumnJSON struct {
	Index           uint64           `json:"index,string"`
	Column          []hexutil.Bytes  `json:"column"`
	KZGProofs       []deneb.KZGProof `json:"kzg_proofs"`
	Slot            phase0.Slot      `json:"slot,string"`
	BeaconBlockRoot phase0.Root      `json:"beacon_block_root"`
}

func (v gloasDataColumn) MarshalJSON() ([]byte, error) {
	columns := make([]hexutil.Bytes, len(v.Column))
	for i := range v.Column {
		columns[i] = v.Column[i][:]
	}
	return json.Marshal(gloasColumnJSON{v.Index, columns, v.KZGProofs, v.Slot, v.BeaconBlockRoot})
}
func (v *gloasDataColumn) UnmarshalJSON(data []byte) error {
	var value gloasColumnJSON
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	columns := make([][2048]byte, len(value.Column))
	for i, cell := range value.Column {
		if len(cell) != 2048 {
			return fmt.Errorf("data column cell has %d bytes, want 2048", len(cell))
		}
		copy(columns[i][:], cell)
	}
	*v = gloasDataColumn{value.Index, columns, value.KZGProofs, value.Slot, value.BeaconBlockRoot}
	return nil
}

// Consensus JSON encodes all validator indices as quoted decimal integers,
// including indices nested inside progressive lists.
type gloasValidatorIndex uint64

func (v gloasValidatorIndex) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(v), 10))
}
func (v *gloasValidatorIndex) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	index, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return err
	}
	*v = gloasValidatorIndex(index)
	return nil
}
