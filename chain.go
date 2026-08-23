package ethertest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/types/bal"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/triedb"
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
	genesisHash          common.Hash
	externalGenesis      bool
	slotDuration         uint64
	slotByHash           map[common.Hash]uint64
	canonicalBlockBySlot map[uint64]common.Hash
	beaconBlockByRoot    map[common.Hash]common.Hash
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
	blobs                map[common.Hash]*blobBundle
	order                string
	pendingView          *pendingView
}

type pendingView struct {
	block      *types.Block
	state      *state.StateDB
	receipts   types.Receipts
	executable map[common.Hash]struct{}
	queued     map[common.Hash]struct{}
	referenced *common.Hash
	trie       *triedb.Database
}

type transactionPoolSnapshot struct {
	pending     map[common.Address]map[uint64]*types.Transaction
	arrival     map[common.Hash]uint64
	nextArrival uint64
	blobs       map[common.Hash]*blobBundle
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
		BlobScheduleConfig:      repositoryBlobSchedule(),
	}
}

func newExecutionChain(cfg *Config, accounts []common.Address, suppliedGenesis *core.Genesis) (*executionChain, error) {
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
	var persisted storedTimeline
	if existingData {
		var err error
		persisted, err = readPersistedGenesisMetadata(database)
		if err != nil {
			_ = database.Close() //nolint:errcheck
			return nil, err
		}
		storedHash := rawdb.ReadCanonicalHash(database, 0)
		if storedHash == (common.Hash{}) {
			_ = database.Close() //nolint:errcheck
			return nil, errors.New("persisted ethertest data is missing the execution genesis")
		}
		if persisted.GenesisHash != (common.Hash{}) && persisted.GenesisHash != storedHash {
			_ = database.Close() //nolint:errcheck
			return nil, fmt.Errorf("stored genesis metadata hash %s does not match database hash %s", persisted.GenesisHash, storedHash)
		}
	}

	genesis := suppliedGenesis
	externalGenesis := !existingData && suppliedGenesis != nil
	if existingData && (persisted.ExternalGenesis || suppliedGenesis != nil) {
		storedGenesis, err := readPersistedExecutionGenesis(database)
		if err != nil {
			_ = database.Close() //nolint:errcheck
			return nil, fmt.Errorf("read persisted execution genesis: %w", err)
		}
		if suppliedGenesis != nil {
			if err := compareExecutionGenesis(storedGenesis, suppliedGenesis); err != nil {
				_ = database.Close() //nolint:errcheck
				return nil, err
			}
		}
		genesis = storedGenesis
		externalGenesis = persisted.ExternalGenesis
	}
	if genesis != nil {
		if err := applyExecutionGenesis(cfg, genesis); err != nil {
			_ = database.Close() //nolint:errcheck
			return nil, fmt.Errorf("execution genesis: %w", err)
		}
	}
	if existingData {
		if cfg.Chain.GenesisTime == 0 && !persisted.ExternalGenesis && suppliedGenesis == nil {
			cfg.Chain.GenesisTime = int64(persisted.GenesisTime)
		} else if uint64(cfg.Chain.GenesisTime) != persisted.GenesisTime {
			_ = database.Close() //nolint:errcheck
			return nil, fmt.Errorf("configured genesis time %d does not match stored genesis time %d", cfg.Chain.GenesisTime, persisted.GenesisTime)
		}
	} else if genesis == nil && cfg.Chain.GenesisTime == 0 {
		cfg.Chain.GenesisTime = time.Now().UTC().Unix()
	}
	if err := cfg.validateResolved(); err != nil {
		_ = database.Close() //nolint:errcheck
		return nil, err
	}
	if genesis == nil {
		chainConfig := executionChainConfig(*cfg)
		genesis = core.DeveloperGenesisBlock(cfg.Chain.GasLimit, nil)
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
	}
	if cfg.Chain.NetworkID == 0 {
		cfg.Chain.NetworkID = cfg.Chain.ChainID
	}
	if err := cfg.validateResolved(); err != nil {
		_ = database.Close() //nolint:errcheck
		return nil, err
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
		config: blockchain.Config(), db: database, blockchain: blockchain,
		feeRecipient: feeRecipient,
		pending:      make(map[common.Address]map[uint64]*types.Transaction),
		arrival:      make(map[common.Hash]uint64),
		blobs:        make(map[common.Hash]*blobBundle), order: cfg.Mining.Order,
		genesisTime: uint64(cfg.Chain.GenesisTime), genesisHash: blockchain.Genesis().Hash(),
		externalGenesis: externalGenesis, slotDuration: uint64(cfg.Chain.SlotDuration / time.Second),
		slot: currentSlot, slotByHash: slotByHash, canonicalBlockBySlot: canonicalBlockBySlot,
		beaconBlockByRoot: make(map[common.Hash]common.Hash),
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
	var retainedBundle *blobBundle
	if sidecar := tx.BlobTxSidecar(); sidecar != nil {
		retainedBundle, err = newBlobBundle(sidecar)
		if err != nil {
			return fmt.Errorf("%w: %v", txpool.ErrKZGVerificationError, err)
		}
	}
	if byNonce == nil {
		byNonce = make(map[uint64]*types.Transaction)
		c.pending[from] = byNonce
	}
	if previous := byNonce[tx.Nonce()]; previous != nil {
		c.arrival[tx.Hash()] = c.arrival[previous.Hash()]
		delete(c.arrival, previous.Hash())
		delete(c.blobs, previous.Hash())
	} else {
		c.nextArrival++
		c.arrival[tx.Hash()] = c.nextArrival
	}
	byNonce[tx.Nonce()] = tx
	if retainedBundle != nil {
		c.blobs[tx.Hash()] = retainedBundle
	}
	return nil
}

func (c *executionChain) snapshotTransactionPool() transactionPoolSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	snapshot := transactionPoolSnapshot{
		pending: make(map[common.Address]map[uint64]*types.Transaction, len(c.pending)),
		arrival: make(map[common.Hash]uint64, len(c.arrival)), nextArrival: c.nextArrival,
		blobs: make(map[common.Hash]*blobBundle, len(c.blobs)),
	}
	for address, transactions := range c.pending {
		snapshot.pending[address] = make(map[uint64]*types.Transaction, len(transactions))
		maps.Copy(snapshot.pending[address], transactions)
	}
	maps.Copy(snapshot.arrival, c.arrival)
	for hash, bundle := range c.blobs {
		snapshot.blobs[hash] = bundle.copy()
	}
	return snapshot
}

