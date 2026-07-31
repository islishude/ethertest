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
	mu           sync.RWMutex
	config       *params.ChainConfig
	db           ethdb.Database
	blockchain   *core.BlockChain
	accounts     []Account
	feeRecipient common.Address
	slot         uint64
	genesisTime  uint64
	slotDuration uint64
	slotByHash   map[common.Hash]uint64
	blockBySlot  map[uint64]common.Hash
	pending      map[common.Address]map[uint64]*types.Transaction
	arrival      map[common.Hash]uint64
	nextArrival  uint64
	blobs        map[common.Hash]*types.BlobTxSidecar
	order        string
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

func newExecutionChain(cfg Config) (*executionChain, error) {
	accounts, err := DeriveAccounts(cfg.Accounts.Mnemonic, cfg.Accounts.Count)
	if err != nil {
		return nil, err
	}
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
	chainConfig := executionChainConfig(cfg)
	genesis := core.DeveloperGenesisBlock(cfg.Chain.GasLimit, nil)
	genesis.Config = chainConfig
	genesis.Timestamp = uint64(cfg.Chain.GenesisTime)
	balance, err := parseBalance(cfg.Accounts.Balance)
	if err != nil {
		_ = database.Close() //nolint:errcheck
		return nil, err
	}
	for _, account := range accounts {
		genesis.Alloc[account.Address] = types.Account{Balance: new(big.Int).Set(balance)}
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
	feeRecipient := accounts[0].Address
	if cfg.Mining.FeeRecipient != "" {
		if !common.IsHexAddress(cfg.Mining.FeeRecipient) {
			blockchain.Stop()
			_ = database.Close() //nolint:errcheck
			return nil, errors.New("invalid mining.fee_recipient")
		}
		feeRecipient = common.HexToAddress(cfg.Mining.FeeRecipient)
	}
	slotByHash := make(map[common.Hash]uint64)
	blockBySlot := make(map[uint64]common.Hash)
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
		blockBySlot[slot] = block.Hash()
	}
	currentSlot := slotByHash[blockchain.CurrentBlock().Hash()]
	return &executionChain{
		config: chainConfig, db: database, blockchain: blockchain,
		accounts: accounts, feeRecipient: feeRecipient,
		pending: make(map[common.Address]map[uint64]*types.Transaction),
		arrival: make(map[common.Hash]uint64),
		blobs:   make(map[common.Hash]*types.BlobTxSidecar), order: cfg.Mining.Order,
		genesisTime: uint64(cfg.Chain.GenesisTime), slotDuration: uint64(cfg.Chain.SlotDuration / time.Second),
		slot: currentSlot, slotByHash: slotByHash, blockBySlot: blockBySlot,
	}, nil
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
	signer := types.MakeSigner(c.config, head.Number, head.Time)
	opts := &txpool.ValidationOptions{
		Config: c.config, Accept: 0xff, MaxSize: 4 << 20,
		MaxBlobCount: params.BlobTxMaxBlobs, MinTip: new(big.Int),
	}
	if err := txpool.ValidateTransaction(tx, head, signer, opts); err != nil {
		return err
	}
	if sidecar := tx.BlobTxSidecar(); sidecar != nil {
		if err := kzg4844.VerifyCellProofs(sidecar.Blobs, sidecar.Commitments, sidecar.Proofs); err != nil {
			return fmt.Errorf("%w: %v", txpool.ErrKZGVerificationError, err)
		}
	}
	from, _ := types.Sender(signer, tx)
	state, err := c.blockchain.StateAt(head)
	if err != nil {
		return err
	}
	if tx.Nonce() < state.GetNonce(from) {
		return fmt.Errorf("%w: next nonce %d, tx nonce %d", core.ErrNonceTooLow, state.GetNonce(from), tx.Nonce())
	}
	if state.GetBalance(from).ToBig().Cmp(tx.Cost()) < 0 {
		return core.ErrInsufficientFunds
	}
	byNonce := c.pending[from]
	if byNonce == nil {
		byNonce = make(map[uint64]*types.Transaction)
		c.pending[from] = byNonce
	}
	if previous := byNonce[tx.Nonce()]; previous != nil {
		feeBump := new(big.Int).Mul(previous.GasFeeCap(), big.NewInt(110))
		feeBump.Div(feeBump, big.NewInt(100))
		tipBump := new(big.Int).Mul(previous.GasTipCap(), big.NewInt(110))
		tipBump.Div(tipBump, big.NewInt(100))
		if tx.GasFeeCap().Cmp(feeBump) < 0 || tx.GasTipCap().Cmp(tipBump) < 0 {
			return errors.New("replacement transaction underpriced")
		}
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
		encoded, err := rlp.EncodeToBytes(sidecar)
		if err != nil {
			return err
		}
		if err := c.db.Put(append(append([]byte(nil), blobNamespace...), tx.Hash().Bytes()...), encoded); err != nil {
			return err
		}
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

func (c *executionChain) mine(slotDuration uint64, empty bool) (block *types.Block, receipts types.Receipts, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	parentHeader := c.blockchain.CurrentBlock()
	parent := c.blockchain.GetBlock(parentHeader.Hash(), parentHeader.Number.Uint64())
	txs, err := c.executableTransactions()
	if err != nil {
		return nil, nil, err
	}
	if empty {
		txs = nil
	}
	targetSlot := c.slot + 1
	targetTime := c.genesisTime + targetSlot*slotDuration
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
		gen.SetParentBeaconRoot(parent.Hash())
		var gas uint64
		for _, tx := range txs {
			if gas+tx.Gas() > parent.GasLimit() {
				continue
			}
			gen.AddTxWithChain(c.blockchain, tx.WithoutBlobTxSidecar())
			gas += tx.Gas()
		}
	})
	if _, err := c.blockchain.InsertChain(blocks); err != nil {
		return nil, nil, err
	}
	block = blocks[0]
	receipts = receiptSets[0]
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
	c.slotByHash[block.Hash()] = targetSlot
	c.blockBySlot[targetSlot] = block.Hash()
	return block, receipts, nil
}

func (c *executionChain) missedSlot() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slot++
	return c.slot
}

func (c *executionChain) blockAtOrBeforeSlot(slot uint64) *types.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for {
		if hash, ok := c.blockBySlot[slot]; ok {
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
