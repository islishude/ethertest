package ethertest

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

var blobNamespace = []byte("ethertest/blob/")

type executionChain struct {
	mu                   sync.RWMutex
	config               *params.ChainConfig
	db                   ethdb.Database
	blockchain           *core.BlockChain
	feeRecipient         common.Address
	slot                 uint64
	genesisTime          uint64
	slotDuration         uint64
	slotByHash           map[common.Hash]uint64
	canonicalBlockBySlot map[uint64]common.Hash
	lastProcessedSlot    uint64
	timelineComplete     bool
	finalityPaused       bool
	finalitySlot         uint64
	blockSafety          map[common.Hash]BlockSafety
	sessionTainted       bool
	firstUnsafeBlock     *common.Hash
	taintReasons         map[string]struct{}
	pending              map[common.Address]map[uint64]*types.Transaction
	arrival              map[common.Hash]uint64
	nextArrival          uint64
	blobs                map[common.Hash]*types.BlobTxSidecar
	order                string
	pendingView          *pendingView
}

type pendingView struct {
	block      *types.Block
	state      *state.StateDB
	receipts   types.Receipts
	executable map[common.Hash]struct{}
	queued     map[common.Hash]struct{}
}

// executionChainConfig pins ethertest's supported protocol surface. Do not
// derive it from geth's rolling development configs: dependency upgrades must
// not activate a new fork or change the blob schedule implicitly.
func executionChainConfig(cfg Config) *params.ChainConfig {
	activationTime := func(epoch uint64) *uint64 {
		value := uint64(cfg.Chain.GenesisTime) + epoch*cfg.Chain.SlotsPerEpoch*uint64(cfg.Chain.SlotDuration/time.Second)
		return &value
	}
	zeroBlock := func() *big.Int { return new(big.Int) }
	zeroTime := uint64(0)
	return &params.ChainConfig{
		ChainID:                 new(big.Int).SetUint64(cfg.Chain.ChainID),
		HomesteadBlock:          zeroBlock(),
		EIP150Block:             zeroBlock(),
		EIP155Block:             zeroBlock(),
		EIP158Block:             zeroBlock(),
		ByzantiumBlock:          zeroBlock(),
		ConstantinopleBlock:     zeroBlock(),
		PetersburgBlock:         zeroBlock(),
		IstanbulBlock:           zeroBlock(),
		MuirGlacierBlock:        zeroBlock(),
		BerlinBlock:             zeroBlock(),
		LondonBlock:             zeroBlock(),
		ArrowGlacierBlock:       zeroBlock(),
		GrayGlacierBlock:        zeroBlock(),
		ShanghaiTime:            &zeroTime,
		CancunTime:              activationTime(cfg.Chain.Forks.CancunEpoch),
		PragueTime:              activationTime(cfg.Chain.Forks.PragueEpoch),
		OsakaTime:               activationTime(cfg.Chain.Forks.OsakaEpoch),
		TerminalTotalDifficulty: zeroBlock(),
		BlobScheduleConfig: &params.BlobScheduleConfig{
			Cancun: &params.BlobConfig{Target: 3, Max: 6, UpdateFraction: 3_338_477},
			Prague: &params.BlobConfig{Target: 6, Max: 9, UpdateFraction: 5_007_716},
		},
	}
}

