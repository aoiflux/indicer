package store

import (
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func processRel(index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fhash, cnst.DataSeperator, index)

	// Relation keys are deterministic per (file hash, chunk index), so an upsert is
	// safe and avoids an extra DB read on the hot ingest path.
	return dbio.SetBatchNode(db, relKey, chash, batch)
}

// processRevRel records, when revrel is set, that chunk chash occurs at index
// in fhash, as one key of its own. Nothing is buffered or merged, so memory
// does not grow with the evidence and concurrent ingests never overwrite each
// other's relations (STO-14).
func processRevRel(revrel bool, index int64, fhash, chash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	if !revrel {
		return nil
	}
	return dbio.SetReverseRelationAppendMember(db, chash, index, fhash, batch)
}
