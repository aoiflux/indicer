package fio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const (
	ChunksPerBlock       = 1000 // Soft flush threshold
	BlockIDPrefixBytes   = 1    // Bytes of hash used as block bucket ID (256 buckets)
	BlockIndexExt        = ".bidx"
	HashLength           = 64
	ChonkNamespaceLength = 5 // Length of "C|||:"

	// v2 file format constants.
	blockMagic      = "BIDX"
	blockVersion    = uint8(2)
	blockHeaderSize = 9  // 4 magic + 1 version + 4 count (uint32)
	blockRecordSize = 86 // 64 hash + 8 offset + 8 size + 4 pathOffset + 2 pathLen
)

// BlockManager manages hierarchical chunk metadata in blocks.
type BlockManager struct {
	mutex        sync.Mutex
	dbpath       string
	currentBlock *Block
	blockFiles   map[string]*os.File // kept for interface compat; not used in v2
	containerMgr *ContainerManager
}

// Block represents a group of chunk metadata entries.
type Block struct {
	blockID    string
	chunkCount int
	metadata   []ChunkMetadata
	filePath   string
}

// ChunkMetadata stores information about a single chunk.
type ChunkMetadata struct {
	chunkHash     [64]byte
	containerPath string
	offset        int64
	size          int64
}

// blockRecord is the in-memory representation of a v2 index entry.
type blockRecord struct {
	hash       [64]byte
	offset     int64
	size       int64
	pathOffset uint32
	pathLen    uint16
	path       string
}

// NewBlockManager creates a new block index manager.
func NewBlockManager(dbpath string, containerMgr *ContainerManager) *BlockManager {
	return &BlockManager{
		dbpath:       dbpath,
		blockFiles:   make(map[string]*os.File),
		containerMgr: containerMgr,
	}
}

// AddChunkMetadata adds chunk metadata to the appropriate block.
func (bm *BlockManager) AddChunkMetadata(chunkHash []byte, containerPath string, offset, size int64) error {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	actualHash := chunkHash[ChonkNamespaceLength:]
	blockID := bm.getBlockID(actualHash)

	if bm.currentBlock == nil || bm.currentBlock.blockID != blockID {
		if bm.currentBlock != nil && bm.currentBlock.chunkCount > 0 {
			if err := bm.flushBlock(bm.currentBlock); err != nil {
				return err
			}
		}
		bm.currentBlock = bm.getOrCreateBlock(blockID)
	}

	var hashArray [64]byte
	copy(hashArray[:], actualHash)

	bm.currentBlock.metadata = append(bm.currentBlock.metadata, ChunkMetadata{
		chunkHash:     hashArray,
		containerPath: containerPath,
		offset:        offset,
		size:          size,
	})
	bm.currentBlock.chunkCount++

	if bm.currentBlock.chunkCount >= ChunksPerBlock {
		if err := bm.flushBlock(bm.currentBlock); err != nil {
			return err
		}
		bm.currentBlock = nil
	}

	return nil
}

// GetChunkMetadata retrieves chunk metadata using binary search in the v2 block file.
func (bm *BlockManager) GetChunkMetadata(chunkHash []byte) (string, int64, int64, error) {
	actualHash := chunkHash[ChonkNamespaceLength:]
	blockID := bm.getBlockID(actualHash)
	blockFilePath := bm.getBlockFilePath(blockID)

	if _, err := os.Stat(blockFilePath); os.IsNotExist(err) {
		return "", 0, 0, fmt.Errorf("block file not found for block %s", blockID)
	}

	file, err := os.Open(blockFilePath)
	if err != nil {
		return "", 0, 0, err
	}
	defer file.Close()

	return searchBlockFileBinary(file, actualHash)
}

