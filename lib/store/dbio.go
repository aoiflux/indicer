package store

import (
	"encoding/json"
	"indicer/lib/constant"
	"indicer/lib/structs"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/dgraph-io/badger/v3/options"
	"github.com/klauspost/compress/s2"
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
	opts.EncryptionKeyRotationDuration = time.Hour * 12
	return badger.Open(opts)
}

type FileTypes interface {
	structs.IndexedFile | structs.PartitionFile | structs.EvidenceFile
}

func setFile[T FileTypes](id []byte, filenode T, db *badger.DB) error {
	data, err := json.Marshal(filenode)
	if err != nil {
		return err
	}
	return setNode(id, data, db)
}

func getEvidenceFile(key []byte, db *badger.DB) (structs.EvidenceFile, error) {
	var evidenceFile structs.EvidenceFile

	data, err := getNode(key, db)
	if err != nil {
		return evidenceFile, err
	}

	err = json.Unmarshal(data, &evidenceFile)
	return evidenceFile, err
}
func getPartitionFile(key []byte, db *badger.DB) (structs.PartitionFile, error) {
	var partitionFile structs.PartitionFile

	data, err := getNode(key, db)
	if err != nil {
		return partitionFile, err
	}

	err = json.Unmarshal(data, &partitionFile)
	return partitionFile, err
}
func getIndexedFile(key []byte, db *badger.DB) (structs.IndexedFile, error) {
	var indexedFile structs.IndexedFile

	data, err := getNode(key, db)
	if err != nil {
		return indexedFile, err
	}

	err = json.Unmarshal(data, &indexedFile)
	return indexedFile, err
}

func setBatchNode(key, data []byte, batch *badger.WriteBatch) error {
	encoded := s2.EncodeBest(nil, data)
	return batch.Set(key, encoded)
}
func setNode(key, data []byte, db *badger.DB) error {
	encoded := s2.EncodeBest(nil, data)
	return db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, encoded)
	})
}
func pingNode(key []byte, db *badger.DB) error {
	return db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(key)
		return err
	})
}
func getNode(key []byte, db *badger.DB) ([]byte, error) {
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
