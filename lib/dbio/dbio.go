package dbio

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/vmihailenco/msgpack/v5"
)

func ConnectDB(datadir string, key []byte) (*badger.DB, error) {
	cacheLimit, err := cnst.GetCacheLimit()
	if err != nil {
		cacheLimit = 256 * cnst.MB
	}

	// Treat cacheLimit as the total cache budget and split it between
	// block/index caches so larger-memory machines still get large caches
	// without accidentally allocating the same full budget twice.
	blockCache := (cacheLimit * 3) / 4
	indexCache := cacheLimit - blockCache
	if blockCache < 64*cnst.MB {
		blockCache = 64 * cnst.MB
	}
	if indexCache < 32*cnst.MB {
		indexCache = 32 * cnst.MB
	}

	opts := badger.DefaultOptions(datadir)
	opts = opts.WithLoggingLevel(badger.ERROR)
	opts.IndexCacheSize = indexCache
	opts.SyncWrites = true
	opts.NumGoroutines = cnst.GetMaxThreadCount()
	if !cnst.QUICKOPT {
		opts.Compression = options.ZSTD
		opts.ZSTDCompressionLevel = 15
		opts.EncryptionKey = key
		opts.EncryptionKeyRotationDuration = time.Hour * 168
	}
	opts.CompactL0OnClose = true
	opts.LmaxCompaction = true
	opts.NumCompactors = opts.NumGoroutines
	opts.BlockCacheSize = blockCache
	opts.IndexCacheSize = indexCache
	opts.ValueLogFileSize = 64 << 20
	opts.ValueLogMaxEntries = uint32(opts.NumGoroutines)

	return badger.Open(opts)
}

func SetFile[T structs.FileTypes](id []byte, filenode T, db *badger.DB) error {
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		return err
	}
	return SetNode(id, data, db)
}
func SetIndexedFile(id []byte, filenode structs.IndexedFile, batch *badger.WriteBatch) error {
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		return err
	}
	return SetBatchNode(id, data, batch)
}

func GetEvidenceFile(key []byte, db *badger.DB) (structs.EvidenceFile, error) {
	var evidenceFile structs.EvidenceFile

	data, err := GetNode(key, db)
	if err != nil {
		return evidenceFile, err
	}

	err = msgpack.Unmarshal(data, &evidenceFile)
	return evidenceFile, err
}
func GetPartitionFile(key []byte, db *badger.DB) (structs.PartitionFile, error) {
	var partitionFile structs.PartitionFile

	data, err := GetNode(key, db)
	if err != nil {
		return partitionFile, err
	}

	err = msgpack.Unmarshal(data, &partitionFile)
	return partitionFile, err
}
func GetIndexedFile(key []byte, db *badger.DB) (structs.IndexedFile, error) {
	var indexedFile structs.IndexedFile

	data, err := GetNode(key, db)
	if err != nil {
		return indexedFile, err
	}

	err = msgpack.Unmarshal(data, &indexedFile)
	return indexedFile, err
}

func SetReverseRelationNode(key []byte, revRelNode map[string]struct{}, batch *badger.WriteBatch) error {
	data, err := msgpack.Marshal(revRelNode)
	if err != nil {
		return err
	}
	return SetBatchNode(key, data, batch)
}
func GetReverseRelationNode(key []byte, db *badger.DB) (map[string]struct{}, error) {
	var reverseRelations map[string]struct{}

	data, err := GetNode(key, db)
	if err != nil {
		return nil, err
	}

	err = msgpack.Unmarshal(data, &reverseRelations)
	return reverseRelations, err
}

func SetBatchChonkSignature(chash []byte, signature uint64, batch *badger.WriteBatch) error {
	key := util.AppendToBytesSlice(cnst.ChonkSimhashNamespace, chash)
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, signature)
	return SetBatchNode(key, data, batch)
}

func SetChonkSignature(chash []byte, signature uint64, db *badger.DB) error {
	key := util.AppendToBytesSlice(cnst.ChonkSimhashNamespace, chash)
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, signature)
	return SetNode(key, data, db)
}