func newExecutionChain(cfg *Config, accounts []common.Address) (*executionChain, error) {
	var database ethdb.Database
	switch cfg.Storage.Engine {
	case "memory":
		database = rawdb.NewMemoryDatabase()
	case "pebble":
		kv, err := pebble.New(cfg.Storage.Path, 64, 64, "ethertest", false)
		if err != nil {
			return nil, err
		}
		database = rawdb.NewDatabase(kv)
	default:
		return nil, fmt.Errorf("unsupported storage engine %q", cfg.Storage.Engine)
	}
	existingData := false
	iterator := database.NewIterator(nil, nil)
	if iterator.Next() {
		existingData = true
	}
	if err := iterator.Error(); err != nil {
		iterator.Release()
		_ = database.Close() //nolint:errcheck
		return nil, err
	}
	iterator.Release()
	if existingData {
		storedGenesisTime, err := readPersistedGenesisTime(database)
		if err != nil {
			_ = database.Close() //nolint:errcheck
			return nil, err
		}
		if cfg.Chain.GenesisTime == 0 {
			cfg.Chain.GenesisTime = int64(storedGenesisTime)
		} else if uint64(cfg.Chain.GenesisTime) != storedGenesisTime {
			_ = database.Close() //nolint:errcheck
			return nil, fmt.Errorf("configured genesis time %d does not match stored genesis time %d", cfg.Chain.GenesisTime, storedGenesisTime)
		}
	} else if cfg.Chain.GenesisTime == 0 {
		cfg.Chain.GenesisTime = time.Now().UTC().Unix()
	}
	chainConfig := executionChainConfig(*cfg)
	genesis := core.DeveloperGenesisBlock(cfg.Chain.GasLimit, nil)
	genesis.Config = chainConfig
	genesis.Timestamp = uint64(cfg.Chain.GenesisTime)
	balance, err := parseBalance(cfg.Accounts.Balance)
	if err != nil {
		_ = database.Close() //nolint:errcheck
		return nil, err
	}
	for _, address := range accounts {
		genesis.Alloc[address] = types.Account{Balance: new(big.Int).Set(balance)}
	}
	engine := beacon.New(ethash.NewFaker())
	bcCfg := core.DefaultConfig()
	bcCfg.ArchiveMode = cfg.Storage.Archive
	bcCfg.TxLookupLimit = 0
	blockchain, err := core.NewBlockChain(database, genesis, engine, bcCfg)
	if err != nil {
		_ = database.Close() //nolint:errcheck
		return nil, err
	}
	feeRecipient := accounts[0]
	if cfg.Mining.FeeRecipient != "" {
		if !common.IsHexAddress(cfg.Mining.FeeRecipient) {
			blockchain.Stop()
			_ = database.Close() //nolint:errcheck
			return nil, errors.New("invalid mining.fee_recipient")
		}
		feeRecipient = common.HexToAddress(cfg.Mining.FeeRecipient)
	}
	slotByHash := make(map[common.Hash]uint64)
	canonicalBlockBySlot := make(map[uint64]common.Hash)
	for number := uint64(0); number <= blockchain.CurrentBlock().Number.Uint64(); number++ {
		block := blockchain.GetBlockByNumber(number)
		if block == nil {
			continue
		}
		slot := uint64(0)
		if block.Time() > uint64(cfg.Chain.GenesisTime) {
			slot = (block.Time() - uint64(cfg.Chain.GenesisTime)) / uint64(cfg.Chain.SlotDuration/time.Second)
		}
		slotByHash[block.Hash()] = slot
		canonicalBlockBySlot[slot] = block.Hash()
	}
	currentSlot := slotByHash[blockchain.CurrentBlock().Hash()]
	chain := &executionChain{
		config: chainConfig, db: database, blockchain: blockchain,
		feeRecipient: feeRecipient,
		pending:      make(map[common.Address]map[uint64]*types.Transaction),
		arrival:      make(map[common.Hash]uint64),
		blobs:        make(map[common.Hash]*types.BlobTxSidecar), order: cfg.Mining.Order,
		genesisTime: uint64(cfg.Chain.GenesisTime), slotDuration: uint64(cfg.Chain.SlotDuration / time.Second),
		slot: currentSlot, slotByHash: slotByHash, canonicalBlockBySlot: canonicalBlockBySlot,
		lastProcessedSlot: currentSlot, timelineComplete: true,
		blockSafety: make(map[common.Hash]BlockSafety), taintReasons: make(map[string]struct{}),
	}
	if err := initializeRuntimeMetadata(chain, existingData); err != nil {
		chain.blockchain.Stop()
		_ = database.Close() //nolint:errcheck
		return nil, err
	}
	return chain, nil
}

func parseBalance(value string) (*big.Int, error) {
	const suffix = "ether"
	if len(value) <= len(suffix) || value[len(value)-len(suffix):] != suffix {
		return nil, fmt.Errorf("balance must use the ether suffix")
	}
	n, ok := new(big.Int).SetString(value[:len(value)-len(suffix)], 10)
	if !ok || n.Sign() < 0 {
		return nil, fmt.Errorf("invalid account balance %q", value)
	}
	return n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)), nil
}

func (c *executionChain) close() error {
	c.blockchain.Stop()
	return c.db.Close()
}

