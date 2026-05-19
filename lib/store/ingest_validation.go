package store

import (
	"bytes"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// ValidateFlushedEvidenceMaterialized verifies that all logical relation and
// chunk metadata dependencies required by a FLUSHED ingest are present.
// This prevents auto-completing records that are marked flushed when required
// relation/chunk links are missing.
func ValidateFlushedEvidenceMaterialized(fileID []byte, evidenceFile structs.EvidenceFile, db *badger.DB) error {
	if evidenceFile.Completed {
		return nil
	}
	if evidenceFile.IngestState != structs.IngestStateFlushed {
		return fmt.Errorf("invalid state for flushed validation: %s", evidenceFile.IngestState)
	}
	if evidenceFile.Size <= 0 {
		return nil
	}

	ehash := bytes.TrimPrefix(fileID, []byte(cnst.EviFileNamespace))
	if len(ehash) == len(fileID) {
		return fmt.Errorf("invalid evidence id namespace")
	}

	end := evidenceFile.Start + evidenceFile.Size
	dbstart := util.GetDBStartOffset(evidenceFile.Start)

	// Pass 1: iterate the relation prefix in a single transaction, collecting
	// chunk hashes in index order. This replaces one db.View call per chunk.
	chunkCount := int((end - dbstart + cnst.ChonkSize - 1) / cnst.ChonkSize)
	chashes := make([][]byte, 0, chunkCount)

	if err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = chunkCount
		it := txn.NewIterator(opts)
		defer it.Close()

		for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
			relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, restoreIndex)
			item, err := txn.Get(relKey)
			if err != nil {
				return fmt.Errorf("relation lookup failed at index %d: %w", restoreIndex, err)
			}
			chash, err := item.ValueCopy(nil)
			if err != nil {
				return fmt.Errorf("relation value copy failed at index %d: %w", restoreIndex, err)
			}
			chashes = append(chashes, chash)
		}
		return nil
	}); err != nil {
		return err
	}

	// Pass 2: verify each chunk node exists in a single transaction.
	return db.View(func(txn *badger.Txn) error {
		for i, chash := range chashes {
			chonkKey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
			if _, err := txn.Get(chonkKey); err != nil {
				return fmt.Errorf("chunk lookup failed at chunk %d: %w", i, err)
			}
		}
		return nil
	})
}