func GetChonkSignature(chash []byte, db *badger.DB) (uint64, error) {
	key := util.AppendToBytesSlice(cnst.ChonkSimhashNamespace, chash)
	data, err := GetNode(key, db)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("invalid chunk signature length: %d", len(data))
	}
	return binary.BigEndian.Uint64(data), nil
}

// SetFileSimhash stores the full-file Phase 2 SimHash for fid so that subsequent
// --advanced-deep runs can skip re-streaming all chunk bytes.
// Key: F|||: + fid  (fid already carries its own namespace prefix, e.g. E|||:…)
func SetFileSimhash(fid []byte, sig uint64, db *badger.DB) error {
	key := util.AppendToBytesSlice(cnst.FileSimhashNamespace, fid)
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, sig)
	return SetNode(key, data, db)
}

// GetFileSimhash retrieves the cached full-file Phase 2 SimHash for fid.
// Returns badger.ErrKeyNotFound when no cached value exists yet.
func GetFileSimhash(fid []byte, db *badger.DB) (uint64, error) {
	key := util.AppendToBytesSlice(cnst.FileSimhashNamespace, fid)
	data, err := GetNode(key, db)
	if err != nil {
		return 0, err
	}
	if len(data) != 8 {
		return 0, fmt.Errorf("invalid file simhash length: %d", len(data))
	}
	return binary.BigEndian.Uint64(data), nil
}

func SetBatchChonkNode(key, data []byte, db *badger.DB, batch *badger.WriteBatch, containerMgr *fio.ContainerManager, blockMgr *fio.BlockManager) error {
	if containerMgr != nil {
		// Container mode: pack chunks into containers
		containerPath, offset, size, err := containerMgr.WriteChunkToContainer(data, key, db.Opts().EncryptionKey)
		if err != nil {
			return err
		}

		// If hierarchical mode, add to block manager instead of DB
		if blockMgr != nil {
			// Hierarchical mode: store in block index
			return blockMgr.AddChunkMetadata(key, containerPath, offset, size)
		}

		// Regular container mode: store metadata in DB
		metadata := []byte(fmt.Sprintf("%s|%d|%d", containerPath, offset, size))
		return SetBatchNode(key, metadata, batch)
	}

	// Original mode: one file per chunk
	cfpath, err := fio.WriteChonk(db.Opts().Dir, data, key, db.Opts().EncryptionKey)
	if err != nil {
		return err
	}
	return SetBatchNode(key, cfpath, batch)
}

func SetBatchNode(key, data []byte, batch *badger.WriteBatch) error {
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	}
	return batch.Set(key, data)
}
func SetNode(key, data []byte, db *badger.DB) error {
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	}
	return db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, data)
	})
}
func PingNode(key []byte, db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		return err
	})
}
func GetChonkData(restoreIndex, start, size, dbstart, end int64, key []byte, db *badger.DB) ([]byte, error) {
	data, err := GetChonkNode(key, db)
	if err != nil {
		return nil, err
	}

	if restoreIndex == dbstart {
		actualStart := start - restoreIndex
		data = data[actualStart:]
	}
	if size < int64(len(data)) {
		data = data[:size]
	} else if (restoreIndex + cnst.ChonkSize) > end {
		actualEnd := end - restoreIndex
		data = data[:actualEnd]
	}

	return data, nil
}
func GetChonkSize(restoreIndex, start, size, dbstart, end int64, key []byte, db *badger.DB) (int64, error) {
	data, err := GetChonkData(restoreIndex, start, size, dbstart, end, key, db)
	if err != nil {
		return -1, err
	}

	data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	data, err = util.SealAES(db.Opts().EncryptionKey, data)
	if err != nil {
		return -1, err
	}
	return int64(len(data)), nil
}
func GetChonkNode(key []byte, db *badger.DB) ([]byte, error) {
	// Try to get from database first (backward compatibility or non-hierarchical mode)
	metadata, err := GetNode(key, db)
	if err == nil {
		// Found in DB, parse and return
		// Parse metadata: "path|offset|size"
		parts := strings.Split(string(metadata), "|")
		if len(parts) != 3 {
			// Fallback to old format (direct file path) for backward compatibility
			return fio.ReadChonk(metadata, db.Opts().EncryptionKey)
		}

		containerPath := parts[0]
		offset, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse offset: %w", err)
		}
		size, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse size: %w", err)
		}

		return fio.ReadChunkFromContainer(containerPath, offset, size, db.Opts().EncryptionKey)
	}

	// Not found in DB, try hierarchical block index
	blockMgr := fio.NewBlockManager(db.Opts().Dir, nil)
	containerPath, offset, size, err := blockMgr.GetChunkMetadata(key)
	if err != nil {
		return nil, fmt.Errorf("chunk not found in DB or block index: %w", err)
	}

	return fio.ReadChunkFromContainer(containerPath, offset, size, db.Opts().EncryptionKey)
}

