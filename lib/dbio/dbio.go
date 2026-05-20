package dbio

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/logging"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"strconv"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
	"github.com/vmihailenco/msgpack/v5"
	"go.uber.org/zap"
)

func ConnectDB(datadir string, key []byte) (*badger.DB, error) {
	datadir = util.KVDBPath(datadir)
	if err := os.MkdirAll(datadir, cnst.DirPerm); err != nil {
		return nil, err
	}

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
	logging.GetLogger().Debug("SetFile START", zap.Int("id_length", len(id)))
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		logging.GetLogger().Error("SetFile MARSHAL_ERROR", zap.Error(err))
		return err
	}
	err = SetNode(id, data, db)
	if err != nil {
		logging.GetLogger().Error("SetFile SET_NODE_ERROR", zap.Error(err), zap.Int("id_length", len(id)))
		return err
	}
	logging.GetLogger().Debug("SetFile COMPLETE", zap.Int("id_length", len(id)))
	return nil
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
	logging.GetLogger().Debug("GetEvidenceFile START", zap.Int("key_length", len(key)))

	data, err := GetNode(key, db)
	if err != nil {
		logging.GetLogger().Error("GetEvidenceFile GET_NODE_ERROR", zap.Error(err), zap.Int("key_length", len(key)))
		return evidenceFile, err
	}

	err = msgpack.Unmarshal(data, &evidenceFile)
	if err != nil {
		logging.GetLogger().Error("GetEvidenceFile UNMARSHAL_ERROR", zap.Error(err), zap.Int("key_length", len(key)))
		return evidenceFile, err
	}
	logging.GetLogger().Debug("GetEvidenceFile COMPLETE", zap.Int("key_length", len(key)))
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

type ReverseRelationAppendMember struct {
	Chash []byte
	Index int64
}

func reverseRelationAppendShard(fhash []byte) int {
	if len(fhash) == 0 {
		return 0
	}
	return int(fhash[len(fhash)-1]) % cnst.ReverseRelationAppendShardCount
}

func buildReverseRelationAppendMemberKey(chash []byte, index int64, shard int, encodedFHash string) []byte {
	key := make([]byte, 0, len(cnst.ReverseRelationAppendNamespace)+len(chash)+len(cnst.DataSeperator)*3+len(encodedFHash)+16)
	key = append(key, cnst.ReverseRelationAppendNamespace...)
	key = append(key, chash...)
	key = append(key, cnst.DataSeperator...)
	key = strconv.AppendInt(key, index, 10)
	key = append(key, cnst.DataSeperator...)
	key = strconv.AppendInt(key, int64(shard), 10)
	key = append(key, cnst.DataSeperator...)
	key = append(key, encodedFHash...)
	return key
}

func SetReverseRelationAppendMembers(fhash []byte, members []ReverseRelationAppendMember, batch *badger.WriteBatch) error {
	if len(members) == 0 {
		return nil
	}

	encodedFHash := base64.RawURLEncoding.EncodeToString(fhash)
	shard := reverseRelationAppendShard(fhash)

	for _, member := range members {
		memberKey := buildReverseRelationAppendMemberKey(member.Chash, member.Index, shard, encodedFHash)
		if err := SetBatchNode(memberKey, fhash, batch); err != nil {
			return err
		}
	}

	return nil
}

func SetReverseRelationAppendMember(chash []byte, index int64, fhash []byte, batch *badger.WriteBatch) error {
	member := ReverseRelationAppendMember{Chash: chash, Index: index}
	return SetReverseRelationAppendMembers(fhash, []ReverseRelationAppendMember{member}, batch)
}

func GetReverseRelationNode(key []byte, db *badger.DB) (map[string]struct{}, error) {
	appendRelations, err := getReverseRelationAppendMembers(key, db)
	if err != nil {
		return nil, err
	}
	if len(appendRelations) == 0 {
		return nil, badger.ErrKeyNotFound
	}
	return appendRelations, nil
}

func GetReverseRelationAppendPrefixMembers(revPrefixKey []byte, db *badger.DB) (map[int64][]string, error) {
	split := bytes.Split(revPrefixKey, []byte(cnst.DataSeperator))
	chash := bytes.TrimPrefix(split[1], []byte(":"))
	prefix := util.AppendToBytesSlice(cnst.ReverseRelationAppendNamespace, chash, cnst.DataSeperator)
	similarMap := make(map[int64][]string)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			memberKey := item.KeyCopy(nil)

			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

			split := bytes.Split(memberKey, []byte(cnst.DataSeperator))
			if len(split) < 3 {
				return fmt.Errorf("invalid reverse-relation append key: %q", string(memberKey))
			}
			idxstr := split[2]
			idx, err := util.GetNumber(string(idxstr))
			if err != nil {
				return err
			}

			revlist := similarMap[idx]
			revlist = append(revlist, string(value))
			similarMap[idx] = revlist
		}

		return nil
	})

	return similarMap, err
}

