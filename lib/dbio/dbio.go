package dbio

import (
	"bytes"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"

	"github.com/klauspost/compress/s2"
	"github.com/vmihailenco/msgpack/v5"
	"go.etcd.io/bbolt"
)

func ConnectDB(readOnly bool, dbpath string) (*bbolt.DB, error) {
	_, err := os.Stat(dbpath)
	if readOnly && os.IsNotExist(err) {
		return nil, err
	}

	opts := bbolt.DefaultOptions
	opts.ReadOnly = readOnly
	opts.FreelistType = bbolt.FreelistMapType
	opts.PreLoadFreelist = true
	db, err := bbolt.Open(dbpath, 0666, opts)
	if err != nil {
		return nil, err
	}
	if readOnly {
		return db, err
	}

	db.MaxBatchSize, err = cnst.GetBatchLimit()
	if err != nil {
		return nil, err
	}
	err = db.Batch(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucket([]byte(cnst.EviBucket))
		if err != nil && err != bbolt.ErrBucketExists {
			return err
		}

		_, err = tx.CreateBucket([]byte(cnst.PartiBucket))
		if err != nil && err != bbolt.ErrBucketExists {
			return err
		}

		_, err = tx.CreateBucket([]byte(cnst.IdxBucket))
		if err != nil && err != bbolt.ErrBucketExists {
			return err
		}

		_, err = tx.CreateBucket([]byte(cnst.RelBucket))
		if err != nil && err != bbolt.ErrBucketExists {
			return err
		}

		_, err = tx.CreateBucket([]byte(cnst.ReverseRelBucket))
		if err != nil && err != bbolt.ErrBucketExists {
			return err
		}

		_, err = tx.CreateBucket([]byte(cnst.ChonkBucket))
		if err != nil && err != bbolt.ErrBucketExists {
			return err
		}

		return nil
	})

	return db, err
}
func SetFile[T structs.FileTypes](id []byte, filenode T, db *bbolt.DB) error {
	data, err := msgpack.Marshal(filenode)
	if err != nil {
		return err
	}
	return SetNode(id, data, db)
}

func GetEvidenceFile(key []byte, db *bbolt.DB) (structs.EvidenceFile, error) {
	var evidenceFile structs.EvidenceFile

	data, err := GetNode(key, db)
	if err != nil {
		return evidenceFile, err
	}

	err = msgpack.Unmarshal(data, &evidenceFile)
	return evidenceFile, err
}
func GetPartitionFile(key []byte, db *bbolt.DB) (structs.PartitionFile, error) {
	var partitionFile structs.PartitionFile

	data, err := GetNode(key, db)
	if err != nil {
		return partitionFile, err
	}

	err = msgpack.Unmarshal(data, &partitionFile)
	return partitionFile, err
}
func GetIndexedFile(key []byte, db *bbolt.DB) (structs.IndexedFile, error) {
	var indexedFile structs.IndexedFile

	data, err := GetNode(key, db)
	if err != nil {
		return indexedFile, err
	}

	err = msgpack.Unmarshal(data, &indexedFile)
	return indexedFile, err
}

func SetReverseRelationNode(key []byte, revRelNode []structs.ReverseRelation, db *bbolt.DB) error {
	data, err := msgpack.Marshal(revRelNode)
	if err != nil {
		return err
	}
	return SetNode(key, data, db)
}
func GetReverseRelationNode(key []byte, db *bbolt.DB) ([]structs.ReverseRelation, error) {
	var reverseRelations []structs.ReverseRelation

	data, err := GetNode(key, db)
	if err != nil {
		return nil, err
	}

	err = msgpack.Unmarshal(data, &reverseRelations)
	return reverseRelations, err
}

func SetNode(key, value []byte, db *bbolt.DB) error {
	splits := bytes.Split(key, []byte(cnst.NamespaceSeperator))
	namespace := splits[0]
	key = splits[1]
	encoded := s2.EncodeBest(nil, value)
	return db.Batch(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(namespace)
		return bucket.Put(key, encoded)
	})
}

func GetNode(key []byte, db *bbolt.DB) ([]byte, error) {
	splits := bytes.Split(key, []byte(cnst.NamespaceSeperator))
	namespace := splits[0]
	key = splits[1]
	var value []byte
	db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(namespace)
		value = bucket.Get(key)
		return nil
	})
	if value == nil {
		return nil, cnst.ErrKeyNotFound
	}
	return s2.Decode(nil, value)
}
func PingNode(key []byte, db *bbolt.DB) error {
	splits := bytes.Split(key, []byte(cnst.NamespaceSeperator))
	namespace := splits[0]
	key = splits[1]
	return db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(namespace)
		v := bucket.Get(key)
		if v == nil {
			return cnst.ErrKeyNotFound
		}
		return nil
	})
}

func GuessFileType(encodedHash string, db *bbolt.DB) ([]byte, error) {
	fhash, err := base64.StdEncoding.DecodeString(encodedHash)
	if err != nil {
		return nil, err
	}

	fid := util.AppendToBytesSlice(cnst.IdxFileNamespace, fhash)
	err = PingNode(fid, db)
	if err != nil && err != cnst.ErrKeyNotFound {
		return nil, err
	}
	if err == nil {
		return fid, nil
	}

	fid = util.AppendToBytesSlice(cnst.PartiFileNamespace, fhash)
	err = PingNode(fid, db)
	if err != nil && err != cnst.ErrKeyNotFound {
		return nil, err
	}
	if err == nil {
		return fid, nil
	}

	fid = util.AppendToBytesSlice(cnst.EviFileNamespace, fhash)
	err = PingNode(fid, db)
	return fid, err
}
