package dbio

import (
	"indicer/lib/constant"
	"indicer/lib/structs"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/dgraph-io/badger/v3/options"
	"github.com/klauspost/compress/s2"
	"github.com/vmihailenco/msgpack/v5"
)

func ConnectDB(datadir string, key []byte) (*badger.DB, error) {
	cacheLimit, err := constant.GetCacheLimit()
	if err != nil {
		return nil, err
	}
	opts := badger.DefaultOptions(datadir)
	opts = opts.WithLoggingLevel(badger.ERROR)
	opts.IndexCacheSize = cacheLimit
	opts.SyncWrites = true
	opts.NumGoroutines = constant.MaxThreadCount
	opts.BlockCacheSize = cacheLimit
	opts.Compression = options.ZSTD
	opts.ZSTDCompressionLevel = 15
	opts.EncryptionKey = key
	opts.EncryptionKeyRotationDuration = time.Hour * 168
	opts.MetricsEnabled = false
	opts.ChecksumVerificationMode = options.OnTableRead
	return badger.Open(opts)
}

type FileTypes interface {
	structs.IndexedFile | structs.PartitionFile | structs.EvidenceFile
}

func SetFile[T FileTypes](id []byte, filenode T, db *badger.DB) error {
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

func SetReverseRelationNode(key []byte, revRelNode []structs.ReverseRelation, db *badger.DB) error {
	data, err := msgpack.Marshal(revRelNode)
	if err != nil {
		return err
	}
	return SetNode(key, data, db)
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
	encoded := s2.EncodeBest(nil, data)
	return batch.Set(key, encoded)
}
func SetNode(key, data []byte, db *badger.DB) error {
	encoded := s2.EncodeBest(nil, data)
	return db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, encoded)
	})
}
func PingNode(key []byte, db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		return err
	})
}
func GetNode(key []byte, db *badger.DB) ([]byte, error) {
	var encoded []byte

	err := db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}

		err = item.Value(func(val []byte) error {
			encoded, err = item.ValueCopy(val)
			return err
		})

		return err
	})
	if err != nil {
		return nil, err
	}

	return s2.Decode(nil, encoded)
}
