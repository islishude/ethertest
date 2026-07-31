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
	"time"

	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/klauspost/compress/zstd"
)

const StateArchiveFormat = "ethertest-state-v1"

type StateManifest struct {
	Format         string `json:"format"`
	Version        string `json:"version"`
	CreatedAt      string `json:"created_at"`
	ChainID        uint64 `json:"chain_id"`
	GenesisHash    string `json:"genesis_hash"`
	HeadHash       string `json:"head_hash"`
	HeadNumber     uint64 `json:"head_number"`
	Revision       uint64 `json:"revision"`
	DatabaseSHA256 string `json:"database_sha256"`
	Secrets        bool   `json:"secrets"`
	Tainted        bool   `json:"tainted"`
}

func (n *Node) DumpState(path string) error {
	if n.running.Load() {
		_, err := n.execute(context.Background(), func(_ *executionChain) (any, error) {
			return nil, n.dumpState(path)
		})
		return err
	}
	return n.dumpState(path)
}

func (n *Node) dumpState(path string) error {
	n.chain.mu.RLock()
	defer n.chain.mu.RUnlock()
	database, err := encodeDatabase(n.chain.db)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(database)
	genesis := n.chain.blockchain.GetBlockByNumber(0)
	head := n.chain.blockchain.CurrentBlock()
	manifest := StateManifest{
		Format: StateArchiveFormat, Version: Version, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ChainID: n.cfg.Chain.ChainID, GenesisHash: genesis.Hash().Hex(),
		HeadHash: head.Hash().Hex(), HeadNumber: head.Number.Uint64(),
		Revision: uint64(n.Revision()), DatabaseSHA256: hex.EncodeToString(sum[:]),
	}
	if err := writeArchiveAtomic(path, manifest, database); err != nil {
		return err
	}
	n.logger.Info("state archive written",
		"event", "state_archive_written",
		"path", path,
		"head_number", manifest.HeadNumber,
		"head_hash", manifest.HeadHash,
		"revision", manifest.Revision,
		"database_bytes", len(database),
	)
	return nil
}

func InspectState(path string) (StateManifest, error) {
	manifest, _, err := readArchive(path, true)
	return manifest, err
}

// LoadState replaces an empty Pebble destination with a verified archive.
func LoadState(path, destination string) error {
	_, database, err := readArchive(path, true)
	if err != nil {
		return err
	}
	if destination == "" {
		return errors.New("destination is required")
	}
	if entries, err := os.ReadDir(destination); err == nil && len(entries) != 0 {
		return errors.New("destination must be empty")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	kv, err := pebble.New(destination, 64, 64, "ethertest-load", false)
	if err != nil {
		return err
	}
	db := rawdb.NewDatabase(kv)
	if err := decodeDatabase(database, db); err != nil {
		_ = db.Close() // nolint:errcheck
		return err
	}
	return db.Close()
}

func MigrateState(path string) error {
	manifest, database, err := readArchive(path, true)
	if err != nil {
		return err
	}
	if manifest.Format != StateArchiveFormat {
		return fmt.Errorf("unsupported state format %q", manifest.Format)
	}
	// v1 is already current. Rewriting is intentionally idempotent.
	return writeArchiveAtomic(path, manifest, database)
}

func encodeDatabase(db ethdb.Database) ([]byte, error) {
	var out bytes.Buffer
	writer := bufio.NewWriter(&out)
	iterator := db.NewIterator(nil, nil)
	defer iterator.Release()
	var lengths [8]byte
	for iterator.Next() {
		key, value := iterator.Key(), iterator.Value()
		binary.BigEndian.PutUint32(lengths[:4], uint32(len(key)))
		binary.BigEndian.PutUint32(lengths[4:], uint32(len(value)))
		if _, err := writer.Write(lengths[:]); err != nil {
			return nil, err
		}
		if _, err := writer.Write(key); err != nil {
			return nil, err
		}
		if _, err := writer.Write(value); err != nil {
			return nil, err
		}
	}
	if err := iterator.Error(); err != nil {
		return nil, err
	}
	if err := writer.Flush(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func decodeDatabase(data []byte, db ethdb.Database) error {
	reader := bytes.NewReader(data)
	batch := db.NewBatch()
	var lengths [8]byte
	for reader.Len() != 0 {
		if _, err := io.ReadFull(reader, lengths[:]); err != nil {
			return errors.New("corrupt database record header")
		}
		keySize := binary.BigEndian.Uint32(lengths[:4])
		valueSize := binary.BigEndian.Uint32(lengths[4:])
		if uint64(keySize)+uint64(valueSize) > uint64(reader.Len()) {
			return errors.New("corrupt database record length")
		}
		key, value := make([]byte, keySize), make([]byte, valueSize)
		_, _ = io.ReadFull(reader, key)
		_, _ = io.ReadFull(reader, value)
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
