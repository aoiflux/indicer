package service

import (
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"log"

	"github.com/dgraph-io/badger/v4"
)

func getEvidenceFile(filePath, fileHashStr string, db *badger.DB) (structs.EvidenceFile, error) {
	var efile structs.EvidenceFile
	fileHash, err := base64.StdEncoding.DecodeString(fileHashStr)
	if err != nil {
		return efile, err
	}
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, fileHash)

	efile, err = dbio.GetEvidenceFile(eid, db)
	if err != nil {
		if err == badger.ErrKeyNotFound {
			log.Printf("File [%s] not found in DB. Proceeding with dedup function.", filePath)
			return efile, cnst.ErrFileNotFound
		}
		return efile, err
	}
	if !efile.Completed {
		log.Printf("File [%s] is incomplete", filePath)
		return efile, cnst.ErrFileNotFound
	}
	return efile, nil
}

func getChunkMap(meta structs.FileMeta, db *badger.DB) (map[string]int64, error) {
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	chunkMap := make(map[string]int64)
	for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return nil, err
		}

		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		data, err := dbio.GetChonkData(restoreIndex, meta.Start, meta.Size, dbstart, end, ckey, db)
		if err != nil {
			return nil, err
		}

		chashStr := base64.StdEncoding.EncodeToString(chash)
		chunkMap[chashStr] = int64(len(data))
	}

	return chunkMap, nil
}
