package dbio

import (
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/dgraph-io/badger/v3/options"
	"github.com/klauspost/compress/s2"
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
	opts.NumMemtables = 1
	opts.NumLevelZeroTables = 1
	opts.NumLevelZeroTablesStall = 2
	opts.BloomFalsePositive = 0

	return badger.Open(opts)
}

func SetFile[T structs.FileTypes](id []byte, filenode T, db *badger.DB) error {
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		return err
	}
	return SetNode(id, data, db)
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

func SetReverseRelationNode(key []byte, revRelNode []structs.ReverseRelation, batch *badger.WriteBatch) error {
	data, err := msgpack.Marshal(revRelNode)
	if err != nil {
		return err
	}
	return SetBatchNode(key, data, batch)
}
func GetReverseRelationNode(key []byte, db *badger.DB) ([]structs.ReverseRelation, error) {
	var reverseRelations []structs.ReverseRelation

	data, err := GetNode(key, db)
	if err != nil {
		return nil, err
	}

	err = msgpack.Unmarshal(data, &reverseRelations)
	return reverseRelations, err
}

func SetBatchNode(key, data []byte, batch *badger.WriteBatch) error {
	if !cnst.QUICKOPT {
		data = s2.EncodeBest(nil, data)
	}
	return batch.Set(key, data)
}
func SetNode(key, data []byte, db *badger.DB) error {
	if !cnst.QUICKOPT {
		data = s2.EncodeBest(nil, data)
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

	decoded, err := s2.Decode(nil, data)
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
