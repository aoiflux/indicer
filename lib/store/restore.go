package store

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/util"
	"math"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v3"
)

func Restore(db *badger.DB, dst *os.File, fid []byte) error {
	if bytes.HasPrefix(fid, []byte(constant.IndexedFileNamespace)) {
		return restoreIndexedFile(fid, dst, db)
	}
	if bytes.HasPrefix(fid, []byte(constant.PartitionFileNamespace)) {
		return restorePartitionFile(fid, dst, db)
	}
	return restoreEvidenceFile(fid, dst, db)
}

func getDBStartOffset(startIndex int64) int64 {
	if startIndex == 0 {
		return 0
	}

	ans := float64(startIndex) / float64(constant.ChonkSize)
	ans = math.Floor(ans)

	offset := int64(ans) * constant.ChonkSize
	return offset
}

func getEvidenceFileHash(fname string) ([]byte, error) {
	eviFileHashString := strings.Split(fname, constant.FilePathSeperator)[0]
	eviFileHash, err := base64.StdEncoding.DecodeString(eviFileHashString)
	if err != nil {
		return nil, err
	}
	return eviFileHash, err
}
func getEvidenceFileID(eviFileHash []byte) []byte {
	return append([]byte(constant.EvidenceFileNamespace), eviFileHash...)
}
func checkCompleted(ehash []byte, db *badger.DB) error {
	eid := getEvidenceFileID(ehash)
	eviFile, err := getEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	if !eviFile.Completed {
		return constant.ErrIncompleteFile
	}
	return nil
}

func restoreIndexedFile(fid []byte, dst *os.File, db *badger.DB) error {
	indexedFile, err := getIndexedFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := getEvidenceFileHash(indexedFile.Names[0])
	if err != nil {
		return err
	}
	err = checkCompleted(ehash, db)
	if err != nil {
		return err
	}

	if indexedFile.DBStart == constant.IgnoreVar {
		indexedFile.DBStart = getDBStartOffset(indexedFile.Start)
		err = setFile(fid, indexedFile, db)
		if err != nil {
			return err
		}
	}

	return restoreData(ehash, indexedFile.Start, indexedFile.DBStart, indexedFile.Size, dst, db)
}
func restorePartitionFile(fid []byte, dst *os.File, db *badger.DB) error {
	partitionFile, err := getPartitionFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := getEvidenceFileHash(partitionFile.Names[0])
	if err != nil {
		return err
	}
	err = checkCompleted(ehash, db)
	if err != nil {
		return err
	}

	if partitionFile.DBStart == constant.IgnoreVar {
		partitionFile.DBStart = getDBStartOffset(partitionFile.Start)
		err = setFile(fid, partitionFile, db)
		if err != nil {
			return err
		}
	}

	return restoreData(ehash, partitionFile.Start, partitionFile.DBStart, partitionFile.Size, dst, db)
}
func restoreEvidenceFile(fid []byte, dst *os.File, db *badger.DB) error {
	evidenceFile, err := getEvidenceFile(fid, db)
	if err != nil {
		return err
	}
	if !evidenceFile.Completed {
		return constant.ErrIncompleteFile
	}
	ehash := bytes.Split(fid, []byte(constant.EvidenceFileNamespace))[1]
	return restoreData(ehash, evidenceFile.Start, 0, evidenceFile.Size, dst, db)
}

func restoreData(ehash []byte, start, dbstart, size int64, dst *os.File, db *badger.DB) error {
	end := start + size

	for restoreIndex := dbstart; ; restoreIndex += constant.ChonkSize {
		if restoreIndex > end {
			break
		}

		relKey := util.AppendToBytesSlice(constant.RelationNapespace, ehash, constant.PipeSeperator, restoreIndex)
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
		}
		if size < int64(len(data)) {
			data = data[:size]
		} else if (restoreIndex + constant.ChonkSize) > end {
			actualEnd := end - restoreIndex
			data = data[:actualEnd]
		}

		_, err = dst.Write(data)
		if err != nil {
			return err
		}
	}

	fmt.Println("Restored file with size: ", size)
	return nil
}
