package fio

import (
	"encoding/binary"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	ChunksPerBlock       = 1000             // Number of chunks per block
	BlockIDPrefixBytes   = 1                // Use first 2 bytes
	BlockIndexExt        = ".bidx"          // Block index file extension
	HashLength           = 64               // SHA3-512 hash length in bytes
	ChunkMetadataSize    = 64 + 8 + 8 + 256 // hash(64) + offset(8) + size(8) + path(256)
	ChonkNamespaceLength = 5                // Length of "C|||:" in bytes
)

// BlockManager manages hierarchical chunk metadata in blocks
type BlockManager struct {
	mutex        sync.Mutex
	dbpath       string
	currentBlock *Block
	blockFiles   map[string]*os.File // Block ID (hex string) -> file handle
	containerMgr *ContainerManager
}

// Block represents a group of chunk metadata entries
type Block struct {
	blockID    string // Full hash in hex format (128 chars)
	chunkCount int
	metadata   []ChunkMetadata
	filePath   string
}

// ChunkMetadata stores information about a single chunk
type ChunkMetadata struct {
	chunkHash     [64]byte // SHA3-512 hash (64 bytes)
	containerPath string
	offset        int64
	size          int64
}

// NewBlockManager creates a new block index manager
func NewBlockManager(dbpath string, containerMgr *ContainerManager) *BlockManager {
	return &BlockManager{
		dbpath:       dbpath,
		blockFiles:   make(map[string]*os.File),
		containerMgr: containerMgr,
	}
}

// AddChunkMetadata adds chunk metadata to the appropriate block
func (bm *BlockManager) AddChunkMetadata(chunkHash []byte, containerPath string, offset, size int64) error {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	// Extract actual chunk hash from the database key (skip namespace prefix)
	// Key format: "C|||:" (5 bytes) + actual_hash (64 bytes)
	actualHash := chunkHash[ChonkNamespaceLength:]

	// Determine block ID from full hash
	blockID := bm.getBlockID(actualHash)

	// Get or create block
	if bm.currentBlock == nil || bm.currentBlock.blockID != blockID {
		if bm.currentBlock != nil && bm.currentBlock.chunkCount > 0 {
			// Flush current block if it's from a different blockID
			if err := bm.flushBlock(bm.currentBlock); err != nil {
				return err
			}
		}
		bm.currentBlock = bm.getOrCreateBlock(blockID)
	}

	// Add metadata to current block
	var hashArray [64]byte
	copy(hashArray[:], actualHash)

	metadata := ChunkMetadata{
		chunkHash:     hashArray,
		containerPath: containerPath,
		offset:        offset,
		size:          size,
	}

	bm.currentBlock.metadata = append(bm.currentBlock.metadata, metadata)
	bm.currentBlock.chunkCount++

	// Flush if block is full
	if bm.currentBlock.chunkCount >= ChunksPerBlock {
		if err := bm.flushBlock(bm.currentBlock); err != nil {
			return err
		}
		bm.currentBlock = nil
	}

	return nil
}

// GetChunkMetadata retrieves chunk metadata using two-level lookup
func (bm *BlockManager) GetChunkMetadata(chunkHash []byte) (string, int64, int64, error) {
	// Extract actual chunk hash from the database key (skip namespace prefix)
	// Key format: "C|||:" (5 bytes) + actual_hash (64 bytes)
	actualHash := chunkHash[ChonkNamespaceLength:]

	blockID := bm.getBlockID(actualHash)

	// Get block file path
	blockFilePath := bm.getBlockFilePath(blockID)

	// Check if block file exists
	if _, err := os.Stat(blockFilePath); os.IsNotExist(err) {
		return "", 0, 0, fmt.Errorf("block file not found for block %s", blockID)
	}

	// Open and search block file
	file, err := os.Open(blockFilePath)
	if err != nil {
		return "", 0, 0, err
	}
	defer file.Close()

	return bm.searchBlockFile(file, actualHash)
}

