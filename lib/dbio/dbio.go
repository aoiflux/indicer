package dbio

import (
	"encoding/base64"
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
		return nil, err
	}

	opts := badger.DefaultOptions(datadir)
	opts = opts.WithLoggingLevel(badger.ERROR)
	opts.IndexCacheSize = cacheLimit
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
	opts.BlockCacheSize = cacheLimit
	opts.IndexCacheSize = cacheLimit
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
