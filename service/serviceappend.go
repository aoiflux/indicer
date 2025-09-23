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
func CheckAndAppend(filePath, fileHash string, db *badger.DB) (string, error) {
	efile, err := getEvidenceFile(filePath, fileHash, db)
	if err != nil {
		return "", err
	}

	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)
	return appendFile(eid, filePath, efile, db)
}

func appendFile(eid []byte, filePath string, efile structs.EvidenceFile, db *badger.DB) (string, error) {
	if _, ok := efile.Names[filePath]; ok {
		return cnst.FILE_EXISTS, nil
	}
	efile.Names[filePath] = struct{}{}
	err := dbio.SetFile(eid, efile, db)
	if err != nil {
		return "", err
	}
	return cnst.FILE_APPENDED, nil
}
