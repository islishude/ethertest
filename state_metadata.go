package ethertest

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

const currentMetadataFormat = 1

var (
	stateSchemaKey      = []byte("ethertest/meta/schema-version")
	timelineKey         = []byte("ethertest/meta/timeline")
	sessionSafetyKey    = []byte("ethertest/meta/session-safety")
	journalKey          = []byte("ethertest/meta/prepared-operation")
	canonicalSlotPrefix = []byte("ethertest/canonical-slot/")
	blockSlotPrefix     = []byte("ethertest/block-slot/")
	blockSafetyPrefix   = []byte("ethertest/block-safety/")
)

const (
	taintControlStateOverride      = "control-state-override"
	taintSafetyMetadataUnavailable = "safety-metadata-unavailable"
)

// BlockSafety describes whether a block belongs to Ethereum-replayable history
// or to an unsafe fixture lineage.
type BlockSafety struct {
	BlockHash        common.Hash  `json:"block_hash"`
	Tainted          bool         `json:"tainted"`
	FirstUnsafeBlock *common.Hash `json:"first_unsafe_block,omitempty"`
	Reasons          []string     `json:"reasons,omitempty"`
}

// SafetyStatus describes the safety of both the active head and the database
// session. Session taint is permanent because archives retain non-canonical
// history even after the active head returns to a clean branch.
type SafetyStatus struct {
	SessionTainted   bool         `json:"session_tainted"`
	HeadTainted      bool         `json:"head_tainted"`
	FirstUnsafeBlock *common.Hash `json:"first_unsafe_block,omitempty"`
	Reasons          []string     `json:"reasons,omitempty"`
	TimelineComplete bool         `json:"timeline_complete"`
	ConsensusMode    string       `json:"consensus_mode"`
}

type storedTimeline struct {
	GenesisTime       uint64 `json:"genesis_time"`
	CurrentSlot       uint64 `json:"current_slot"`
	LastProcessedSlot uint64 `json:"last_processed_slot"`
	Complete          bool   `json:"complete"`
	FinalityPaused    bool   `json:"finality_paused,omitempty"`
	FinalitySlot      uint64 `json:"finality_slot,omitempty"`
}

type storedSessionSafety struct {
	Tainted          bool         `json:"tainted"`
	FirstUnsafeBlock *common.Hash `json:"first_unsafe_block,omitempty"`
	Reasons          []string     `json:"reasons,omitempty"`
}

type journalKV struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

type preparedOperation struct {
	Version               int         `json:"version"`
	Kind                  string      `json:"kind"`
	OldHead               common.Hash `json:"old_head,omitempty"`
	NewHead               common.Hash `json:"new_head,omitempty"`
	TargetBlock           common.Hash `json:"target_block,omitempty"`
	TargetNumber          uint64      `json:"target_number,omitempty"`
	DiscardTargetOnCancel bool        `json:"discard_target_on_cancel,omitempty"`
	Puts                  []journalKV `json:"puts,omitempty"`
	Deletes               [][]byte    `json:"deletes,omitempty"`
}

func recoverPreparedOperation(db ethdb.Database, currentHead func() common.Hash, blockExists func(common.Hash) bool) error {
	exists, err := db.Has(journalKey)
	if err != nil {
		return fmt.Errorf("check recovery journal: %w", err)
	}
	if !exists {
		return nil
	}
	encoded, err := db.Get(journalKey)
	if err != nil {
		return fmt.Errorf("read recovery journal: %w", err)
	}
	var operation preparedOperation
	if err := json.Unmarshal(encoded, &operation); err != nil {
		return fmt.Errorf("decode recovery journal: %w", err)
	}
	if operation.Version != currentMetadataFormat {
		return fmt.Errorf("unsupported recovery journal version %d", operation.Version)
	}
	switch operation.Kind {
	case "head":
		head := currentHead()
		switch head {
		case operation.NewHead:
			return finalizePreparedOperation(db, operation)
		case operation.OldHead:
			if operation.DiscardTargetOnCancel {
				discardPreparedTarget(db, operation.NewHead, operation.TargetNumber)
			}
			return db.Delete(journalKey)
		default:
			return fmt.Errorf("recovery journal head mismatch: have %s, old %s, target %s", head, operation.OldHead, operation.NewHead)
		}
	case "block":
		if blockExists(operation.TargetBlock) {
			return finalizePreparedOperation(db, operation)
		}
		return db.Delete(journalKey)
	default:
		return fmt.Errorf("unsupported recovery journal kind %q", operation.Kind)
	}
}