func (c *executionChain) addTransaction(tx *types.Transaction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	head := c.blockchain.CurrentBlock()
	validationHeader := head
	if c.pendingView != nil && c.pendingView.block != nil {
		validationHeader = c.pendingView.block.Header()
	}
	signer := types.MakeSigner(c.config, validationHeader.Number, validationHeader.Time)
	validationTx := tx
	if sidecar := tx.BlobTxSidecar(); sidecar != nil {
		expectedVersion := types.BlobSidecarVersion0
		if c.config.IsOsaka(validationHeader.Number, validationHeader.Time) {
			expectedVersion = types.BlobSidecarVersion1
		}
		if sidecar.Version != expectedVersion {
			return fmt.Errorf("%w: unexpected sidecar version, want: %d, got: %d", txpool.ErrSidecarFormatError, expectedVersion, sidecar.Version)
		}
		// geth's public txpool validator consumes the cell-proof form even on
		// pre-Osaka heads. Validate a converted copy, then verify and retain the
		// original fork-appropriate sidecar below.
		if sidecar.Version == types.BlobSidecarVersion0 {
			converted := sidecar.Copy()
			if err := converted.ToV1(); err != nil {
				return fmt.Errorf("%w: convert blob sidecar: %v", txpool.ErrKZGVerificationError, err)
			}
			validationTx = tx.WithBlobTxSidecar(converted)
		}
	}
	opts := &txpool.ValidationOptions{
		Config: c.config, Accept: 0xff, MaxSize: 4 << 20,
		MaxBlobCount: params.BlobTxMaxBlobs, MinTip: new(big.Int),
	}
	if err := txpool.ValidateTransaction(validationTx, validationHeader, signer, opts); err != nil {
		return err
	}
	from, _ := types.Sender(signer, tx)
	state, err := c.blockchain.StateAt(head)
	if err != nil {
		return err
	}
	byNonce := c.pending[from]
	if previous := byNonce[tx.Nonce()]; previous != nil {
		feeBump := new(big.Int).Mul(previous.GasFeeCap(), big.NewInt(110))
		feeBump.Div(feeBump, big.NewInt(100))
		tipBump := new(big.Int).Mul(previous.GasTipCap(), big.NewInt(110))
		tipBump.Div(tipBump, big.NewInt(100))
		if tx.GasFeeCap().Cmp(feeBump) < 0 || tx.GasTipCap().Cmp(tipBump) < 0 {
			return errors.New("replacement transaction underpriced")
		}
	}
	if err := txpool.ValidateTransactionWithState(validationTx, signer, &txpool.ValidationOptionsWithState{
		State: state,
		ExistingExpenditure: func(address common.Address) *big.Int {
			total := new(big.Int)
			for _, pooled := range c.pending[address] {
				total.Add(total, pooled.Cost())
			}
			return total
		},
		ExistingCost: func(address common.Address, nonce uint64) *big.Int {
			if pooled := c.pending[address][nonce]; pooled != nil {
				return pooled.Cost()
			}
			return nil
		},
	}); err != nil {
		return err
	}
	var encodedSidecar []byte
	if sidecar := tx.BlobTxSidecar(); sidecar != nil {
		switch sidecar.Version {
		case types.BlobSidecarVersion0:
			if len(sidecar.Blobs) != len(sidecar.Commitments) || len(sidecar.Blobs) != len(sidecar.Proofs) {
				return fmt.Errorf("%w: malformed version 0 sidecar", txpool.ErrKZGVerificationError)
			}
			for index := range sidecar.Blobs {
				if err := kzg4844.VerifyBlobProof(&sidecar.Blobs[index], sidecar.Commitments[index], sidecar.Proofs[index]); err != nil {
					return fmt.Errorf("%w: %v", txpool.ErrKZGVerificationError, err)
				}
			}
		case types.BlobSidecarVersion1:
			if err := kzg4844.VerifyCellProofs(sidecar.Blobs, sidecar.Commitments, sidecar.Proofs); err != nil {
				return fmt.Errorf("%w: %v", txpool.ErrKZGVerificationError, err)
			}
		default:
			return fmt.Errorf("%w: unsupported sidecar version %d", txpool.ErrKZGVerificationError, sidecar.Version)
		}
		encodedSidecar, err = rlp.EncodeToBytes(sidecar)
		if err != nil {
			return err
		}
		if err := c.db.Put(append(append([]byte(nil), blobNamespace...), tx.Hash().Bytes()...), encodedSidecar); err != nil {
			return err
		}
	}
	if byNonce == nil {
		byNonce = make(map[uint64]*types.Transaction)
		c.pending[from] = byNonce
	}
	if previous := byNonce[tx.Nonce()]; previous != nil {
		c.arrival[tx.Hash()] = c.arrival[previous.Hash()]
		delete(c.arrival, previous.Hash())
	} else {
		c.nextArrival++
		c.arrival[tx.Hash()] = c.nextArrival
	}
	byNonce[tx.Nonce()] = tx
	if sidecar := tx.BlobTxSidecar(); sidecar != nil {
		c.blobs[tx.Hash()] = sidecar.Copy()
	}
	return nil
}