func getReverseRelationAppendMembers(key []byte, db *badger.DB) (map[string]struct{}, error) {
	reverseRelations := make(map[string]struct{})
	prefix := reverseRelationAppendMemberPrefix(key)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 128
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(value, nil)
			if err == nil {
				value = decoded
			}

			reverseRelations[string(value)] = struct{}{}
		}

		return nil
	})

	return reverseRelations, err
}

func reverseRelationAppendMemberPrefix(key []byte) []byte {
	split := bytes.Split(key, []byte(cnst.DataSeperator))
	chash := bytes.TrimPrefix(split[1], []byte(":"))
	return util.AppendToBytesSlice(cnst.ReverseRelationAppendNamespace, chash, cnst.DataSeperator, split[2], cnst.DataSeperator)
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
	encodedMetadata, err := MaterializeChonkNode(key, data, db, containerMgr, blockMgr)
	if err != nil {
		return err
	}
	if len(encodedMetadata) == 0 {
		return nil
	}
	return SetBatchNode(key, encodedMetadata, batch)
}

// MaterializeChonkNode performs the expensive chunk materialization work
// (compression/encryption/container write or file write). It returns encoded
// metadata for DB persistence when metadata is DB-backed, or nil when metadata
// is written to hierarchical block index files.
func MaterializeChonkNode(key, data []byte, db *badger.DB, containerMgr *fio.ContainerManager, blockMgr *fio.BlockManager) ([]byte, error) {
	originalSize := int64(len(data))

	if containerMgr != nil {
		return materializeContainerChonkNode(key, data, originalSize, db, containerMgr, blockMgr)
	}

	return materializeFileChonkNode(key, data, originalSize, db)
}

func materializeContainerChonkNode(key, data []byte, originalSize int64, db *badger.DB, containerMgr *fio.ContainerManager, blockMgr *fio.BlockManager) ([]byte, error) {
	containerPath, offset, size, err := containerMgr.WriteChunkToContainer(data, key, db.Opts().EncryptionKey)
	if err != nil {
		return nil, err
	}

	if blockMgr != nil {
		return nil, blockMgr.AddChunkMetadata(key, containerPath, offset, size)
	}

	metadata := structs.ChonkMetadata{
		Path:         containerPath,
		Offset:       offset,
		OriginalSize: originalSize,
		StoredSize:   size,
		EncodedSize:  size,
		Container:    true,
	}
	encodedMetadata, err := msgpack.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	return encodedMetadata, nil
}

func materializeFileChonkNode(key, data []byte, originalSize int64, db *badger.DB) ([]byte, error) {
	cfpath, err := fio.WriteChonk(db.Opts().Dir, data, key, db.Opts().EncryptionKey)
	if err != nil {
		return nil, err
	}

	stat, err := os.Stat(string(cfpath))
	if err != nil {
		return nil, err
	}

	metadata := structs.ChonkMetadata{
		Path:         string(cfpath),
		Offset:       0,
		OriginalSize: originalSize,
		StoredSize:   stat.Size(),
		EncodedSize:  stat.Size(),
		Container:    false,
	}
	encodedMetadata, err := msgpack.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	return encodedMetadata, nil
}

func setBatchChonkMetadata(key []byte, metadata structs.ChonkMetadata, batch *badger.WriteBatch) error {
	encodedMetadata, err := msgpack.Marshal(metadata)
	if err != nil {
		return err
	}
	return SetBatchNode(key, encodedMetadata, batch)
}

func SetBatchNode(key, data []byte, batch *badger.WriteBatch) error {
	logging.GetLogger().Debug("SetBatchNode START",
		zap.Int("key_length", len(key)),
		zap.Int("data_length", len(data)),
	)
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	}
	err := batch.Set(key, data)
	if err != nil {
		logging.GetLogger().Error("SetBatchNode WRITE_ERROR", zap.Error(err), zap.Int("key_length", len(key)))
		return err
	}
	return nil
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

	return trimLogicalChunkData(data, restoreIndex, start, size, dbstart, end), nil
}

