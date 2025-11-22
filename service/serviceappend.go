package service

import (
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// checkfile function should eventually be updated to check evi, parti, and indexed files
// this change may require a db rearchitecture
// remember this while moving from kvdb only to specialised db model
func CheckAndAppend(filePath, fileHashStr string, db *badger.DB) (structs.EvidenceFile, string, error) {
	efile, err := getEvidenceFile(filePath, fileHashStr, db)
	if err != nil {
		return efile, "", err
	}

	fileHash, err := base64.StdEncoding.DecodeString(fileHashStr)
	if err != nil {
		return efile, "", err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)

	appendded, err := appendFile(eid, filePath, efile, db)
	return efile, appendded, err
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
