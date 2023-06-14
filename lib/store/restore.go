package store

import (
	"bytes"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"os"

	"github.com/dgraph-io/badger/v3"
)

func Restore(fhash string, dst *os.File, db *badger.DB) error {
	fid, err := util.GuessFileType(fhash, db)
	if err != nil {
		return err
	}
	if bytes.HasPrefix(fid, []byte(constant.IndexedFileNamespace)) {
		return restoreIndexedFile(fid, dst, db)
	}
	if bytes.HasPrefix(fid, []byte(constant.PartitionFileNamespace)) {
		return restorePartitionFile(fid, dst, db)
	}
	return restoreEvidenceFile(fid, dst, db)
}

func checkCompleted(ehash []byte, db *badger.DB) error {
	eid := util.GetEvidenceFileID(ehash)
	eviFile, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	if !eviFile.Completed {
		return constant.ErrIncompleteFile
	}
	return nil
}

func restoreIndexedFile(fid []byte, dst *os.File, db *badger.DB) error {
	indexedFile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := util.GetEvidenceFileHash(indexedFile.Names[0])
	if err != nil {
		return err
	}
	err = checkCompleted(ehash, db)
	if err != nil {
		return err
	}

	if indexedFile.DBStart == constant.IgnoreVar {
		indexedFile.DBStart = util.GetDBStartOffset(indexedFile.Start)
		err = dbio.SetFile(fid, indexedFile, db)
		if err != nil {
			return err
		}
	}

	return restoreData(indexedFile.Start, indexedFile.DBStart, indexedFile.Size, ehash, dst, db)
}
func restorePartitionFile(fid []byte, dst *os.File, db *badger.DB) error {
	partitionFile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := util.GetEvidenceFileHash(partitionFile.Names[0])
	if err != nil {
		return err
	}
	err = checkCompleted(ehash, db)
	if err != nil {
		return err
	}

	if partitionFile.DBStart == constant.IgnoreVar {
		partitionFile.DBStart = util.GetDBStartOffset(partitionFile.Start)
		err = dbio.SetFile(fid, partitionFile, db)
		if err != nil {
			return err
		}
	}

	return restoreData(partitionFile.Start, partitionFile.DBStart, partitionFile.Size, ehash, dst, db)
}
func restoreEvidenceFile(fid []byte, dst *os.File, db *badger.DB) error {
	evidenceFile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return err
	}
	if !evidenceFile.Completed {
		return constant.ErrIncompleteFile
	}
	ehash := bytes.Split(fid, []byte(constant.EvidenceFileNamespace))[1]
	return restoreData(evidenceFile.Start, 0, evidenceFile.Size, ehash, dst, db)
}

func restoreData(start, dbstart, size int64, ehash []byte, dst *os.File, db *badger.DB) error {
	end := start + size

	for restoreIndex := dbstart; restoreIndex <= end; restoreIndex += constant.ChonkSize {
		relKey := util.AppendToBytesSlice(constant.RelationNapespace, ehash, constant.PipeSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := util.AppendToBytesSlice(constant.ChonkNamespace, chash)
		data, err := dbio.GetNode(ckey, db)
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
