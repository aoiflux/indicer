package store

import (
	"errors"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// ErrMaterialMissing reports a FLUSHED ingest whose relations or chunks are
// not all in the database, so it cannot be completed. Any other validation
// error is a failure to read, which says nothing about the ingest.
var ErrMaterialMissing = errors.New("ingest material missing")

// validateChunksPerTxn bounds how many chunks one validation transaction
// checks, so memory stays flat however large the evidence is.
const validateChunksPerTxn = 1024

// validateFlushed is the validation recovery and repair run; tests replace it.
var validateFlushed = ValidateFlushedEvidenceMaterialized

// ValidateFlushedEvidenceMaterialized verifies that all logical relation and
// chunk metadata dependencies required by a FLUSHED ingest are present.
// This prevents auto-completing records that are marked flushed when required
// relation/chunk links are missing. A missing relation or chunk is
// ErrMaterialMissing (ING-10).
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

	cs := cnst.ChonkSize
	end := evidenceFile.Start + evidenceFile.Size
	var blocks *fio.BlockManager
	for batchStart := util.GetDBStartOffset(evidenceFile.Start); batchStart < end; batchStart += validateChunksPerTxn * cs {
		batchEnd := min(end, batchStart+validateChunksPerTxn*cs)
		var notInBadger [][]byte
		err := db.View(func(txn *badger.Txn) error {
			for offset := batchStart; offset < batchEnd; offset += cs {
				relKey := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, offset)
				item, err := txn.Get(relKey)
				if errors.Is(err, badger.ErrKeyNotFound) {
					return fmt.Errorf("%w: no relation at offset %d", ErrMaterialMissing, offset)
				}
				if err != nil {
					return fmt.Errorf("relation at offset %d: %w", offset, err)
				}
				chash, err := item.ValueCopy(nil)
				if err != nil {
					return fmt.Errorf("relation at offset %d: %w", offset, err)
				}
				// Relation values are zstd-compressed outside quick mode.
				if decoded, err := cnst.DECODER.DecodeAll(chash, nil); err == nil {
					chash = decoded
				}
				chonkKey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
				_, err = txn.Get(chonkKey)
				if errors.Is(err, badger.ErrKeyNotFound) {
					notInBadger = append(notInBadger, chonkKey)
					continue
				}
				if err != nil {
					return fmt.Errorf("chunk for offset %d: %w", offset, err)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		// A hierarchical database keeps chunk metadata in its block index.
		for _, chonkKey := range notInBadger {
			if blocks == nil {
				blocks = fio.NewBlockManager(db.Opts().Dir, nil)
			}
			_, _, _, err := blocks.GetChunkMetadata(chonkKey)
			if errors.Is(err, fio.ErrChunkNotIndexed) {
				return fmt.Errorf("%w: chunk %x is neither in the database nor the block index", ErrMaterialMissing, chonkKey[len(cnst.ChonkNamespace):])
			}
			if err != nil {
				return fmt.Errorf("block index: %w", err)
			}
		}
	}
	return nil
}