func (c *executionChain) restoreTransactionPool(snapshot transactionPoolSnapshot) {
	c.mu.Lock()
	c.pending = snapshot.pending
	c.arrival = snapshot.arrival
	c.nextArrival = snapshot.nextArrival
	c.blobs = snapshot.blobs
	c.mu.Unlock()
}

func (c *executionChain) blobPuts(block *types.Block) ([]journalKV, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var puts []journalKV
	for _, transaction := range block.Transactions() {
		bundle := c.blobs[transaction.Hash()]
		if bundle == nil {
			continue
		}
		encoded, err := rlp.EncodeToBytes(bundle)
		if err != nil {
			return nil, err
		}
		puts = append(puts, journalKV{
			Key: append(append([]byte(nil), blobNamespace...), transaction.Hash().Bytes()...), Value: encoded,
		})
	}
	return puts, nil
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
	ctx context.Context,
	slotDuration uint64,
	empty bool,
	parentBeaconRoot common.Hash,
	withdrawalRequests []WithdrawalRequest,
) (block *types.Block, receipts types.Receipts, postState *state.StateDB, candidate *candidateState, targetSlot uint64, executionRequests [][]byte, err error) {
	c.mu.RLock()
	targetSlot = c.slot + 1
	c.mu.RUnlock()
	if targetSlot == 0 {
		return nil, nil, nil, nil, 0, nil, errors.New("next block slot overflows uint64")
	}
	return c.buildBlockAtSlot(ctx, slotDuration, empty, parentBeaconRoot, withdrawalRequests, targetSlot)
}

