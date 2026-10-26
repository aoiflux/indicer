package dbio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"indicer/internal/msgpackx"
	"indicer/lib/blobcodec"
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
	// Legacy quick mode used badger's default (Snappy) tables and no key.
	if cnst.QUICKOPT {
		return badger.Open(BadgerOptions(datadir, nil, options.Snappy))
	}
	return badger.Open(BadgerOptions(datadir, key, options.ZSTD))
}

// badgerLogger sends badger's own messages to the DUES log. Its default logger
// writes to stderr, which corrupts the TUI and pollutes FFI hosts' output.
type badgerLogger struct{}

func (badgerLogger) Errorf(f string, a ...any)   { logging.GetSugar().Errorf("badger: "+f, a...) }
func (badgerLogger) Warningf(f string, a ...any) { logging.GetSugar().Warnf("badger: "+f, a...) }
func (badgerLogger) Infof(f string, a ...any)    { logging.GetSugar().Debugf("badger: "+f, a...) }
func (badgerLogger) Debugf(f string, a ...any)   { logging.GetSugar().Debugf("badger: "+f, a...) }

// BadgerZSTDLevel is the zstd level badger compresses table blocks with.
//
// badger builds one process-wide encoder from the level of the first table
// it writes and keeps it for the life of the process, with one encoder state
// per CPU. Level 10 and above select zstd's best-compression encoder, about
// 50 MB a state, so level 15 held some 800 MB on a 16-CPU machine for as
// long as a library host kept DUES loaded (FFI-4). Level 3 holds under 2 MB a
// state. Table blocks are 4 KiB of keys and values DUES has mostly
// compressed already: storing jean.e01 made 18.3 to 18.7 MB of tables at
// either level, and level 3 stored it 4% faster. Decoding does not depend on
// the level, so existing databases are unaffected.
const BadgerZSTDLevel = 3

// BadgerOptions builds the options a badger directory is opened with. key is
// the encryption key (nil for none). Both key and table compression are fixed
// when the database is created: badger decodes every table with the
// compression it is opened with.
func BadgerOptions(datadir string, key []byte, compression options.CompressionType) badger.Options {
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
	// WithLoggingLevel installs badger's stderr logger, so it must not follow
	// WithLogger.
	opts = opts.WithLogger(badgerLogger{})
	opts.IndexCacheSize = indexCache
	opts.SyncWrites = true
	opts.NumGoroutines = cnst.GetMaxThreadCount()
	opts.Compression = compression
	if compression == options.ZSTD {
		opts.ZSTDCompressionLevel = BadgerZSTDLevel
	}
	if len(key) > 0 {
		opts.EncryptionKey = key
		opts.EncryptionKeyRotationDuration = time.Hour * 168
	}
	opts.CompactL0OnClose = true
	opts.LmaxCompaction = true
	// badger refuses fewer than two compactors, and low-resource mode runs a
	// single worker thread (CFG-4). More than eight adds contention, not speed.
	opts.NumCompactors = min(max(opts.NumGoroutines, 2), 8)
	opts.BlockCacheSize = blockCache
	opts.IndexCacheSize = indexCache
	opts.ValueLogFileSize = 64 << 20
	// ValueLogMaxEntries keeps badger's default. Tying it to the worker count
	// rotated value-log files after a few dozen entries (CFG-9).

	return opts
}

func SetFile[T structs.FileTypes](id []byte, filenode T, db *badger.DB) error {
	logging.GetLogger().Debug("SetFile START", zap.Int("id_length", len(id)))
	if _, ok := any(filenode).(structs.EvidenceFile); ok {
		id = CanonicalEvidenceKey(id)
	}
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		logging.GetLogger().Error("SetFile MARSHAL_ERROR", zap.Error(err))
		return err
	}
	if !cnst.QUICKOPT {
		data = cnst.ENCODER.EncodeAll(data, make([]byte, 0, len(data)))
	}

	err = db.Update(func(txn *badger.Txn) error {
		return txn.Set(id, data)
	})
	if err != nil {
		logging.GetLogger().Error("SetFile SET_NODE_ERROR", zap.Error(err), zap.Int("id_length", len(id)))
		return err
	}
	logging.GetLogger().Debug("SetFile COMPLETE", zap.Int("id_length", len(id)))
	return nil
}
func SetIndexedFile(id []byte, filenode structs.IndexedFile, batch *badger.WriteBatch) error {
	id = CanonicalIndexedKey(id)
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		return err
	}
	return SetBatchNode(id, data, batch)
}

