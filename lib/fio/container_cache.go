package fio

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultMaxCacheSize = 2 * cnst.GB
)

type cachedContainer struct {
	data       []byte
	lastAccess time.Time
}

type containerEntry struct {
	path       string
	lastAccess time.Time
	size       int64
}

var (
	containerReadCacheMu sync.RWMutex
	containerReadCache   = map[string]*cachedContainer{}
	containerDiskCache   = map[string]string{}
	containerCacheOn     bool
	maxCacheSize         int64 = defaultMaxCacheSize
	currentCacheSize     int64
	containerTempDir     string
)

func EnableContainerReadCache() {
	containerReadCacheMu.Lock()
	containerCacheOn = true
	containerReadCacheMu.Unlock()
}

func SetContainerReadCacheSize(sizeBytes int64) {
	containerReadCacheMu.Lock()
	maxCacheSize = sizeBytes
	containerReadCacheMu.Unlock()
}

func DisableContainerReadCache() {
	containerReadCacheMu.Lock()
	containerCacheOn = false
	containerReadCache = map[string]*cachedContainer{}
	currentCacheSize = 0

	for _, tempPath := range containerDiskCache {
		_ = os.Remove(tempPath)
	}
	containerDiskCache = map[string]string{}

	if containerTempDir != "" {
		_ = os.RemoveAll(containerTempDir)
		containerTempDir = ""
	}
	containerReadCacheMu.Unlock()
}

func readFromCompressedContainer(containerPath string, offset, size int64) ([]byte, error) {
	if data, ok := getCachedDecompressedContainer(containerPath); ok {
		return extractEncodedChunk(data, offset, size)
	}

	decompressedData, err := loadAndCacheDecompressedContainer(containerPath)
	if err != nil {
		return nil, err
	}
	if decompressedData != nil {
		return extractEncodedChunk(decompressedData, offset, size)
	}

	containerReadCacheMu.RLock()
	cacheEnabled := containerCacheOn
	diskCachePath, hasDiskCache := containerDiskCache[containerPath]
	containerReadCacheMu.RUnlock()

	if cacheEnabled {
		if !hasDiskCache {
			diskCachePath, err = decompressContainerToDisk(containerPath)
			if err != nil {
				return nil, err
			}
		}

		encoded := make([]byte, size)
		tempFile, err := os.Open(diskCachePath)
		if err != nil {
			return nil, err
		}
		defer tempFile.Close()

		if _, err = tempFile.ReadAt(encoded, offset); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("offset %d + size %d exceeds container size", offset, size)
			}
			return nil, err
		}
		return encoded, nil
	}

	return streamReadCompressedChunk(containerPath, offset, size)
}

func extractEncodedChunk(decompressedData []byte, offset, size int64) ([]byte, error) {
	if offset+size > int64(len(decompressedData)) {
		return nil, fmt.Errorf("offset %d + size %d exceeds decompressed container size", offset, size)
	}
	encoded := make([]byte, size)
	copy(encoded, decompressedData[offset:offset+size])
	return encoded, nil
}

func streamReadCompressedChunk(containerPath string, offset, size int64) ([]byte, error) {
	file, err := os.Open(containerPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder, err := zstd.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("failed to create decompressor: %w", err)
	}
	defer decoder.Close()

	if offset > 0 {
		if _, err = io.CopyN(io.Discard, decoder, offset); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("offset %d exceeds decompressed container size", offset)
			}
			return nil, err
		}
	}

	encoded := make([]byte, size)
	if _, err = io.ReadFull(decoder, encoded); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("offset %d + size %d exceeds decompressed container size", offset, size)
		}
		return nil, err
	}
	return encoded, nil
}

func getCachedDecompressedContainer(containerPath string) ([]byte, bool) {
	containerReadCacheMu.Lock()
	defer containerReadCacheMu.Unlock()

	if !containerCacheOn {
		return nil, false
	}
	cached, ok := containerReadCache[containerPath]
	if !ok {
		return nil, false
	}

	cached.lastAccess = time.Now()
	return cached.data, true
}

func ensureTempDir() error {
	if containerTempDir == "" {
		tempDir, err := os.MkdirTemp("", "indicer-containers-*")
		if err != nil {
			return fmt.Errorf("failed to create temp directory: %w", err)
		}
		containerTempDir = tempDir
	}
	return nil
}

func decompressContainerToDisk(containerPath string) (string, error) {
	containerReadCacheMu.Lock()
	defer containerReadCacheMu.Unlock()

	if tempPath, ok := containerDiskCache[containerPath]; ok {
		return tempPath, nil
	}

	if err := ensureTempDir(); err != nil {
		return "", err
	}

	srcFile, err := os.Open(containerPath)
	if err != nil {
		return "", err
	}
	defer srcFile.Close()

	base := filepath.Base(containerPath)
	tempFile, err := os.CreateTemp(containerTempDir, base+"-decompressed-*")
	if err != nil {
		return "", err
	}
	tempPath := tempFile.Name()
	defer tempFile.Close()

	decoder, err := zstd.NewReader(srcFile)
	if err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("failed to create decompressor: %w", err)
	}
	defer decoder.Close()

	if _, err = io.Copy(tempFile, decoder); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("failed to decompress to disk: %w", err)
	}

	if err = tempFile.Sync(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}

	containerDiskCache[containerPath] = tempPath
	return tempPath, nil
}

func loadAndCacheDecompressedContainer(containerPath string) ([]byte, error) {
	containerReadCacheMu.RLock()
	if !containerCacheOn {
		containerReadCacheMu.RUnlock()
		return nil, nil
	}
	containerReadCacheMu.RUnlock()

	file, err := os.Open(containerPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	compressedData, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}

	decompressedData, err := cnst.DECODER.DecodeAll(compressedData, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress container: %w", err)
	}

	containerReadCacheMu.Lock()
	defer containerReadCacheMu.Unlock()

	if !containerCacheOn {
		return nil, nil
	}
	if existing, ok := containerReadCache[containerPath]; ok {
		return existing.data, nil
	}

	neededSpace := int64(len(decompressedData))
	if neededSpace > maxCacheSize {
		return nil, nil
	}

	evictLRUContainers(neededSpace)
	containerReadCache[containerPath] = &cachedContainer{data: decompressedData, lastAccess: time.Now()}
	currentCacheSize += neededSpace

	return decompressedData, nil
}

func evictLRUContainers(neededSpace int64) {
	if currentCacheSize+neededSpace <= maxCacheSize {
		return
	}

	entries := make([]containerEntry, 0, len(containerReadCache))
	for path, cached := range containerReadCache {
		entries = append(entries, containerEntry{path: path, lastAccess: cached.lastAccess, size: int64(len(cached.data))})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastAccess.Before(entries[j].lastAccess)
	})

	for _, entry := range entries {
		if currentCacheSize+neededSpace <= maxCacheSize {
			break
		}
		delete(containerReadCache, entry.path)
		currentCacheSize -= entry.size
	}
}
