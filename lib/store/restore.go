package store

import (
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"os"

	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/schollz/progressbar/v3"
)

func Restore(fhash string, dst *os.File, db *badger.DB) error {
	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}
	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		return restoreIndexedFile(fid, dst, db)
	}
	if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
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
		return cnst.ErrIncompleteFile
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
	return restoreData(indexedFile.Start, indexedFile.Size, ehash, dst, db)
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
	return restoreData(partitionFile.Start, partitionFile.Size, ehash, dst, db)
}
func restoreEvidenceFile(fid []byte, dst *os.File, db *badger.DB) error {
	evidenceFile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return err
	}
	if !evidenceFile.Completed {
		return cnst.ErrIncompleteFile
	}
	ehash := bytes.Split(fid, []byte(cnst.EviFileNamespace))[1]
	return restoreData(evidenceFile.Start, evidenceFile.Size, ehash, dst, db)
}

func restoreData(start, size int64, ehash []byte, dst *os.File, db *badger.DB) error {
	var dbstart int64
	if start > 0 {
		dbstart = util.GetDBStartOffset(start)
	}
	end := start + size

	bar := progressbar.DefaultBytes(size)
	for restoreIndex := dbstart; restoreIndex <= end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		data, err := dbio.GetChonkNode(ckey, db)
		if err != nil {
			return err
		}

		if restoreIndex == dbstart {
			actualStart := start - restoreIndex
			data = data[actualStart:]
		}
		if size < int64(len(data)) {
			data = data[:size]
		} else if (restoreIndex + cnst.ChonkSize) > end {
			actualEnd := end - restoreIndex
			data = data[:actualEnd]
		}

		_, err = dst.Write(data)
		if err != nil {
			return err
		}

		bar.Add64(cnst.ChonkSize)
	}

	bar.Finish()
	fmt.Println("Restored file with size: ", humanize.Bytes(uint64(size)))
	return nil
}