func discardPreparedTarget(db ethdb.Database, hash common.Hash, number uint64) {
	if block := rawdb.ReadBlock(db, hash, number); block != nil {
		for _, transaction := range block.Transactions() {
			indexedNumber := rawdb.ReadTxLookupEntry(db, transaction.Hash())
			if indexedNumber != nil && *indexedNumber == number {
				rawdb.DeleteTxLookupEntry(db, transaction.Hash())
			}
		}
	}
	if rawdb.ReadCanonicalHash(db, number) == hash {
		rawdb.DeleteCanonicalHash(db, number)
	}
	rawdb.DeleteBlock(db, hash, number)
}

func writePreparedOperation(db ethdb.Database, operation preparedOperation) error {
	operation.Version = currentMetadataFormat
	encoded, err := json.Marshal(operation)
	if err != nil {
		return err
	}
	return db.Put(journalKey, encoded)
}

func finalizePreparedOperation(db ethdb.Database, operation preparedOperation) error {
	batch := db.NewBatch()
	for _, item := range operation.Puts {
		if err := batch.Put(item.Key, item.Value); err != nil {
			return err
		}
	}
	for _, key := range operation.Deletes {
		if err := batch.Delete(key); err != nil {
			return err
		}
	}
	if err := batch.Delete(journalKey); err != nil {
		return err
	}
	return batch.Write()
}

func initializeRuntimeMetadata(chain *executionChain, existingData bool) error {
	if err := recoverPreparedOperation(
		chain.db,
		func() common.Hash { return chain.blockchain.CurrentBlock().Hash() },
		func(hash common.Hash) bool { return chain.blockchain.GetBlockByHash(hash) != nil },
	); err != nil {
		return err
	}
	exists, err := chain.db.Has(stateSchemaKey)
	if err != nil {
		return err
	}
	if exists {
		version, err := chain.db.Get(stateSchemaKey)
		if err != nil {
			return err
		}
		if len(version) != 8 || binary.BigEndian.Uint64(version) != currentMetadataFormat {
			return errors.New("unsupported ethertest state metadata schema")
		}
		return loadRuntimeMetadata(chain)
	}
	if existingData {
		return errors.New("existing ethertest data does not contain the current in-place metadata format; remove it and create a fresh chain")
	}
	return initializeFreshRuntimeMetadata(chain)
}

func readPersistedGenesisTime(db ethdb.Database) (uint64, error) {
	exists, err := db.Has(stateSchemaKey)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, errors.New("existing ethertest data does not contain the current in-place metadata format; remove it and create a fresh chain")
	}
	version, err := db.Get(stateSchemaKey)
	if err != nil {
		return 0, err
	}
	if len(version) != 8 || binary.BigEndian.Uint64(version) != currentMetadataFormat {
		return 0, errors.New("unsupported ethertest state metadata schema")
	}
	encoded, err := db.Get(timelineKey)
	if err != nil {
		return 0, fmt.Errorf("read timeline metadata: %w", err)
	}
	var timeline storedTimeline
	if err := json.Unmarshal(encoded, &timeline); err != nil {
		return 0, fmt.Errorf("decode timeline metadata: %w", err)
	}
	if timeline.GenesisTime > uint64(1<<63-1) {
		return 0, errors.New("stored genesis time is out of range")
	}
	return timeline.GenesisTime, nil
}

