package store

import (
	"bytes"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// ValidateFlushedEvidenceMaterialized verifies that all logical chunk relations
// and chunk payloads required by a FLUSHED ingest are readable from storage.
// This prevents auto-completing records that are marked flushed but missing
// sidecar/container content.
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

	for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return fmt.Errorf("relation lookup failed at index %d: %w", restoreIndex, err)
		}

		chonkKey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		if _, err := dbio.GetChonkNode(chonkKey, db); err != nil {
			return fmt.Errorf("chunk lookup failed at index %d: %w", restoreIndex, err)
		}
	}

	return nil
}