// StreamLogicalFileBytes iterates over all chunks belonging to the logical file
// identified by fid (IndexedFile, PartitionFile, or EvidenceFile), trims each chunk
// to the file's actual byte range, and calls fn with the trimmed data in order.
// This mirrors the restore loop but without writing to disk, enabling streaming
// computation (e.g. full-file SimHash) without buffering the entire file in memory.
func StreamLogicalFileBytes(fid []byte, db *badger.DB, fn func([]byte) error) error {
	start, size, ehash, err := getLogicalFileMeta(fid, db)
	if err != nil {
		return err
	}

	var dbstart int64
	if start > 0 {
		dbstart = util.GetDBStartOffset(start)
	}
	end := start + size

	for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, restoreIndex)
		chash, err := GetNode(relKey, db)
		if err != nil {
			return err
		}
		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		data, err := GetChonkData(restoreIndex, start, size, dbstart, end, ckey, db)
		if err != nil {
			return err
		}
		if err := fn(data); err != nil {
			return err
		}
	}
	return nil
}

func getLogicalFileMeta(fid []byte, db *badger.DB) (start, size int64, ehash []byte, err error) {
	switch {
	case bytes.HasPrefix(fid, []byte(cnst.EviFileNamespace)):
		efile, e := GetEvidenceFile(fid, db)
		if e != nil {
			return 0, 0, nil, e
		}
		ehash = fid[len(cnst.EviFileNamespace):]
		return efile.Start, efile.Size, ehash, nil
	case bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)):
		pfile, e := GetPartitionFile(fid, db)
		if e != nil {
			return 0, 0, nil, e
		}
		name := util.GetArbitratyMapKey(pfile.Names)
		ehash, e = util.GetEvidenceFileHash(name)
		if e != nil {
			return 0, 0, nil, e
		}
		return pfile.Start, pfile.Size, ehash, nil
	case bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)):
		ifile, e := GetIndexedFile(fid, db)
		if e != nil {
			return 0, 0, nil, e
		}
		name := util.GetArbitratyMapKey(ifile.Names)
		ehash, e = util.GetEvidenceFileHash(name)
		if e != nil {
			return 0, 0, nil, e
		}
		return ifile.Start, ifile.Size, ehash, nil
	default:
		return 0, 0, nil, cnst.ErrUnknownFileType
	}
}

func GetNode(key []byte, db *badger.DB) ([]byte, error) {
	var data []byte

	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}

		err = item.Value(func(val []byte) error {
			data, err = item.ValueCopy(val)
			return err
		})

		return err
	})
	if err != nil {
		return nil, err
	}

	decoded, err := cnst.DECODER.DecodeAll(data, nil)
	if err == nil {
		data = decoded
	}
	return data, nil
}

func GuessFileType(encodedHash string, db *badger.DB) ([]byte, error) {
	fhash, err := base64.StdEncoding.DecodeString(encodedHash)
	if err != nil {
		return nil, err
	}

	fid := util.AppendToBytesSlice(cnst.IdxFileNamespace, fhash)
	err = PingNode(fid, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == nil {
		return fid, nil
	}

	fid = util.AppendToBytesSlice(cnst.PartiFileNamespace, fhash)
	err = PingNode(fid, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == nil {
		return fid, nil
	}

	fid = util.AppendToBytesSlice(cnst.EviFileNamespace, fhash)
	err = PingNode(fid, db)
	return fid, err
}
