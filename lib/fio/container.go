package fio

import (
	"crypto/sha3"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const MaxContainerSize = 1 * cnst.GB // 1GB max per container file

// ContainerManager handles writing chunks to container files
type ContainerManager struct {
	mutex            sync.Mutex
	currentContainer string
	currentFile      *os.File
	currentOffset    int64
	dbpath           string
	containerIndex   int
}

// NewContainerManager creates a new container manager
func NewContainerManager(dbpath string) *ContainerManager {
	return &ContainerManager{
		dbpath:         dbpath,
		containerIndex: 0,
	}
}

// WriteChunkToContainer writes a chunk to a container file and returns metadata
func (cm *ContainerManager) WriteChunkToContainer(data, ckey, key []byte) (containerPath string, offset int64, size int64, err error) {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	// Encrypt and compress data
	var processedData []byte
	if !cnst.QUICKOPT {
		processedData = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
		processedData, err = util.SealAES(key, processedData)
		if err != nil {
			return "", 0, 0, err
		}
	} else {
		processedData = data
	}

	dataSize := int64(len(processedData))

	// Check if we need a new container (first time, or current won't fit)
	if cm.currentFile == nil || cm.currentOffset+dataSize > MaxContainerSize {
		err = cm.createNewContainer()
		if err != nil {
			return "", 0, 0, err
		}
	}

	// Write data to current container
	n, err := cm.currentFile.Write(processedData)
	if err != nil {
		return "", 0, 0, err
	}

	// Record current position
	containerPath = cm.currentContainer
	offset = cm.currentOffset
	size = int64(n)

	// Update offset for next write
	cm.currentOffset += size

	return containerPath, offset, size, nil
}

// createNewContainer creates a new container file
func (cm *ContainerManager) createNewContainer() error {
	// Close and compress previous container if open
	if cm.currentFile != nil {
		cm.currentFile.Sync()
		cm.currentFile.Close()

		// Compress the previous container
		if err := cm.compressContainer(cm.currentContainer); err != nil {
			return fmt.Errorf("failed to compress container: %w", err)
		}
	}

	// Generate unique container filename
	cm.containerIndex++
	ckhash, err := util.GetChonkHash([]byte(fmt.Sprintf("container_%d", cm.containerIndex)), sha3.New512())
	if err != nil {
		return err
	}
	cfname := base64.RawURLEncoding.EncodeToString(ckhash)[:cnst.FileNameLen] + cnst.BLOBEXT
	cfpath := filepath.Join(cm.dbpath, cnst.BLOBSDIR, cfname)

	// Ensure BLOBS directory exists
	blobsDir := filepath.Join(cm.dbpath, cnst.BLOBSDIR)
	err = os.MkdirAll(blobsDir, os.ModePerm)
	if err != nil {
		return err
	}

	// Open new container file
	file, err := os.OpenFile(cfpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return err
	}

	cm.currentFile = file
	cm.currentContainer = cfpath
	cm.currentOffset = 0

	return nil
}

// Close closes the current container file
func (cm *ContainerManager) Close() error {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cm.currentFile != nil {
		cm.currentFile.Sync()
		err := cm.currentFile.Close()
		if err != nil {
			return err
		}

		// Compress the final container
		if err := cm.compressContainer(cm.currentContainer); err != nil {
			return fmt.Errorf("failed to compress final container: %w", err)
		}
	}
	return nil
}

// compressContainer compresses a container file using zstd
func (cm *ContainerManager) compressContainer(containerPath string) error {
	if containerPath == "" {
		return nil
	}

	// Skip if already compressed
	if strings.HasSuffix(containerPath, cnst.BLOBZSTEXT) {
		return nil
	}

	// Open source file
	srcFile, err := os.Open(containerPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// Create compressed file
	compressedPath := strings.TrimSuffix(containerPath, cnst.BLOBEXT) + cnst.BLOBZSTEXT
	dstFile, err := os.Create(compressedPath)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Read entire file
	data, err := io.ReadAll(srcFile)
	if err != nil {
		return err
	}

	// Compress data
	compressed := cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))

	// Write compressed data
	_, err = dstFile.Write(compressed)
	if err != nil {
		return err
	}

	// Sync and close
	dstFile.Sync()
	dstFile.Close()
	srcFile.Close()

	// Remove original uncompressed file
	err = os.Remove(containerPath)
	if err != nil {
		return err
	}

	// Update the current container path to point to compressed file
	if cm.currentContainer == containerPath {
		cm.currentContainer = compressedPath
	}

	return nil
}

// ReadChunkFromContainer reads a chunk from a container file at the specified offset
func ReadChunkFromContainer(containerPath string, offset, size int64, key []byte) ([]byte, error) {
	var fileData []byte

	// Check if container is compressed
	if strings.HasSuffix(containerPath, cnst.BLOBZSTEXT) {
		// Read and decompress the entire container
		file, err := os.Open(containerPath)
		if err != nil {
			return nil, err
		}
		defer file.Close()

		compressedData, err := io.ReadAll(file)
		if err != nil {
			return nil, err
		}

		// Decompress
		fileData, err = cnst.DECODER.DecodeAll(compressedData, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress container: %w", err)
		}
	} else {
		// Try compressed extension if uncompressed doesn't exist
		if _, err := os.Stat(containerPath); os.IsNotExist(err) {
			compressedPath := strings.TrimSuffix(containerPath, cnst.BLOBEXT) + cnst.BLOBZSTEXT
			if _, err := os.Stat(compressedPath); err == nil {
				// Found compressed version, use it
				return ReadChunkFromContainer(compressedPath, offset, size, key)
			}
		}

		// Read uncompressed container
		file, err := os.Open(containerPath)
		if err != nil {
			return nil, err
		}
		defer file.Close()

		fileData, err = io.ReadAll(file)
		if err != nil {
			return nil, err
		}
	}

	// Extract chunk at offset
	if offset+size > int64(len(fileData)) {
		return nil, fmt.Errorf("offset %d + size %d exceeds container size %d", offset, size, len(fileData))
	}
	encoded := fileData[offset : offset+size]

	// Decrypt and decompress
	var data []byte
	if !cnst.QUICKOPT {
		decrypted, err := util.UnsealAES(key, encoded)
		if err != nil {
			return nil, err
		}
		data = decrypted

		decoded, err := cnst.DECODER.DecodeAll(data, nil)
		if err != nil {
			return nil, err
		}
		data = decoded
	} else {
		data = encoded
	}

	return data, nil
}