// searchBlockFileBinary performs O(log n) binary search in a v2 block file.
// The index section contains fixed-size records sorted by chunkHash.
func searchBlockFileBinary(file *os.File, targetHash []byte) (string, int64, int64, error) {
	var target [HashLength]byte
	copy(target[:], targetHash)

	header := make([]byte, blockHeaderSize)
	if _, err := io.ReadFull(file, header); err != nil {
		return "", 0, 0, fmt.Errorf("read block header: %w", err)
	}
	if string(header[0:4]) != blockMagic {
		return "", 0, 0, fmt.Errorf("block file has unknown format (expected %q magic); re-ingest required", blockMagic)
	}
	if header[4] != blockVersion {
		return "", 0, 0, fmt.Errorf("unsupported block file version %d", header[4])
	}
	count := int(binary.LittleEndian.Uint32(header[5:9]))
	if count == 0 {
		return "", 0, 0, fmt.Errorf("chunk not found in block")
	}

	buf := make([]byte, blockRecordSize)
	lo, hi := 0, count-1
	for lo <= hi {
		mid := (lo + hi) / 2
		seekPos := int64(blockHeaderSize + mid*blockRecordSize)
		if _, err := file.Seek(seekPos, io.SeekStart); err != nil {
			return "", 0, 0, err
		}
		if _, err := io.ReadFull(file, buf); err != nil {
			return "", 0, 0, err
		}

		cmp := bytes.Compare(buf[0:64], target[:])
		switch {
		case cmp == 0:
			offset := int64(binary.LittleEndian.Uint64(buf[64:72]))
			size := int64(binary.LittleEndian.Uint64(buf[72:80]))
			pathOffset := uint32(binary.LittleEndian.Uint32(buf[80:84]))
			pathLen := uint16(binary.LittleEndian.Uint16(buf[84:86]))

			stringTableStart := int64(blockHeaderSize + count*blockRecordSize)
			if _, err := file.Seek(stringTableStart+int64(pathOffset), io.SeekStart); err != nil {
				return "", 0, 0, err
			}
			pathBytes := make([]byte, pathLen)
			if _, err := io.ReadFull(file, pathBytes); err != nil {
				return "", 0, 0, err
			}
			return string(pathBytes), offset, size, nil

		case cmp < 0:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return "", 0, 0, fmt.Errorf("chunk not found in block")
}

// flushBlock sorts, deduplicates, merges with any existing on-disk records, and
// writes a new v2 block file atomically (temp file → sync → rename).
func (bm *BlockManager) flushBlock(block *Block) error {
	if block == nil || block.chunkCount == 0 {
		return nil
	}

	blockDir := filepath.Join(util.BlobPath(bm.dbpath), "blocks")
	if err := os.MkdirAll(blockDir, cnst.DirPerm); err != nil {
		return err
	}

	blockFilePath := bm.getBlockFilePath(block.blockID)

	// Read existing on-disk records so we perform a merge-sort, keeping a
	// single sorted file per block ID rather than growing an append log.
	var existing []blockRecord
	if _, err := os.Stat(blockFilePath); err == nil {
		existing, _ = readBlockFileRecords(blockFilePath)
		// Ignore read errors for old-format files; they will be overwritten.
	}

	// Build in-memory records from the new metadata batch.
	fresh := make([]blockRecord, len(block.metadata))
	for i, meta := range block.metadata {
		fresh[i] = blockRecord{
			hash:   meta.chunkHash,
			offset: meta.offset,
			size:   meta.size,
			path:   meta.containerPath,
		}
	}

	// Merge, sort by hash, deduplicate (last writer wins for duplicate hashes).
	all := append(existing, fresh...)
	sort.Slice(all, func(i, j int) bool {
		return bytes.Compare(all[i].hash[:], all[j].hash[:]) < 0
	})
	deduped := all[:0]
	for i, rec := range all {
		if i == 0 || rec.hash != all[i-1].hash {
			deduped = append(deduped, rec)
		}
	}

	// Build the string table and assign path offsets.
	var stringTable []byte
	for i := range deduped {
		deduped[i].pathOffset = uint32(len(stringTable))
		deduped[i].pathLen = uint16(len(deduped[i].path))
		stringTable = append(stringTable, []byte(deduped[i].path)...)
	}

	// Write to a temporary file alongside the target, then atomically rename.
	tmpPath := blockFilePath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	writeErr := func() error {
		// Header: magic (4) + version (1) + count (4)
		if _, err := f.Write([]byte(blockMagic)); err != nil {
			return err
		}
		if err := binary.Write(f, binary.LittleEndian, blockVersion); err != nil {
			return err
		}
		if err := binary.Write(f, binary.LittleEndian, uint32(len(deduped))); err != nil {
			return err
		}

		// Index section: N × 86-byte fixed records (sorted by hash).
		for _, rec := range deduped {
			if _, err := f.Write(rec.hash[:]); err != nil {
				return err
			}
			if err := binary.Write(f, binary.LittleEndian, rec.offset); err != nil {
				return err
			}
			if err := binary.Write(f, binary.LittleEndian, rec.size); err != nil {
				return err
			}
			if err := binary.Write(f, binary.LittleEndian, rec.pathOffset); err != nil {
				return err
			}
			if err := binary.Write(f, binary.LittleEndian, rec.pathLen); err != nil {
				return err
			}
		}

		// String table.
		if len(stringTable) > 0 {
			if _, err := f.Write(stringTable); err != nil {
				return err
			}
		}

		return f.Sync()
	}()

	f.Close()
	if writeErr != nil {
		os.Remove(tmpPath)
		return writeErr
	}

	return os.Rename(tmpPath, blockFilePath)
}

// readBlockFileRecords reads all index records from a v2 block file.
// Returns an error for unknown or corrupt formats.
func readBlockFileRecords(path string) ([]blockRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	header := make([]byte, blockHeaderSize)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("read block header: %w", err)
	}
	if string(header[0:4]) != blockMagic {
		return nil, fmt.Errorf("unknown block file format")
	}
	count := int(binary.LittleEndian.Uint32(header[5:9]))

	records := make([]blockRecord, count)
	indexBuf := make([]byte, blockRecordSize)
	for i := range records {
		if _, err := io.ReadFull(f, indexBuf); err != nil {
			return nil, fmt.Errorf("read record %d: %w", i, err)
		}
		copy(records[i].hash[:], indexBuf[0:64])
		records[i].offset = int64(binary.LittleEndian.Uint64(indexBuf[64:72]))
		records[i].size = int64(binary.LittleEndian.Uint64(indexBuf[72:80]))
		records[i].pathOffset = binary.LittleEndian.Uint32(indexBuf[80:84])
		records[i].pathLen = binary.LittleEndian.Uint16(indexBuf[84:86])
	}

	// Read string table and resolve paths.
	stringTable, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read string table: %w", err)
	}
	for i := range records {
		start := records[i].pathOffset
		end := start + uint32(records[i].pathLen)
		if int(end) > len(stringTable) {
			return nil, fmt.Errorf("corrupt string table at record %d", i)
		}
		records[i].path = string(stringTable[start:end])
	}

	return records, nil
}

// Close flushes any remaining buffered block and closes open file handles.
func (bm *BlockManager) Close() error {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	if bm.currentBlock != nil && bm.currentBlock.chunkCount > 0 {
		if err := bm.flushBlock(bm.currentBlock); err != nil {
			return err
		}
	}

	for _, file := range bm.blockFiles {
		file.Close()
	}

	return nil
}

func (bm *BlockManager) getBlockID(chunkHash []byte) string {
	if len(chunkHash) < BlockIDPrefixBytes {
		return fmt.Sprintf("%x", chunkHash)
	}
	return fmt.Sprintf("%x", chunkHash[:BlockIDPrefixBytes])
}

func (bm *BlockManager) getBlockFilePath(blockID string) string {
	blockDir := filepath.Join(util.BlobPath(bm.dbpath), "blocks")
	return filepath.Join(blockDir, fmt.Sprintf("block_%s%s", blockID, BlockIndexExt))
}

func (bm *BlockManager) getOrCreateBlock(blockID string) *Block {
	return &Block{
		blockID:    blockID,
		chunkCount: 0,
		metadata:   make([]ChunkMetadata, 0, ChunksPerBlock),
		filePath:   bm.getBlockFilePath(blockID),
	}
}
