package ethertest

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/klauspost/compress/zstd"
)

const StateArchiveFormat = "ethertest-state-v3"

type StateManifest struct {
	Format           string   `json:"format"`
	Version          string   `json:"version"`
	CreatedAt        string   `json:"created_at"`
	ChainID          uint64   `json:"chain_id"`
	GenesisHash      string   `json:"genesis_hash"`
	HeadHash         string   `json:"head_hash"`
	HeadNumber       uint64   `json:"head_number"`
	Revision         uint64   `json:"revision"`
	DatabaseSHA256   string   `json:"database_sha256"`
	Secrets          bool     `json:"secrets"`
	Tainted          bool     `json:"tainted"`
	HeadTainted      bool     `json:"head_tainted"`
	TaintReasons     []string `json:"taint_reasons,omitempty"`
	TimelineComplete bool     `json:"timeline_complete"`
	ConsensusMode    string   `json:"consensus_mode"`
}

func (n *Node) DumpState(path string) error {
	n.lifecycleMu.Lock()
	switch n.lifecycle {
	case nodeLifecycleNew:
		defer n.lifecycleMu.Unlock()
		return n.dumpState(path)
	case nodeLifecycleStarting, nodeLifecycleRunning:
		n.lifecycleMu.Unlock()
		_, err := n.execute(context.Background(), func(_ *executionChain) (any, error) {
			return nil, n.dumpState(path)
		})
		return err
	default:
		n.lifecycleMu.Unlock()
		return ErrNodeStopped
	}
}

func (n *Node) dumpState(path string) error {
	n.chain.mu.RLock()
	defer n.chain.mu.RUnlock()
	if n.writeErr != nil {
		return fmt.Errorf("cannot archive after a persistence failure: %w", n.writeErr)
	}
	prepared, err := n.chain.db.Has(journalKey)
	if err != nil {
		return fmt.Errorf("check recovery journal before archive: %w", err)
	}
	if prepared {
		return errors.New("cannot archive while a prepared operation requires recovery")
	}
	spoolPath, databaseSize, sum, err := spoolDatabase(n.chain.db, filepath.Dir(path))
	if err != nil {
		return err
	}
	defer os.Remove(spoolPath) //nolint:errcheck
	genesis := n.chain.blockchain.GetBlockByNumber(0)
	head := n.chain.blockchain.CurrentBlock()
	manifest := StateManifest{
		Format: StateArchiveFormat, Version: Version, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ChainID: n.cfg.Chain.ChainID, GenesisHash: genesis.Hash().Hex(),
		HeadHash: head.Hash().Hex(), HeadNumber: head.Number.Uint64(),
		Revision: uint64(n.Revision()), DatabaseSHA256: hex.EncodeToString(sum),
		Tainted:          n.chain.sessionTainted,
		HeadTainted:      n.chain.blockSafety[head.Hash()].Tainted,
		TaintReasons:     sortedReasonSet(n.chain.taintReasons),
		TimelineComplete: n.chain.timelineComplete,
		ConsensusMode:    "synthetic",
	}
	if err := writeArchiveAtomicFromSpool(path, manifest, spoolPath, databaseSize); err != nil {
		return err
	}
	n.logger.Info("state archive written",
		"event", "state_archive_written",
		"path", path,
		"head_number", manifest.HeadNumber,
		"head_hash", manifest.HeadHash,
		"revision", manifest.Revision,
		"database_bytes", databaseSize,
	)
	return nil
}

func InspectState(path string) (StateManifest, error) {
	return inspectArchive(path)
}

// LoadState replaces an empty Pebble destination with a verified archive.
func LoadState(path, destination string) error {
	if destination == "" {
		return errors.New("destination is required")
	}
	target, err := inspectEmptyDirectoryTarget(destination)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+".load-*")
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(staging)
		}
	}()
	kv, err := pebble.New(staging, 64, 64, "ethertest-load", false)
	if err != nil {
		return err
	}
	db := rawdb.NewDatabase(kv)
	manifest, err := scanArchive(path, db)
	if err != nil {
		_ = db.Close() // nolint:errcheck
		return err
	}
	if err := validateArchiveDatabase(db, manifest); err != nil {
		_ = db.Close() //nolint:errcheck
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if _, err := installStagedDirectory(staging, destination, target); err != nil {
		return err
	}
	installed = true
	return nil
}

