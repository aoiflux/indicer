package store

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/constant"
	"math"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v3"
)

func Restore(db *badger.DB, dst *os.File, fid []byte) error {
	if bytes.HasPrefix(fid, []byte(constant.IndexedFileNamespace)) {
		return restoreIndexedFile(db, dst, fid)
	}

	if bytes.HasPrefix(fid, []byte(constant.PartitionFileNamespace)) {
	}

	return nil
}

func getDBStartChonk(startIndex int64) int64 {
	if startIndex == 0 {
		return 0
	}

	ans := float64(startIndex) / float64(constant.ChonkSize)
	ans = math.Floor(ans)

	return int64(ans)
}
func getEvidenceFileID(fname string) ([]byte, error) {
	eviFileHashString := strings.Split(fname, constant.FilePathSeperator)[0]
	eviFileHash, err := base64.StdEncoding.DecodeString(eviFileHashString)
	if err != nil {
		return nil, err
	}
	return append([]byte(constant.EvidenceFileNamespace), eviFileHash...), nil
}
func checkCompleted(eid []byte, db *badger.DB) error {
	eviFile, err := getEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	if !eviFile.Completed {
		return constant.ErrIncompleteFile
	}
	return nil
}

func restoreIndexedFile(db *badger.DB, dst *os.File, fid []byte) error {
	indexedFile, err := getIndexedFile(fid, db)
	if err != nil {
		return err
	}
	eid, err := getEvidenceFileID(indexedFile.Names[0])
	if err != nil {
		return err
	}
	err = checkCompleted(eid, db)
	if err != nil {
		return err
	}

	if indexedFile.DBStart == constant.IgnoreVar {
		dbstart := getDBStartChonk(indexedFile.Start)
		indexedFile.DBStart = dbstart
		err = setFile(fid, indexedFile, db)
		if err != nil {
			return err
		}
	}

	return restoreData(eid, indexedFile.Start, indexedFile.DBStart, indexedFile.Size, dst, db)
}
func restorePartitionFile() {}
func restoreEvidenceFile()  {}

func restoreData(eid []byte, start, dbstart, size int64, dst *os.File, db *badger.DB) error {
	end := start + size

	for restoreIndex := dbstart; ; restoreIndex += constant.ChonkSize {
		if restoreIndex > end {
			break
		}

		relKeyString := fmt.Sprintf("%b%s%d", eid, constant.PipeSeperator, restoreIndex)
		relKey := []byte(relKeyString)
		chash, err := getNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := append([]byte(constant.ChonkNamespace), chash...)
		data, err := getNode(ckey, db)
		if err != nil {
			return err
		}

		if restoreIndex == dbstart {
			actualStart := start - restoreIndex
			data = data[actualStart:]
		} else if (restoreIndex + constant.ChonkSize) > size {
			actualEnd := end - restoreIndex
			data = data[:actualEnd]
		}

		_, err = dst.Write(data)
		if err != nil {
			return err
		}
	}

	return nil
}