func (c *executionChain) executableTransactions() ([]*types.Transaction, error) {
	head := c.blockchain.CurrentBlock()
	state, err := c.blockchain.StateAt(head)
	if err != nil {
		return nil, err
	}
	var result []*types.Transaction
	nextNonce := make(map[common.Address]uint64, len(c.pending))
	for address := range c.pending {
		nextNonce[address] = state.GetNonce(address)
	}
	for {
		var selected *types.Transaction
		var selectedAddress common.Address
		for address, nonce := range nextNonce {
			candidate := c.pending[address][nonce]
			if candidate != nil && (selected == nil || c.transactionBefore(candidate, selected)) {
				selected, selectedAddress = candidate, address
			}
		}
		if selected == nil {
			break
		}
		result = append(result, selected)
		nextNonce[selectedAddress]++
	}
	return result, nil
}

func (c *executionChain) transactionBefore(left, right *types.Transaction) bool {
	if c.order == "fifo" {
		return c.arrival[left.Hash()] < c.arrival[right.Hash()]
	}
	leftTip, _ := left.EffectiveGasTip(c.blockchain.CurrentBlock().BaseFee)
	rightTip, _ := right.EffectiveGasTip(c.blockchain.CurrentBlock().BaseFee)
	if comparison := leftTip.Cmp(rightTip); comparison != 0 {
		return comparison > 0
	}
	return left.Hash().Hex() < right.Hash().Hex()
}

func (c *executionChain) buildBlock(
	slotDuration uint64,
	empty bool,
	parentBeaconRoot common.Hash,
	withdrawalRequests []WithdrawalRequest,
) (block *types.Block, receipts types.Receipts, targetSlot uint64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parentHeader := c.blockchain.CurrentBlock()
	parent := c.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	withdrawals, err := assignedWithdrawals(c.blockchain, parent, withdrawalRequests)
	if err != nil {
		return nil, nil, 0, err
	}
	txs, err := c.executableTransactions()
	if err != nil {
		return nil, nil, 0, err
	}
	if empty {
		txs = nil
	}
	targetSlot = c.slot + 1
	targetTime := c.genesisTime + targetSlot*slotDuration
	if len(txs) == 0 {
		block, receipts, err = c.generateBlock(parent, targetTime, parentBeaconRoot, nil, withdrawals)
		return block, receipts, targetSlot, err
	}
	// Build the candidate incrementally. A transaction that became invalid after
	// a head change blocks only its own sender's nonce frontier; other senders
	// remain eligible for the block.
	signer := types.MakeSigner(c.config, new(big.Int).Add(parent.Number(), big.NewInt(1)), targetTime)
	accepted := make([]*types.Transaction, 0, len(txs))
	blocked := make(map[common.Address]struct{})
	for _, tx := range txs {
		from, senderErr := types.Sender(signer, tx)
		if senderErr != nil {
			continue
		}
		if _, exists := blocked[from]; exists {
			continue
		}
		trial := append(append(make([]*types.Transaction, 0, len(accepted)+1), accepted...), tx)
		if _, _, trialErr := c.generateBlock(parent, targetTime, parentBeaconRoot, trial, withdrawals); trialErr != nil {
			blocked[from] = struct{}{}
			continue
		}
		accepted = trial
	}
	block, receipts, err = c.generateBlock(parent, targetTime, parentBeaconRoot, accepted, withdrawals)
	return block, receipts, targetSlot, err
}

func (c *executionChain) generateBlock(
	parent *types.Block,
	targetTime uint64,
	parentBeaconRoot common.Hash,
	txs []*types.Transaction,
	withdrawals types.Withdrawals,
) (block *types.Block, receipts types.Receipts, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("block generation failed: %v", recovered)
			block, receipts = nil, nil
		}
	}()
	blocks, receiptSets := core.GenerateChain(c.config, parent, c.blockchain.Engine(), c.db, 1, func(_ int, gen *core.BlockGen) {
		gen.OffsetTime(int64(targetTime) - int64(gen.Timestamp()))
		gen.SetPoS()
		gen.SetCoinbase(c.feeRecipient)
		gen.SetParentBeaconRoot(parentBeaconRoot)
		addWithdrawals(gen, withdrawals)
		for _, tx := range txs {
			gen.AddTxWithChain(c.blockchain, tx.WithoutBlobTxSidecar())
		}
	})
	block = blocks[0]
	receipts = receiptSets[0]
	block = replaceGeneratedWithdrawals(block, receipts, withdrawals)
	return block, receipts, nil
}