func initializeFreshRuntimeMetadata(chain *executionChain) error {
	genesis := chain.blockchain.Genesis()
	chain.timelineComplete = true
	chain.lastProcessedSlot = chain.slot
	chain.blockSafety = make(map[common.Hash]BlockSafety)
	chain.blockSafety[genesis.Hash()] = BlockSafety{BlockHash: genesis.Hash()}
	chain.sessionTainted = false
	chain.firstUnsafeBlock = nil
	chain.taintReasons = make(map[string]struct{})

	batch := chain.db.NewBatch()
	var version [8]byte
	binary.BigEndian.PutUint64(version[:], currentMetadataFormat)
	if err := batch.Put(stateSchemaKey, version[:]); err != nil {
		return err
	}
	if err := putTimeline(batch, chain.timeline()); err != nil {
		return err
	}
	if err := putSessionSafety(batch, chain.sessionSafety()); err != nil {
		return err
	}
	for slot, hash := range chain.canonicalBlockBySlot {
		if err := batch.Put(slotKey(canonicalSlotPrefix, slot), hash.Bytes()); err != nil {
			return err
		}
	}
	for hash, slot := range chain.slotByHash {
		if err := putBlockSlot(batch, hash, slot); err != nil {
			return err
		}
	}
	for hash, safety := range chain.blockSafety {
		if err := putBlockSafety(batch, hash, safety); err != nil {
			return err
		}
	}
	return batch.Write()
}

func loadRuntimeMetadata(chain *executionChain) error {
	encoded, err := chain.db.Get(timelineKey)
	if err != nil {
		return fmt.Errorf("read timeline metadata: %w", err)
	}
	var timeline storedTimeline
	if err := json.Unmarshal(encoded, &timeline); err != nil {
		return fmt.Errorf("decode timeline metadata: %w", err)
	}
	if timeline.GenesisTime != chain.genesisTime {
		return fmt.Errorf("stored genesis time %d does not match configured genesis time %d", timeline.GenesisTime, chain.genesisTime)
	}
	chain.slot = timeline.CurrentSlot
	chain.lastProcessedSlot = timeline.LastProcessedSlot
	chain.timelineComplete = timeline.Complete
	chain.finalityPaused = timeline.FinalityPaused
	chain.finalitySlot = timeline.FinalitySlot

	chain.canonicalBlockBySlot = make(map[uint64]common.Hash)
	iterator := chain.db.NewIterator(canonicalSlotPrefix, nil)
	for iterator.Next() {
		if len(iterator.Key()) != len(canonicalSlotPrefix)+8 || len(iterator.Value()) != common.HashLength {
			iterator.Release()
			return errors.New("invalid canonical slot metadata")
		}
		slot := binary.BigEndian.Uint64(iterator.Key()[len(canonicalSlotPrefix):])
		chain.canonicalBlockBySlot[slot] = common.BytesToHash(iterator.Value())
	}
	if err := iterator.Error(); err != nil {
		iterator.Release()
		return err
	}
	iterator.Release()

	chain.slotByHash = make(map[common.Hash]uint64)
	iterator = chain.db.NewIterator(blockSlotPrefix, nil)
	for iterator.Next() {
		if len(iterator.Key()) != len(blockSlotPrefix)+common.HashLength || len(iterator.Value()) != 8 {
			iterator.Release()
			return errors.New("invalid block slot metadata")
		}
		hash := common.BytesToHash(iterator.Key()[len(blockSlotPrefix):])
		chain.slotByHash[hash] = binary.BigEndian.Uint64(iterator.Value())
	}
	if err := iterator.Error(); err != nil {
		iterator.Release()
		return err
	}
	iterator.Release()

	chain.blockSafety = make(map[common.Hash]BlockSafety)
	iterator = chain.db.NewIterator(blockSafetyPrefix, nil)
	for iterator.Next() {
		if len(iterator.Key()) != len(blockSafetyPrefix)+common.HashLength {
			iterator.Release()
			return errors.New("invalid block safety metadata")
		}
		var safety BlockSafety
		if err := json.Unmarshal(iterator.Value(), &safety); err != nil {
			iterator.Release()
			return fmt.Errorf("decode block safety metadata: %w", err)
		}
		hash := common.BytesToHash(iterator.Key()[len(blockSafetyPrefix):])
		safety.BlockHash = hash
		chain.blockSafety[hash] = safety
	}
	if err := iterator.Error(); err != nil {
		iterator.Release()
		return err
	}
	iterator.Release()

	encoded, err = chain.db.Get(sessionSafetyKey)
	if err != nil {
		return fmt.Errorf("read session safety metadata: %w", err)
	}
	var safety storedSessionSafety
	if err := json.Unmarshal(encoded, &safety); err != nil {
		return fmt.Errorf("decode session safety metadata: %w", err)
	}
	chain.sessionTainted = safety.Tainted
	chain.firstUnsafeBlock = cloneHashPointer(safety.FirstUnsafeBlock)
	chain.taintReasons = make(map[string]struct{}, len(safety.Reasons))
	for _, reason := range safety.Reasons {
		chain.taintReasons[reason] = struct{}{}
	}
	return nil
}