func (c *executionChain) buildBlockAtSlot(
	ctx context.Context,
	slotDuration uint64,
	empty bool,
	parentBeaconRoot common.Hash,
	withdrawalRequests []WithdrawalRequest,
	targetSlot uint64,
) (block *types.Block, receipts types.Receipts, postState *state.StateDB, candidate *candidateState, slot uint64, executionRequests [][]byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parentHeader := c.blockchain.CurrentBlock()
	parent := c.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	withdrawals, err := assignedWithdrawals(c.blockchain, parent, withdrawalRequests)
	if err != nil {
		return nil, nil, nil, nil, 0, nil, err
	}
	txs, err := c.executableTransactions()
	if err != nil {
		return nil, nil, nil, nil, 0, nil, err
	}
	if empty {
		txs = nil
	}
	if targetSlot == 0 || targetSlot <= c.slot {
		return nil, nil, nil, nil, 0, nil, errors.New("target block slot must advance the current slot")
	}
	if slotDuration != 0 && targetSlot > (^uint64(0)-c.genesisTime)/slotDuration {
		return nil, nil, nil, nil, 0, nil, errors.New("next block timestamp overflows uint64")
	}
	targetTime := c.genesisTime + targetSlot*slotDuration
	candidate, initialState, err := newCandidateState(c.db, parent.Root())
	if err != nil {
		return nil, nil, nil, nil, 0, nil, err
	}
	block, receipts, postState, executionRequests, err = c.generateBlock(
		ctx, parent, targetTime, parentBeaconRoot, txs, withdrawals, true, nil, initialState, nil,
	)
	return block, receipts, postState, candidate, targetSlot, executionRequests, err
}

func (c *executionChain) generateBlock(
	ctx context.Context,
	parent *types.Block,
	targetTime uint64,
	parentBeaconRoot common.Hash,
	txs []*types.Transaction,
	withdrawals types.Withdrawals,
	skipInvalid bool,
	extra []byte,
	initialState *state.StateDB,
	feeRecipient *common.Address,
) (block *types.Block, receipts types.Receipts, postState *state.StateDB, executionRequests [][]byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("block generation failed: %v", recovered)
			block, receipts, postState, executionRequests = nil, nil, nil, nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, nil, err
	}
	parentHeader := parent.Header()
	if parent.Number().Sign() < 0 || parent.Number().BitLen() > 64 || parent.NumberU64() == ^uint64(0) {
		return nil, nil, nil, nil, errors.New("next block number overflows uint64")
	}
	coinbase := c.feeRecipient
	if feeRecipient != nil {
		coinbase = *feeRecipient
	}
	header := &types.Header{
		ParentHash: parent.Hash(), Coinbase: coinbase,
		Number: new(big.Int).Add(parent.Number(), big.NewInt(1)), GasLimit: parent.GasLimit(),
		Time: targetTime, Difficulty: new(big.Int), Extra: append([]byte(nil), extra...),
	}
	if c.config.IsLondon(header.Number) {
		header.BaseFee = eip1559.CalcBaseFee(c.config, parentHeader)
		if !c.config.IsLondon(parent.Number()) {
			parentGasLimit := parent.GasLimit() * c.config.ElasticityMultiplier()
			header.GasLimit = core.CalcGasLimit(parentGasLimit, parentGasLimit)
		}
	}
	if c.config.IsCancun(header.Number, header.Time) {
		excess := eip4844.CalcExcessBlobGas(c.config, parentHeader, targetTime)
		header.ExcessBlobGas = &excess
		header.BlobGasUsed = new(uint64)
		header.ParentBeaconRoot = &parentBeaconRoot
	}
	if err := c.blockchain.Engine().Prepare(c.blockchain, header); err != nil {
		return nil, nil, nil, nil, err
	}
	postState = initialState
	if postState == nil {
		postState, err = c.blockchain.StateAt(parentHeader)
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}
	blockAccessList := bal.NewConstructionBlockAccessList()
	preEVM := vm.NewEVM(core.NewEVMBlockContext(header, c.blockchain, nil), postState, c.config, vm.Config{})
	blockAccessList.Merge(core.PreExecution(ctx, header.ParentBeaconRoot, parentHeader, c.config, preEVM, header.Number, header.Time))
	preEVM.Release()

	gasPool := core.NewGasPool(header.GasLimit)
	signer := types.MakeSigner(c.config, header.Number, header.Time)
	accepted := make([]*types.Transaction, 0, len(txs))
	receipts = make(types.Receipts, 0, len(txs))
	allLogs := make([]*types.Log, 0)
	blocked := make(map[common.Address]struct{})
	for _, transaction := range txs {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		tx := transaction.WithoutBlobTxSidecar()
		from, senderErr := types.Sender(signer, tx)
		if senderErr != nil {
			if skipInvalid {
				continue
			}
			return nil, nil, nil, nil, senderErr
		}
		if _, exists := blocked[from]; exists {
			continue
		}
		message, messageErr := core.TransactionToMessage(tx, signer, header.BaseFee)
		if messageErr != nil {
			if skipInvalid {
				blocked[from] = struct{}{}
				continue
			}
			return nil, nil, nil, nil, messageErr
		}
		stateSnapshot := postState.Snapshot()
		gasSnapshot := gasPool.Snapshot()
		index := len(accepted)
		postState.SetTxContext(tx.Hash(), index, uint32(index+1))
		txEVM := vm.NewEVM(core.NewEVMBlockContext(header, c.blockchain, nil), postState, c.config, vm.Config{})
		txEVM.SetTxContext(core.NewEVMTxContext(message))
		receipt, accessList, applyErr := core.ApplyTransactionWithEVM(
			message, gasPool, postState, header.Number, header.Hash(), header.Time, tx, txEVM,
		)
		txEVM.Release()
		if applyErr != nil {
			postState.RevertToSnapshot(stateSnapshot)
			gasPool.Set(gasSnapshot)
			if skipInvalid {
				blocked[from] = struct{}{}
				continue
			}
			return nil, nil, nil, nil, applyErr
		}
		accepted = append(accepted, tx)
		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
		blockAccessList.Merge(accessList)
		if header.BlobGasUsed != nil {
			*header.BlobGasUsed += receipt.BlobGasUsed
		}
	}
	header.GasUsed = gasPool.Used()
	postEVM := vm.NewEVM(core.NewEVMBlockContext(header, c.blockchain, nil), postState, c.config, vm.Config{})
	executionRequests, requestAccessList, err := core.PostExecution(
		ctx, c.config, header.Number, header.Time, allLogs, postEVM, uint32(len(accepted)+1),
	)
	postEVM.Release()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	blockAccessList.Merge(requestAccessList)
	if executionRequests != nil {
		hash := types.CalcRequestsHash(executionRequests)
		header.RequestsHash = &hash
	}
	body := &types.Body{Transactions: accepted, Withdrawals: withdrawals}
	if c.config.IsShanghai(header.Number, header.Time) && body.Withdrawals == nil {
		body.Withdrawals = types.Withdrawals{}
	}
	c.blockchain.Engine().Finalize(c.blockchain, header, postState, body, uint32(len(accepted)+1), blockAccessList)
	if err := postState.Error(); err != nil {
		return nil, nil, nil, nil, err
	}
	block = core.AssembleBlock(c.blockchain, header, postState, body, receipts, blockAccessList)
	if err := c.deriveReceiptFields(block, receipts); err != nil {
		return nil, nil, nil, nil, err
	}
	if err := verifyExecutionRequestsHash(block, executionRequests); err != nil {
		return nil, nil, nil, nil, err
	}
	return block, receipts, postState, cloneExecutionRequestBytes(executionRequests), nil
}

