package store

import (
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
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
	meta, err := GetFileMeta(fid, db)
	if err != nil {
		return err
	}
	return restoreData(meta, dst, db)
}

func GetFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		return getIndexedFileMeta(fid, db)
	}
	if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		return getPartitionFileMeta(fid, db)
	}
	return getEvidenceFileMeta(fid, db)
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

func getIndexedFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	var meta structs.FileMeta

	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return meta, err
	}
	ehash, err := getLogicalFileEviHash(ifile.Names, db)
	if err != nil {
		return meta, err
	}

	meta.EviHash = ehash
	meta.Start = ifile.Start
	meta.Size = ifile.Size
	return meta, nil
}
func getPartitionFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	var meta structs.FileMeta

	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return meta, err
	}
	ehash, err := getLogicalFileEviHash(pfile.Names, db)
	if err != nil {
		return meta, err
	}

	meta.EviHash = ehash
	meta.Start = pfile.Start
	meta.Size = pfile.Size
	return meta, nil
}
func getLogicalFileEviHash(names map[string]struct{}, db *badger.DB) ([]byte, error) {
	name := util.GetArbitratyMapKey(names)
	ehash, err := util.GetEvidenceFileHash(name)
	if err != nil {
		return nil, err
	}
	err = checkCompleted(ehash, db)
	if err != nil {
		return nil, err
	}
	return ehash, nil
}

func getEvidenceFileMeta(fid []byte, db *badger.DB) (structs.FileMeta, error) {
	var meta structs.FileMeta

	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return meta, err
	}
	if !efile.Completed {
		return meta, cnst.ErrIncompleteFile
	}
	ehash := bytes.Split(fid, []byte(cnst.EviFileNamespace))[1]

	meta.EviHash = ehash
	meta.Start = efile.Start
	meta.Size = efile.Size
	return meta, nil
}

func restoreData(meta structs.FileMeta, dst *os.File, db *badger.DB) error {
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	bar := progressbar.DefaultBytes(meta.Size)
	for restoreIndex := dbstart; restoreIndex < end; restoreIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, restoreIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		data, err := dbio.GetChonkData(restoreIndex, meta.Start, meta.Size, dbstart, ckey, db)
		if err != nil {
			return err
		}

		_, err = dst.Write(data)
		if err != nil {
			return err
		}

		bar.Add64(cnst.ChonkSize)
	}

	bar.Finish()
	fmt.Println("Restored file with size: ", humanize.Bytes(uint64(meta.Size)))
	return bar.Close()
}