func validateRuntimeMetadata(chain *executionChain) error {
	chain.mu.RLock()
	defer chain.mu.RUnlock()
	if chain.lastProcessedSlot != chain.slot {
		return fmt.Errorf("timeline last processed slot %d does not match current slot %d", chain.lastProcessedSlot, chain.slot)
	}
	if chain.finalityPaused {
		if chain.finalitySlot > chain.slot {
			return fmt.Errorf("paused finality slot %d exceeds current slot %d", chain.finalitySlot, chain.slot)
		}
	} else if chain.finalitySlot != 0 {
		return fmt.Errorf("active finality has stale frozen slot %d", chain.finalitySlot)
	}
	head := chain.blockchain.CurrentBlock()
	headSlot, exists := chain.slotByHash[head.Hash()]
	if !exists || headSlot > chain.slot {
		return fmt.Errorf("canonical head %s has invalid slot metadata", head.Hash())
	}
	for hash, slot := range chain.slotByHash {
		block := chain.blockchain.GetBlockByHash(hash)
		if block == nil {
			return fmt.Errorf("slot metadata references missing execution block %s", hash)
		}
		_, projection, projected, err := loadProjection(chain, hash)
		if err != nil {
			return err
		}
		if !projected || projection.Slot != slot {
			return fmt.Errorf("execution block %s has missing or inconsistent Beacon projection", hash)
		}
		safety, safe := chain.blockSafety[hash]
		if !safe || safety.BlockHash != hash {
			return fmt.Errorf("execution block %s has missing safety metadata", hash)
		}
		if safety.Tainted && safety.FirstUnsafeBlock == nil {
			return fmt.Errorf("tainted block %s has no first unsafe ancestor", hash)
		}
		for _, reason := range safety.Reasons {
			if _, recorded := chain.taintReasons[reason]; safety.Tainted && !recorded {
				return fmt.Errorf("block %s taint reason %q is missing from session safety", hash, reason)
			}
		}
	}
	for slot, hash := range chain.canonicalBlockBySlot {
		block := chain.blockchain.GetBlockByHash(hash)
		if block == nil || chain.blockchain.GetCanonicalHash(block.NumberU64()) != hash || chain.slotByHash[hash] != slot {
			return fmt.Errorf("canonical slot %d references an invalid block %s", slot, hash)
		}
	}
	for number := uint64(0); number <= head.Number.Uint64(); number++ {
		block := chain.blockchain.GetBlockByNumber(number)
		if block == nil {
			return fmt.Errorf("canonical execution block %d is missing", number)
		}
		slot, slotted := chain.slotByHash[block.Hash()]
		if !slotted || chain.canonicalBlockBySlot[slot] != block.Hash() {
			return fmt.Errorf("canonical execution block %s is absent from the canonical slot index", block.Hash())
		}
	}
	if chain.sessionTainted {
		if chain.firstUnsafeBlock == nil {
			return errors.New("tainted session has no first unsafe block")
		}
		if safety, exists := chain.blockSafety[*chain.firstUnsafeBlock]; !exists || !safety.Tainted {
			return errors.New("session first unsafe block is missing or clean")
		}
	} else {
		for hash, safety := range chain.blockSafety {
			if safety.Tainted {
				return fmt.Errorf("clean session contains tainted block %s", hash)
			}
		}
	}
	return nil
}

func (c *executionChain) timeline() storedTimeline {
	return storedTimeline{
		GenesisTime: c.genesisTime, CurrentSlot: c.slot,
		LastProcessedSlot: c.lastProcessedSlot, Complete: c.timelineComplete,
		FinalityPaused: c.finalityPaused, FinalitySlot: c.finalitySlot,
	}
}

func (c *executionChain) sessionSafety() storedSessionSafety {
	return storedSessionSafety{
		Tainted: c.sessionTainted, FirstUnsafeBlock: cloneHashPointer(c.firstUnsafeBlock),
		Reasons: sortedReasonSet(c.taintReasons),
	}
}