func (c *executionChain) applyCanonicalBlock(block *types.Block, targetSlot uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tx := range block.Transactions() {
		signer := types.MakeSigner(c.config, block.Number(), block.Time())
		from, senderErr := types.Sender(signer, tx)
		if senderErr == nil {
			delete(c.arrival, tx.Hash())
			delete(c.pending[from], tx.Nonce())
			if len(c.pending[from]) == 0 {
				delete(c.pending, from)
			}
		}
	}
	c.slot = targetSlot
	c.lastProcessedSlot = targetSlot
	c.slotByHash[block.Hash()] = targetSlot
	c.canonicalBlockBySlot[targetSlot] = block.Hash()
}

func (c *executionChain) blockAtOrBeforeSlot(slot uint64) *types.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for {
		if hash, ok := c.canonicalBlockBySlot[slot]; ok {
			block := c.blockchain.GetBlockByHash(hash)
			if block != nil && c.blockchain.GetCanonicalHash(block.NumberU64()) == hash {
				return block
			}
		}
		if slot == 0 {
			return c.blockchain.Genesis()
		}
		slot--
	}
}

func (c *executionChain) slotOf(block *types.Block) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slotOfUnlocked(block)
}

func (c *executionChain) slotOfUnlocked(block *types.Block) uint64 {
	if slot, ok := c.slotByHash[block.Hash()]; ok {
		return slot
	}
	if block.Time() <= c.genesisTime || c.slotDuration == 0 {
		return 0
	}
	return (block.Time() - c.genesisTime) / c.slotDuration
}

func (c *executionChain) currentSlot() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.slot
}

func (c *executionChain) blobSidecar(hash common.Hash) *types.BlobTxSidecar {
	c.mu.RLock()
	if sidecar := c.blobs[hash]; sidecar != nil {
		c.mu.RUnlock()
		return sidecar.Copy()
	}
	c.mu.RUnlock()
	encoded, err := c.db.Get(append(append([]byte(nil), blobNamespace...), hash.Bytes()...))
	if err != nil {
		return nil
	}
	var sidecar types.BlobTxSidecar
	if rlp.DecodeBytes(encoded, &sidecar) != nil {
		return nil
	}
	return sidecar.Copy()
}

func (c *executionChain) pendingCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, txs := range c.pending {
		total += len(txs)
	}
	return total
}

func (c *executionChain) feeRecipientAddress() common.Address {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.feeRecipient
}

func (c *executionChain) setFeeRecipient(address common.Address) {
	c.mu.Lock()
	c.feeRecipient = address
	c.mu.Unlock()
}

func (c *executionChain) setPendingView(block *types.Block, statedb *state.StateDB, receipts types.Receipts) {
	c.mu.Lock()
	defer c.mu.Unlock()
	executable := make(map[common.Hash]struct{}, len(block.Transactions()))
	for _, tx := range block.Transactions() {
		executable[tx.Hash()] = struct{}{}
	}
	queued := make(map[common.Hash]struct{})
	for _, byNonce := range c.pending {
		for _, tx := range byNonce {
			if _, exists := executable[tx.Hash()]; !exists {
				queued[tx.Hash()] = struct{}{}
			}
		}
	}
	c.pendingView = &pendingView{
		block: block, state: statedb.Copy(), receipts: receipts,
		executable: executable, queued: queued,
	}
}

func (c *executionChain) pendingBlock() *types.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.pendingView == nil {
		return nil
	}
	return c.pendingView.block
}

func (c *executionChain) pendingSnapshot() (*types.Block, *state.StateDB, types.Receipts) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.pendingView == nil {
		return nil, nil, nil
	}
	return c.pendingView.block, c.pendingView.state.Copy(), append(types.Receipts(nil), c.pendingView.receipts...)
}

func (c *executionChain) pendingClassification(hash common.Hash) (executable, queued bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.pendingView == nil {
		return false, false
	}
	_, executable = c.pendingView.executable[hash]
	_, queued = c.pendingView.queued[hash]
	return executable, queued
}