func GetEvidenceFile(key []byte, db *badger.DB) (structs.EvidenceFile, error) {
	var evidenceFile structs.EvidenceFile
	key = CanonicalEvidenceKey(key)
	logging.GetLogger().Debug("GetEvidenceFile START", zap.Int("key_length", len(key)))

	data, err := GetNode(key, db)
	if err != nil {
		logging.GetLogger().Error("GetEvidenceFile GET_NODE_ERROR", zap.Error(err), zap.Int("key_length", len(key)))
		return evidenceFile, err
	}

	err = msgpackx.Unmarshal(data, &evidenceFile)
	if err != nil {
		logging.GetLogger().Error("GetEvidenceFile UNMARSHAL_ERROR", zap.Error(err), zap.Int("key_length", len(key)))
		return evidenceFile, err
	}
	logging.GetLogger().Debug("GetEvidenceFile COMPLETE", zap.Int("key_length", len(key)))
	return evidenceFile, err
}

func CanonicalEvidenceKey(key []byte) []byte {
	if bytes.HasPrefix(key, []byte(cnst.EviFileNamespace)) {
		return key
	}
	return util.AppendToBytesSlice(cnst.EviFileNamespace, key)
}

func EvidenceRawID(key []byte) []byte {
	if bytes.HasPrefix(key, []byte(cnst.EviFileNamespace)) {
		return key[len(cnst.EviFileNamespace):]
	}
	return key
}

func CanonicalPartitionKey(key []byte) []byte {
	if bytes.HasPrefix(key, []byte(cnst.PartiFileNamespace)) {
		return key
	}
	return util.AppendToBytesSlice(cnst.PartiFileNamespace, key)
}

func CanonicalIndexedKey(key []byte) []byte {
	if bytes.HasPrefix(key, []byte(cnst.IdxFileNamespace)) {
		return key
	}
	return util.AppendToBytesSlice(cnst.IdxFileNamespace, key)
}
func GetPartitionFile(key []byte, db *badger.DB) (structs.PartitionFile, error) {
	var partitionFile structs.PartitionFile
	key = CanonicalPartitionKey(key)

	data, err := GetNode(key, db)
	if err != nil {
		return partitionFile, err
	}

	err = msgpackx.Unmarshal(data, &partitionFile)
	return partitionFile, err
}
func GetIndexedFile(key []byte, db *badger.DB) (structs.IndexedFile, error) {
	var indexedFile structs.IndexedFile
	key = CanonicalIndexedKey(key)

	data, err := GetNode(key, db)
	if err != nil {
		return indexedFile, err
	}

	err = msgpackx.Unmarshal(data, &indexedFile)
	return indexedFile, err
}

type ReverseRelationAppendMember struct {
	Chash []byte
	Index int64
}

const reverseRelationAppendKeyVersionBinary byte = 1

func reverseRelationAppendShard(fhash []byte) int {
	if len(fhash) == 0 {
		return 0
	}
	return int(fhash[len(fhash)-1]) % cnst.ReverseRelationAppendShardCount
}

func buildReverseRelationAppendMemberKey(chash []byte, index int64, shard int, fhash []byte) []byte {
	key := make([]byte, 0, len(cnst.ReverseRelationAppendNamespace)+len(chash)+len(cnst.DataSeperator)+1+8+1+len(fhash))
	key = append(key, cnst.ReverseRelationAppendNamespace...)
	key = append(key, chash...)
	key = append(key, cnst.DataSeperator...)
	key = append(key, reverseRelationAppendKeyVersionBinary)
	var idxBytes [8]byte
	binary.BigEndian.PutUint64(idxBytes[:], uint64(index))
	key = append(key, idxBytes[:]...)
	key = append(key, byte(shard))
	key = append(key, fhash...)
	return key
}