func encodeDatabase(db ethdb.Database) ([]byte, error) {
	var out bytes.Buffer
	if err := encodeDatabaseToWriter(db, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func encodeDatabaseToWriter(db ethdb.Database, output io.Writer) error {
	writer := bufio.NewWriter(output)
	iterator := db.NewIterator(nil, nil)
	defer iterator.Release()
	var lengths [8]byte
	for iterator.Next() {
		key, value := iterator.Key(), iterator.Value()
		if uint64(len(key)) > uint64(^uint32(0)) || uint64(len(value)) > uint64(^uint32(0)) {
			return errors.New("database record exceeds archive encoding limit")
		}
		binary.BigEndian.PutUint32(lengths[:4], uint32(len(key)))
		binary.BigEndian.PutUint32(lengths[4:], uint32(len(value)))
		if _, err := writer.Write(lengths[:]); err != nil {
			return err
		}
		if _, err := writer.Write(key); err != nil {
			return err
		}
		if _, err := writer.Write(value); err != nil {
			return err
		}
	}
	if err := iterator.Error(); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return nil
}

func spoolDatabase(db ethdb.Database, directory string) (path string, size int64, sum []byte, err error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", 0, nil, err
	}
	file, err := os.CreateTemp(directory, ".ethertest-database-*.tmp")
	if err != nil {
		return "", 0, nil, err
	}
	path = file.Name()
	hasher := sha256.New()
	writer := io.MultiWriter(file, hasher)
	defer func() {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = encodeDatabaseToWriter(db, writer); err != nil {
		return "", 0, nil, err
	}
	if err = file.Sync(); err != nil {
		return "", 0, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return "", 0, nil, err
	}
	return path, info.Size(), hasher.Sum(nil), nil
}

func decodeDatabaseReader(reader io.Reader, size int64, db ethdb.Database) error {
	remaining := size
	batch := db.NewBatch()
	defer batch.Close()
	var lengths [8]byte
	var previousKey []byte
	for remaining != 0 {
		if remaining < int64(len(lengths)) {
			return errors.New("corrupt database record header")
		}
		if _, err := io.ReadFull(reader, lengths[:]); err != nil {
			return errors.New("corrupt database record header")
		}
		remaining -= int64(len(lengths))
		keySize := binary.BigEndian.Uint32(lengths[:4])
		valueSize := binary.BigEndian.Uint32(lengths[4:])
		if uint64(keySize)+uint64(valueSize) > uint64(remaining) {
			return errors.New("corrupt database record length")
		}
		key, value := make([]byte, keySize), make([]byte, valueSize)
		if _, err := io.ReadFull(reader, key); err != nil {
			return errors.New("corrupt database key")
		}
		if _, err := io.ReadFull(reader, value); err != nil {
			return errors.New("corrupt database value")
		}
		remaining -= int64(keySize) + int64(valueSize)
		if previousKey != nil && bytes.Compare(previousKey, key) >= 0 {
			return errors.New("archive database keys are duplicated or out of order")
		}
		previousKey = append(previousKey[:0], key...)
		if err := batch.Put(key, value); err != nil {
			return err
		}
		if batch.ValueSize() >= 4<<20 {
			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	return batch.Write()
}

func writeArchiveAtomic(path string, manifest StateManifest, database []byte) (err error) {
	sum := sha256.Sum256(database)
	manifest.DatabaseSHA256 = hex.EncodeToString(sum[:])
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		_ = file.Close() // nolint:errcheck
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	zstdWriter, err := zstd.NewWriter(file)
	if err != nil {
		return err
	}
	tarWriter := tar.NewWriter(zstdWriter)
	for _, entry := range []struct {
		name     string
		contents []byte
	}{
		{"manifest.json", manifestJSON},
		{"database.bin", database},
	} {
		name, contents := entry.name, entry.contents
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(contents))}); err != nil {
			return err
		}
		if _, err := tarWriter.Write(contents); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := zstdWriter.Close(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func writeArchiveAtomicFromSpool(path string, manifest StateManifest, spoolPath string, databaseSize int64) (err error) {
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	defer func() {
		_ = file.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	zstdWriter, err := zstd.NewWriter(file)
	if err != nil {
		return err
	}
	tarWriter := tar.NewWriter(zstdWriter)
	spool, err := os.Open(spoolPath)
	if err != nil {
		return err
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "database.bin", Mode: 0o600, Size: databaseSize}); err != nil {
		_ = spool.Close()
		return err
	}
	if _, err := io.CopyN(tarWriter, spool, databaseSize); err != nil {
		_ = spool.Close()
		return err
	}
	if err := spool.Close(); err != nil {
		return err
	}
	if err := tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifestJSON))}); err != nil {
		return err
	}
	if _, err := tarWriter.Write(manifestJSON); err != nil {
		return err
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := zstdWriter.Close(); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func inspectArchive(path string) (StateManifest, error) {
	staging, err := os.MkdirTemp("", "ethertest-inspect-*")
	if err != nil {
		return StateManifest{}, err
	}
	defer os.RemoveAll(staging) //nolint:errcheck
	kv, err := pebble.New(staging, 64, 64, "ethertest-inspect", false)
	if err != nil {
		return StateManifest{}, err
	}
	db := rawdb.NewDatabase(kv)
	manifest, scanErr := scanArchive(path, db)
	if scanErr == nil {
		scanErr = validateArchiveDatabase(db, manifest)
	}
	closeErr := db.Close()
	return manifest, errors.Join(scanErr, closeErr)
}

func scanArchive(path string, db ethdb.Database) (StateManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return StateManifest{}, err
	}
	defer file.Close() //nolint:errcheck
	zstdReader, err := zstd.NewReader(file)
	if err != nil {
		return StateManifest{}, err
	}
	defer zstdReader.Close()
	tarReader := tar.NewReader(zstdReader)
	var manifest StateManifest
	var databaseSum []byte
	seenManifest, seenDatabase := false, false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return StateManifest{}, err
		}
		switch header.Name {
		case "manifest.json":
			if seenManifest || header.Size < 0 || header.Size > 1<<20 {
				return StateManifest{}, errors.New("archive manifest is duplicated or too large")
			}
			seenManifest = true
			decoder := json.NewDecoder(io.LimitReader(tarReader, header.Size))
			if err := decoder.Decode(&manifest); err != nil {
				return StateManifest{}, err
			}
			var trailing any
			if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
				return StateManifest{}, errors.New("archive manifest has trailing data")
			}
		case "database.bin":
			if seenDatabase || header.Size < 0 {
				return StateManifest{}, errors.New("archive database is duplicated or invalid")
			}
			seenDatabase = true
			hasher := sha256.New()
			limited := io.LimitReader(tarReader, header.Size)
			if db == nil {
				if _, err := io.Copy(hasher, limited); err != nil {
					return StateManifest{}, err
				}
			} else if err := decodeDatabaseReader(io.TeeReader(limited, hasher), header.Size, db); err != nil {
				return StateManifest{}, err
			}
			databaseSum = hasher.Sum(nil)
		default:
			return StateManifest{}, fmt.Errorf("unsupported archive entry %q", header.Name)
		}
	}
	if !seenManifest || !seenDatabase {
		return StateManifest{}, errors.New("archive manifest or database is missing")
	}
	if manifest.Format != StateArchiveFormat {
		return StateManifest{}, fmt.Errorf("unsupported state format %q; recreate a %s archive", manifest.Format, StateArchiveFormat)
	}
	if manifest.ConsensusMode != "synthetic" {
		return StateManifest{}, errors.New("archive has an incompatible consensus mode")
	}
	if hex.EncodeToString(databaseSum) != manifest.DatabaseSHA256 {
		return StateManifest{}, errors.New("state archive checksum mismatch")
	}
	return manifest, nil
}