func GetChonkDataWithMetadata(restoreIndex, start, size, dbstart, end int64, metadata []byte, db *badger.DB) ([]byte, error) {
	data, err := readChonkFromMetadata(metadata, db)
	if err != nil {
		return nil, err
	}

	return trimLogicalChunkData(data, restoreIndex, start, size, dbstart, end), nil
}

func trimLogicalChunkData(data []byte, restoreIndex, start, size, dbstart, end int64) []byte {
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

	return data
}

// GetStoredChonkSealedSize extracts the full-chunk sealed size persisted in
// metadata. It returns (size, true) when available, otherwise (0, false).
func GetStoredChonkSealedSize(metadata []byte) (int64, bool) {
	parsed, err := parseChonkMetadata(metadata)
	if err != nil || parsed.StoredSize < 0 {
		return 0, false
	}
	return parsed.StoredSize, true
}

func GetChonkSizeWithMetadata(restoreIndex, start, size, dbstart, end int64, metadata []byte, db *badger.DB) (int64, error) {
	data, err := GetChonkDataWithMetadata(restoreIndex, start, size, dbstart, end, metadata, db)
	if err != nil {
		return -1, err
	}
	return getSealedChunkSize(data, db)
}

func GetChonkSize(restoreIndex, start, size, dbstart, end int64, key []byte, db *badger.DB) (int64, error) {
	data, err := GetChonkData(restoreIndex, start, size, dbstart, end, key, db)
	if err != nil {
		return -1, err
	}
	return getSealedChunkSize(data, db)
}

func getSealedChunkSize(data []byte, db *badger.DB) (int64, error) {
	data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	var err error
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
		return readChonkFromMetadata(metadata, db)
	}

	// Not found in DB, try hierarchical block index
	blockMgr := fio.NewBlockManager(db.Opts().Dir, nil)
	containerPath, offset, size, err := blockMgr.GetChunkMetadata(key)
	if err != nil {
		return nil, fmt.Errorf("chunk not found in DB or block index: %w", err)
	}

	return fio.ReadChunkFromContainer(containerPath, offset, size, db.Opts().EncryptionKey)
}

func readChonkFromMetadata(metadata []byte, db *badger.DB) ([]byte, error) {
	parsed, err := parseChonkMetadata(metadata)
	if err != nil {
		return nil, err
	}

	if parsed.Container {
		return fio.ReadChunkFromContainer(parsed.Path, parsed.Offset, parsed.EncodedSize, db.Opts().EncryptionKey)
	}

	return fio.ReadChonk([]byte(parsed.Path), db.Opts().EncryptionKey)
}

func parseChonkMetadata(metadata []byte) (structs.ChonkMetadata, error) {
	var parsed structs.ChonkMetadata
	if err := msgpack.Unmarshal(metadata, &parsed); err != nil {
		return parsed, fmt.Errorf("failed to decode chunk metadata: %w", err)
	}
	if parsed.Path == "" {
		return parsed, fmt.Errorf("invalid chunk metadata: empty path")
	}
	if parsed.Container && parsed.EncodedSize <= 0 {
		return parsed, fmt.Errorf("invalid chunk metadata: encoded size must be > 0 for container mode")
	}
	if parsed.StoredSize < 0 || parsed.OriginalSize < 0 {
		return parsed, fmt.Errorf("invalid chunk metadata: negative sizes")
	}
	return parsed, nil
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

func GetNodesBatch(keys [][]byte, db *badger.DB) ([][]byte, []error) {
	logging.GetLogger().Debug("GetNodesBatch START", zap.Int("batch_key_count", len(keys)))
	values := make([][]byte, len(keys))
	errs := make([]error, len(keys))

	err := db.View(func(txn *badger.Txn) error {
		for i, key := range keys {
			item, e := txn.Get(key)
			if e != nil {
				errs[i] = e
				continue
			}

			e = item.Value(func(val []byte) error {
				copied, copyErr := item.ValueCopy(val)
				if copyErr != nil {
					return copyErr
				}

				decoded, decodeErr := cnst.DECODER.DecodeAll(copied, nil)
				if decodeErr == nil {
					values[i] = decoded
				} else {
					values[i] = copied
				}
				return nil
			})
			if e != nil {
				errs[i] = e
			}
		}
		return nil
	})

	if err != nil {
		for i := range errs {
			if errs[i] == nil {
				errs[i] = err
			}
		}
		logging.GetLogger().Error("GetNodesBatch DB_VIEW_ERROR", zap.Error(err), zap.Int("batch_key_count", len(keys)))
		return values, errs
	}

	logging.GetLogger().Debug("GetNodesBatch COMPLETE", zap.Int("batch_key_count", len(keys)))
	return values, errs
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