func SetReverseRelationAppendMembers(fhash []byte, members []ReverseRelationAppendMember, batch *badger.WriteBatch) error {
	if len(members) == 0 {
		return nil
	}

	shard := reverseRelationAppendShard(fhash)

	for _, member := range members {
		memberKey := buildReverseRelationAppendMemberKey(member.Chash, member.Index, shard, fhash)
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

func getReverseRelationAppendMembers(key []byte, db *badger.DB) (map[string]struct{}, error) {
	reverseRelations := make(map[string]struct{})
	binaryPrefix, err := reverseRelationAppendMemberPrefixBinary(key)
	if err != nil {
		return nil, err
	}
	legacyPrefix, err := reverseRelationAppendMemberPrefixLegacy(key)
	if err != nil {
		return nil, err
	}

	err = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 128
		it := txn.NewIterator(opts)
		defer it.Close()

		for _, prefix := range [][]byte{binaryPrefix, legacyPrefix} {
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
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	chash, index, err := reverseRelationLookupParts(key)
	if err != nil {
		return nil, err
	}
	segmentRelations, err := getReverseRelationSegmentMembersAtIndex(chash, index, db)
	if err != nil {
		return nil, err
	}
	for rel := range segmentRelations {
		reverseRelations[rel] = struct{}{}
	}

	return reverseRelations, err
}

func reverseRelationAppendMemberPrefixLegacy(key []byte) ([]byte, error) {
	chash, index, err := reverseRelationLookupParts(key)
	if err != nil {
		return nil, err
	}
	return util.AppendToBytesSlice(cnst.ReverseRelationAppendNamespace, chash, cnst.DataSeperator, index, cnst.DataSeperator), nil
}

func reverseRelationAppendMemberPrefixBinary(key []byte) ([]byte, error) {
	chash, index, err := reverseRelationLookupParts(key)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, 0, len(cnst.ReverseRelationAppendNamespace)+len(chash)+len(cnst.DataSeperator)+1+8)
	prefix = append(prefix, cnst.ReverseRelationAppendNamespace...)
	prefix = append(prefix, chash...)
	prefix = append(prefix, cnst.DataSeperator...)
	prefix = append(prefix, reverseRelationAppendKeyVersionBinary)
	var idxBytes [8]byte
	binary.BigEndian.PutUint64(idxBytes[:], uint64(index))
	prefix = append(prefix, idxBytes[:]...)
	return prefix, nil
}

func reverseRelationLookupParts(key []byte) ([]byte, int64, error) {
	if !bytes.HasPrefix(key, []byte(cnst.ReverseRelationNamespace)) {
		return nil, 0, fmt.Errorf("invalid reverse relation lookup key: %q", string(key))
	}
	body := key[len(cnst.ReverseRelationNamespace):]
	sep := []byte(cnst.DataSeperator)
	sepIdx := bytes.LastIndex(body, sep)
	if sepIdx < 0 {
		return nil, 0, fmt.Errorf("invalid reverse relation lookup key: %q", string(key))
	}
	chash := body[:sepIdx]
	idxBytes := bytes.TrimLeft(body[sepIdx+len(sep):], "|:")
	if len(idxBytes) == 0 {
		return nil, 0, fmt.Errorf("invalid reverse relation lookup key index: %q", string(key))
	}
	idx, err := strconv.ParseInt(string(idxBytes), 10, 64)
	if err != nil {
		return nil, 0, err
	}
	return chash, idx, nil
}

func reverseRelationAppendMemberIndex(chash, memberKey []byte) (int64, bool) {
	prefixLen := len(cnst.ReverseRelationAppendNamespace) + len(chash) + len(cnst.DataSeperator)
	if len(memberKey) <= prefixLen {
		return 0, false
	}

	payload := memberKey[prefixLen:]
	if payload[0] == reverseRelationAppendKeyVersionBinary {
		if len(payload) < 1+8 {
			return 0, false
		}
		return int64(binary.BigEndian.Uint64(payload[1 : 1+8])), true
	}

	sep := []byte(cnst.DataSeperator)
	idxEnd := bytes.Index(payload, sep)
	if idxEnd < 0 {
		return 0, false
	}
	idxBytes := bytes.TrimLeft(payload[:idxEnd], "|:")
	if len(idxBytes) == 0 {
		return 0, false
	}
	idx, err := strconv.ParseInt(string(idxBytes), 10, 64)
	if err != nil {
		return 0, false
	}
	return idx, true
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

// MaterializeChonkNode performs the expensive chunk materialization work
// (compression/encryption/container write or file write). It returns the
// encoded C||| metadata to persist for the chunk.
func MaterializeChonkNode(key, data []byte, db *badger.DB, containerMgr *fio.ContainerManager) ([]byte, error) {
	originalSize := int64(len(data))

	if containerMgr != nil {
		return materializeContainerChonkNode(key, data, originalSize, db, containerMgr)
	}

	return materializeFileChonkNode(key, data, originalSize, db)
}

func materializeContainerChonkNode(key, data []byte, originalSize int64, db *badger.DB, containerMgr *fio.ContainerManager) ([]byte, error) {
	containerPath, offset, size, err := containerMgr.WriteChunkToContainer(key, func() ([]byte, error) {
		return blobcodec.For(db).Encode(key, data)
	})
	if err != nil {
		return nil, err
	}

	metadata := structs.ChonkMetadata{
		Path:         StoredBlobPath(KVDir(db), containerPath),
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
	data, err := blobcodec.For(db).Encode(key, data)
	if err != nil {
		return nil, err
	}

	cfpath, err := fio.WriteChonk(KVDir(db), data, key, db.Opts().EncryptionKey)
	if err != nil {
		return nil, err
	}
	dataLen := int64(len(data))

	metadata := structs.ChonkMetadata{
		Path:         StoredBlobPath(KVDir(db), string(cfpath)),
		Offset:       0,
		OriginalSize: originalSize,
		StoredSize:   dataLen,
		EncodedSize:  dataLen,
		Container:    false,
	}
	encodedMetadata, err := msgpack.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	return encodedMetadata, nil
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

// GetChonkNode returns the decoded content of the chunk whose key is key.
func GetChonkNode(key []byte, db *badger.DB) ([]byte, error) {
	loc, err := LocateChunk(key, db)
	if err != nil {
		return nil, err
	}
	return readChonk(key, loc, db)
}

// readChonk reads and decodes the chunk key stored at parsed.
func readChonk(key []byte, parsed structs.ChonkMetadata, db *badger.DB) ([]byte, error) {
	var err error
	var stored []byte
	if parsed.Container {
		stored, err = fio.ReadChunkFromContainer(ResolveBlobPath(KVDir(db), parsed.Path), parsed.Offset, parsed.EncodedSize)
	} else {
		stored, err = fio.ReadChonk(ResolveBlobPath(KVDir(db), parsed.Path))
	}
	if err != nil {
		return nil, err
	}
	return blobcodec.For(db).Decode(key, stored)
}

func parseChonkMetadata(metadata []byte) (structs.ChonkMetadata, error) {
	var parsed structs.ChonkMetadata
	if err := msgpackx.Unmarshal(metadata, &parsed); err != nil {
		return parsed, fmt.Errorf("%w: %v", ErrChunkMetaInvalid, err)
	}
	if parsed.Path == "" {
		return parsed, fmt.Errorf("%w: empty path", ErrChunkMetaInvalid)
	}
	if parsed.Container && parsed.EncodedSize <= 0 {
		return parsed, fmt.Errorf("%w: encoded size must be > 0 for container mode", ErrChunkMetaInvalid)
	}
	if parsed.StoredSize < 0 || parsed.OriginalSize < 0 {
		return parsed, fmt.Errorf("%w: negative sizes", ErrChunkMetaInvalid)
	}
	return parsed, nil
}

// StreamLogicalFileBytes calls fn with the content of the object fid (an
// evidence, partition or indexed file), in order, a chunk's part at a time,
// each chunk checked against its hash. The bytes must not be modified.
func StreamLogicalFileBytes(fid []byte, db *badger.DB, fn func([]byte) error) error {
	l, err := OpenLogical(db, fid, ReadOptions{})
	if err != nil {
		return err
	}
	return l.Stream(context.Background(), fn)
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
	fids, err := GuessFileTypes(encodedHash, db)
	if err != nil {
		return nil, err
	}
	if len(fids) == 0 {
		return nil, badger.ErrKeyNotFound
	}
	return fids[len(fids)-1], nil
}

func GuessFileTypes(encodedHash string, db *badger.DB) ([][]byte, error) {
	fhash, err := base64.StdEncoding.DecodeString(encodedHash)
	if err != nil {
		return nil, err
	}

	var matches [][]byte
	seen := make(map[string]struct{})
	appendUnique := func(ids ...[]byte) {
		for _, id := range ids {
			if len(id) == 0 {
				continue
			}
			k := string(id)
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			matches = append(matches, id)
		}
	}

	fid := util.AppendToBytesSlice(cnst.IdxFileNamespace, fhash)
	err = PingNode(fid, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == nil {
		appendUnique(fid)
	}

	fid = util.AppendToBytesSlice(cnst.PartiFileNamespace, fhash)
	err = PingNode(fid, db)
	if err != nil && err != badger.ErrKeyNotFound {
		return nil, err
	}
	if err == nil {
		appendUnique(fid)
	}

	partitionLookupKey := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, encodedHash)
	mappedPartitionIDs, err := GetHashLookupUUIDsByKey(partitionLookupKey, db)
	if err == nil {
		appendUnique(mappedPartitionIDs...)
	} else if err != badger.ErrKeyNotFound {
		return nil, err
	}

	indexedLookupKey := util.AppendToBytesSlice(cnst.IdxFileHashLookupNamespace, encodedHash)
	mappedIndexedIDs, err := GetHashLookupUUIDsByKey(indexedLookupKey, db)
	if err == nil {
		appendUnique(mappedIndexedIDs...)
	} else if err != badger.ErrKeyNotFound {
		return nil, err
	}

	fid = util.AppendToBytesSlice(cnst.EviFileNamespace, fhash)
	err = PingNode(fid, db)
	if err == nil {
		appendUnique(fid)
	} else if err != badger.ErrKeyNotFound {
		return nil, err
	}

	lookupKey := util.AppendToBytesSlice(cnst.EviFileHashLookupNamespace, encodedHash)
	mappedIDs, err := GetHashLookupUUIDsByKey(lookupKey, db)
	if err == nil {
		appendUnique(mappedIDs...)
	} else if err != badger.ErrKeyNotFound {
		return nil, err
	}

	if len(matches) == 0 {
		return nil, badger.ErrKeyNotFound
	}
	return matches, nil
}

// valueLogGCBudget bounds how long CompactValueLog may run.
const valueLogGCBudget = 30 * time.Second

// CompactValueLog reclaims value-log space after a store or repair, rewriting
// files that are at least half garbage until none is left or the time budget
// is spent. Read-only databases are left alone. It returns the number of
// files rewritten.
func CompactValueLog(db *badger.DB) (int, error) {
	if db.Opts().ReadOnly {
		return 0, nil
	}
	deadline := time.Now().Add(valueLogGCBudget)
	n := 0
	for time.Now().Before(deadline) {
		err := db.RunValueLogGC(0.5)
		switch {
		case err == nil:
			n++
		case errors.Is(err, badger.ErrNoRewrite), errors.Is(err, badger.ErrRejected):
			return n, nil
		default:
			return n, err
		}
	}
	return n, nil
}