func validateArchiveDatabase(db ethdb.Database, manifest StateManifest) error {
	if manifest.Secrets {
		return errors.New("archive manifest unexpectedly declares secrets")
	}
	if prepared, err := db.Has(journalKey); err != nil {
		return err
	} else if prepared {
		return errors.New("archive database contains an unfinished prepared operation")
	}
	version, err := db.Get(stateSchemaKey)
	if err != nil || len(version) != 8 || binary.BigEndian.Uint64(version) != currentMetadataFormat {
		return errors.New("archive database does not contain metadata schema v3")
	}
	genesisHash := rawdb.ReadCanonicalHash(db, 0)
	if genesisHash == (common.Hash{}) || rawdb.ReadBlock(db, genesisHash, 0) == nil || genesisHash.Hex() != manifest.GenesisHash {
		return errors.New("archive manifest genesis does not match database")
	}
	chainConfig := rawdb.ReadChainConfig(db, genesisHash)
	if chainConfig == nil || chainConfig.ChainID == nil || chainConfig.ChainID.BitLen() > 64 ||
		chainConfig.ChainID.Uint64() != manifest.ChainID {
		return errors.New("archive manifest chain ID does not match database")
	}
	headHash := rawdb.ReadHeadBlockHash(db)
	if headHash == (common.Hash{}) || headHash.Hex() != manifest.HeadHash {
		return errors.New("archive manifest head does not match database")
	}
	number, exists := rawdb.ReadHeaderNumber(db, headHash)
	if !exists || number != manifest.HeadNumber {
		return errors.New("archive manifest head number does not match database")
	}
	head := rawdb.ReadBlock(db, headHash, manifest.HeadNumber)
	if head == nil || rawdb.ReadCanonicalHash(db, manifest.HeadNumber) != headHash {
		return errors.New("archive database canonical head is incomplete")
	}
	if len(head.Transactions()) != 0 && rawdb.ReadRawReceipts(db, headHash, manifest.HeadNumber) == nil {
		return errors.New("archive database canonical head receipts are missing")
	}
	var timeline storedTimeline
	encoded, err := db.Get(timelineKey)
	if err != nil || json.Unmarshal(encoded, &timeline) != nil {
		return errors.New("archive database timeline metadata is invalid")
	}
	if timeline.GenesisHash != genesisHash || timeline.Complete != manifest.TimelineComplete {
		return errors.New("archive manifest timeline does not match database")
	}
	var session storedSessionSafety
	encoded, err = db.Get(sessionSafetyKey)
	if err != nil || json.Unmarshal(encoded, &session) != nil {
		return errors.New("archive database session safety metadata is invalid")
	}
	reasons := append([]string(nil), session.Reasons...)
	slices.Sort(reasons)
	if session.Tainted != manifest.Tainted || !slices.Equal(reasons, manifest.TaintReasons) {
		return errors.New("archive manifest session safety does not match database")
	}
	var headSafety BlockSafety
	encoded, err = db.Get(hashKey(blockSafetyPrefix, headHash))
	if err != nil || json.Unmarshal(encoded, &headSafety) != nil || headSafety.BlockHash != headHash ||
		headSafety.Tainted != manifest.HeadTainted {
		return errors.New("archive manifest head safety does not match database")
	}
	iterator := db.NewIterator(eventNamespace, nil)
	defer iterator.Release()
	var revision uint64
	for iterator.Next() {
		var event Event
		if err := json.Unmarshal(iterator.Value(), &event); err != nil {
			return errors.New("archive database contains an invalid event")
		}
		revision = max(revision, uint64(event.Revision))
	}
	if err := iterator.Error(); err != nil {
		return err
	}
	if revision != manifest.Revision {
		return errors.New("archive manifest revision does not match database")
	}
	return nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close() //nolint:errcheck
	return directory.Sync()
}

