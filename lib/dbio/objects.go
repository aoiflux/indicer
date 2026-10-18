package dbio

import (
	"fmt"

	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// GetObject reads the record of the materialised object oid (X|||).
func GetObject(db *badger.DB, oid []byte) (structs.ObjectRecord, error) {
	var obj structs.ObjectRecord
	err := db.View(func(txn *badger.Txn) error { return GetRecordTxn(txn, ObjectKey(oid), &obj) })
	return obj, err
}

// OpenObjectChunks opens the stored chunks of the materialised object oid
// (D10). An object is stored like an evidence, with relations keyed by its
// own ID, at the chunk size its record names.
func OpenObjectChunks(db *badger.DB, oid []byte, opts ReadOptions) (*EvidenceChunks, structs.ObjectRecord, error) {
	obj, err := GetObject(db, oid)
	if err != nil {
		return nil, obj, fmt.Errorf("object %x: %w", oid, err)
	}
	ec, err := openEvidenceChunks(db, oid, structs.NewEvidenceFile("", 0, obj.Size, nil, "", ""), opts)
	if err != nil {
		return nil, obj, fmt.Errorf("object %x: %w", oid, err)
	}
	if obj.ChunkSize > 0 {
		ec.stride = obj.ChunkSize
	}
	return ec, obj, nil
}
