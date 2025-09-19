package service

import (
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// checkfile function should eventually be updated to check evi, parti, and indexed files
// this change may require a db rearchitecture
// remember this while moving from kvdb only to specialised db model
func checkFile(fileHash string, db *badger.DB) (structs.EvidenceFile, error) {
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)
	efile, err := dbio.GetEvidenceFile(eid, db)

	if err != nil {
		return efile, cnst.ErrFileNotFound
	}
	if !efile.Completed {
		return efile, cnst.ErrFileNotFound
	}

	return efile, nil
}