func readArchive(path string, includeDatabase bool) (StateManifest, []byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return StateManifest{}, nil, err
	}
	defer file.Close() // nolint:errcheck
	zstdReader, err := zstd.NewReader(file)
	if err != nil {
		return StateManifest{}, nil, err
	}
	defer zstdReader.Close()
	tarReader := tar.NewReader(zstdReader)
	var manifest StateManifest
	var database []byte
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return StateManifest{}, nil, err
		}
		if header.Size > 1<<32 {
			return StateManifest{}, nil, errors.New("archive entry exceeds resource limit")
		}
		switch header.Name {
		case "manifest.json":
			if err := json.NewDecoder(io.LimitReader(tarReader, header.Size)).Decode(&manifest); err != nil {
				return StateManifest{}, nil, err
			}
		case "database.bin":
			if includeDatabase {
				database, err = io.ReadAll(io.LimitReader(tarReader, header.Size))
				if err != nil {
					return StateManifest{}, nil, err
				}
			}
		}
	}
	if manifest.Format != StateArchiveFormat {
		return StateManifest{}, nil, fmt.Errorf("unsupported or missing state format %q", manifest.Format)
	}
	if manifest.ConsensusMode != "synthetic" {
		return StateManifest{}, nil, errors.New("archive predates the current in-place state layout")
	}
	if includeDatabase {
		if database == nil {
			return StateManifest{}, nil, errors.New("archive database is missing")
		}
		sum := sha256.Sum256(database)
		if hex.EncodeToString(sum[:]) != manifest.DatabaseSHA256 {
			return StateManifest{}, nil, errors.New("state archive checksum mismatch")
		}
	}
	return manifest, database, nil
}