// searchBlockFile performs sequential search within a block file
func (bm *BlockManager) searchBlockFile(file *os.File, targetHash []byte) (string, int64, int64, error) {
	for {
		// Read chunk hash (64 bytes)
		var storedHash [64]byte
		if _, err := io.ReadFull(file, storedHash[:]); err != nil {
			if err == io.EOF {
				return "", 0, 0, fmt.Errorf("chunk not found in block")
			}
			return "", 0, 0, err
		}

		// Read offset (8 bytes)
		var offset int64
		if err := binary.Read(file, binary.LittleEndian, &offset); err != nil {
			return "", 0, 0, err
		}

		// Read size (8 bytes)
		var size int64
		if err := binary.Read(file, binary.LittleEndian, &size); err != nil {
			return "", 0, 0, err
		}

		// Read container path length (2 bytes)
		var pathLen uint16
		if err := binary.Read(file, binary.LittleEndian, &pathLen); err != nil {
			return "", 0, 0, err
		}

		// Read container path
		pathBytes := make([]byte, pathLen)
		if _, err := io.ReadFull(file, pathBytes); err != nil {
			return "", 0, 0, err
		}
		containerPath := string(pathBytes)

		// Check if this is the target chunk
		if bytesEqual(storedHash[:], targetHash) {
			return containerPath, offset, size, nil
		}
	}
}

// flushBlock writes block metadata to disk
func (bm *BlockManager) flushBlock(block *Block) error {
	if block == nil || block.chunkCount == 0 {
		return nil
	}

	// Ensure block directory exists
	blockDir := filepath.Join(bm.dbpath, cnst.BLOBSDIR, "blocks")
	if err := os.MkdirAll(blockDir, os.ModePerm); err != nil {
		return err
	}

	// Open or create block file
	blockFilePath := bm.getBlockFilePath(block.blockID)
	file, err := os.OpenFile(blockFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return err
	}
	defer file.Close()

	// Write each chunk metadata
	for _, meta := range block.metadata {
		// Write hash (64 bytes)
		if _, err := file.Write(meta.chunkHash[:]); err != nil {
			return err
		}

		// Write offset (8 bytes)
		if err := binary.Write(file, binary.LittleEndian, meta.offset); err != nil {
			return err
		}

		// Write size (8 bytes)
		if err := binary.Write(file, binary.LittleEndian, meta.size); err != nil {
			return err
		}

		// Write container path length (2 bytes)
		pathLen := uint16(len(meta.containerPath))
		if err := binary.Write(file, binary.LittleEndian, pathLen); err != nil {
			return err
		}

		// Write container path
		if _, err := file.Write([]byte(meta.containerPath)); err != nil {
			return err
		}
	}

	return file.Sync()
}

// Close flushes any remaining blocks and closes files
func (bm *BlockManager) Close() error {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()

	// Flush current block
	if bm.currentBlock != nil && bm.currentBlock.chunkCount > 0 {
		if err := bm.flushBlock(bm.currentBlock); err != nil {
			return err
		}
	}

	// Close all block files
	for _, file := range bm.blockFiles {
		if err := file.Close(); err != nil {
			return err
		}
	}

	return nil
}

// getBlockID derives block ID from hash prefix (not full hash)
func (bm *BlockManager) getBlockID(chunkHash []byte) string {
	// Use only first N bytes of hash to create block ID
	// This allows multiple chunks to map to same block file
	// Example: first 4 bytes = 8 hex chars = ~4B possible blocks
	if len(chunkHash) < BlockIDPrefixBytes {
		return fmt.Sprintf("%x", chunkHash)
	}
	return fmt.Sprintf("%x", chunkHash[:BlockIDPrefixBytes])
}

// getBlockFilePath returns the file path for a block
func (bm *BlockManager) getBlockFilePath(blockID string) string {
	blockDir := filepath.Join(bm.dbpath, cnst.BLOBSDIR, "blocks")
	// blockID is already the hash prefix, use it directly
	fileName := fmt.Sprintf("block_%s%s", blockID, BlockIndexExt)
	return filepath.Join(blockDir, fileName)
}

// getOrCreateBlock gets or creates a block for the given ID
func (bm *BlockManager) getOrCreateBlock(blockID string) *Block {
	return &Block{
		blockID:    blockID,
		chunkCount: 0,
		metadata:   make([]ChunkMetadata, 0, ChunksPerBlock),
		filePath:   bm.getBlockFilePath(blockID),
	}
}

// bytesEqual compares two byte slices
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
