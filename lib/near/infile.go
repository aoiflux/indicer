package near

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/constant"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v3"
)

func NearInFile(fhash string, db *badger.DB) error {
	fid, err := util.GuessFileType(fhash, db)
	if err != nil {
		return err
	}

	if bytes.HasPrefix(fid, []byte(constant.IndexedFileNamespace)) {
		return nearIndexFile(fid, db)
	}
	if bytes.HasPrefix(fid, []byte(constant.PartitionFileNamespace)) {
		return nearPartitionFile(fid, db)
	}
	return nearEvidenceFile(fid, db)
}

func nearIndexFile(fid []byte, db *badger.DB) error {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := util.GetEvidenceFileHash(ifile.Names[0])
	if err != nil {
		return err
	}
	if ifile.DBStart == constant.IgnoreVar {
		ifile.DBStart = util.GetDBStartOffset(ifile.Start)
		err = dbio.SetFile(fid, ifile, db)
		if err != nil {
			return err
		}
	}
	return getNear(ifile.Start, ifile.DBStart, ifile.Size, ehash, db)
}
func nearPartitionFile(fid []byte, db *badger.DB) error {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := util.GetEvidenceFileHash(pfile.Names[0])
	if err != nil {
		return err
	}
	if pfile.DBStart == constant.IgnoreVar {
		pfile.DBStart = util.GetDBStartOffset(pfile.Start)
		err = dbio.SetFile(fid, pfile, db)
		if err != nil {
			return err
		}
	}
	return getNear(pfile.Start, pfile.DBStart, pfile.Size, ehash, db)
}
func nearEvidenceFile(fid []byte, db *badger.DB) error {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return err
	}
	ehash := bytes.Split(fid, []byte(constant.EvidenceFileNamespace))[1]
	return getNear(efile.Start, 0, efile.Size, ehash, db)
}

func getNear(start, dbstart, size int64, ehash []byte, db *badger.DB) error {
	end := start + size

	for nearIndex := dbstart; nearIndex <= end; nearIndex += constant.ChonkSize {
		relKey := util.AppendToBytesSlice(constant.RelationNapespace, ehash, constant.PipeSeperator, nearIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := util.AppendToBytesSlice(constant.ReverseRelationNamespace, chash)
		revlist, err := dbio.GetReverseRelationNode(ckey, db)
		if err != nil {
			return err
		}

		fmt.Printf("{%s : %d}\n", base64.StdEncoding.EncodeToString(ckey), len(revlist))
	}

	return nil
}