func (c *executionChain) deriveReceiptFields(block *types.Block, receipts types.Receipts) error {
	var blobGasPrice *big.Int
	if block.ExcessBlobGas() != nil {
		blobGasPrice = eip4844.CalcBlobFee(c.config, block.Header())
	}
	return receipts.DeriveFields(
		c.config, block.Hash(), block.NumberU64(), block.Time(), block.BaseFee(), blobGasPrice, block.Transactions(),
	)
}

func (c *executionChain) applyCanonicalBlock(block *types.Block, targetSlot uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tx := range block.Transactions() {
		signer := types.MakeSigner(c.config, block.Number(), block.Time())
		from, senderErr := types.Sender(signer, tx)
		if senderErr == nil {
			delete(c.arrival, tx.Hash())
			delete(c.blobs, tx.Hash())
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

func (c *executionChain) blobSidecarForVersion(hash common.Hash, version byte) *types.BlobTxSidecar {
	c.mu.RLock()
	if bundle := c.blobs[hash]; bundle != nil {
		c.mu.RUnlock()
		return bundle.sidecar(version)
	}
	c.mu.RUnlock()
	encoded, err := c.db.Get(append(append([]byte(nil), blobNamespace...), hash.Bytes()...))
	if err != nil {
		return nil
	}
	var bundle blobBundle
	if rlp.DecodeBytes(encoded, &bundle) != nil || bundle.validate() != nil {
		return nil
	}
	return bundle.sidecar(version)
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

func (c *executionChain) setPendingView(view *pendingView) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingView != nil && c.pendingView.referenced != nil {
		_ = c.pendingView.trie.Dereference(*c.pendingView.referenced)
	}
	block, statedb, receipts := view.block, view.state, view.receipts
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
		executable: executable, queued: queued, referenced: cloneHashPointer(view.referenced), trie: view.trie,
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