func putTimeline(batch ethdb.Batch, timeline storedTimeline) error {
	encoded, err := json.Marshal(timeline)
	if err != nil {
		return err
	}
	return batch.Put(timelineKey, encoded)
}

func putSessionSafety(batch ethdb.Batch, safety storedSessionSafety) error {
	encoded, err := json.Marshal(safety)
	if err != nil {
		return err
	}
	return batch.Put(sessionSafetyKey, encoded)
}

func putBlockSlot(batch ethdb.Batch, hash common.Hash, slot uint64) error {
	var value [8]byte
	binary.BigEndian.PutUint64(value[:], slot)
	return batch.Put(hashKey(blockSlotPrefix, hash), value[:])
}

func putBlockSafety(batch ethdb.Batch, hash common.Hash, safety BlockSafety) error {
	encoded, err := json.Marshal(safety)
	if err != nil {
		return err
	}
	return batch.Put(hashKey(blockSafetyPrefix, hash), encoded)
}

func slotKey(prefix []byte, slot uint64) []byte {
	key := make([]byte, len(prefix)+8)
	copy(key, prefix)
	binary.BigEndian.PutUint64(key[len(prefix):], slot)
	return key
}

func hashKey(prefix []byte, hash common.Hash) []byte {
	key := make([]byte, len(prefix)+common.HashLength)
	copy(key, prefix)
	copy(key[len(prefix):], hash[:])
	return key
}

func blockSafetyForChild(parent BlockSafety, hash common.Hash, unsafeReason string) BlockSafety {
	safety := BlockSafety{BlockHash: hash}
	if !parent.Tainted && unsafeReason == "" {
		return safety
	}
	safety.Tainted = true
	safety.FirstUnsafeBlock = cloneHashPointer(parent.FirstUnsafeBlock)
	if safety.FirstUnsafeBlock == nil {
		value := hash
		safety.FirstUnsafeBlock = &value
	}
	safety.Reasons = append([]string(nil), parent.Reasons...)
	if unsafeReason != "" && !slices.Contains(safety.Reasons, unsafeReason) {
		safety.Reasons = append(safety.Reasons, unsafeReason)
		slices.Sort(safety.Reasons)
	}
	return safety
}

func cloneHashPointer(value *common.Hash) *common.Hash {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func sortedReasonSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func (n *Node) SafetyStatus() SafetyStatus {
	head := n.chain.blockchain.CurrentBlock().Hash()
	n.chain.mu.RLock()
	headSafety, exists := n.chain.blockSafety[head]
	sessionTainted := n.chain.sessionTainted
	firstUnsafeBlock := cloneHashPointer(n.chain.firstUnsafeBlock)
	reasons := sortedReasonSet(n.chain.taintReasons)
	timelineComplete := n.chain.timelineComplete
	n.chain.mu.RUnlock()
	if !exists {
		sessionTainted = true
		headSafety.Tainted = true
		firstUnsafeBlock = cloneHashPointer(&head)
		if !slices.Contains(reasons, taintSafetyMetadataUnavailable) {
			reasons = append(reasons, taintSafetyMetadataUnavailable)
			slices.Sort(reasons)
		}
	}
	return SafetyStatus{
		SessionTainted: sessionTainted, HeadTainted: headSafety.Tainted,
		FirstUnsafeBlock: firstUnsafeBlock,
		Reasons:          reasons,
		TimelineComplete: timelineComplete, ConsensusMode: "synthetic",
	}
}

func (n *Node) BlockSafety(hash common.Hash) (BlockSafety, error) {
	n.chain.mu.RLock()
	safety, exists := n.chain.blockSafety[hash]
	n.chain.mu.RUnlock()
	if !exists {
		if n.chain.blockchain.GetBlockByHash(hash) != nil {
			return BlockSafety{
				BlockHash: hash, Tainted: true, FirstUnsafeBlock: cloneHashPointer(&hash),
				Reasons: []string{taintSafetyMetadataUnavailable},
			}, nil
		}
		return BlockSafety{}, errors.New("block safety metadata not found")
	}
	safety.FirstUnsafeBlock = cloneHashPointer(safety.FirstUnsafeBlock)
	safety.Reasons = append([]string(nil), safety.Reasons...)
	return safety, nil
}
