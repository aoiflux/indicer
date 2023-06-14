package near

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v3"
)

func NearInFile(fhash string, db *badger.DB) error {
	fid, err := util.GuessFileType(fhash, db)
	if err != nil {
		return err
	}

	if bytes.HasPrefix(fid, []byte(cnst.IndexedFileNamespace)) {
		return nearIndexFile(fid, db)
	}
	if bytes.HasPrefix(fid, []byte(cnst.PartitionFileNamespace)) {
		return nearPartitionFile(fid, db)
	}
	return nearEvidenceFile(fid, db)
}

func nearIndexFile(fid []byte, db *badger.DB) error {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return err
	}
	// ehash, err := util.GetEvidenceFileHash(ifile.Names[0])
	// if err != nil {
	// 	return err
	// }
	if ifile.DBStart == cnst.IgnoreVar {
		ifile.DBStart = util.GetDBStartOffset(ifile.Start)
		err = dbio.SetFile(fid, ifile, db)
		if err != nil {
			return err
		}
	}
	// return getNear(ifile.Start, ifile.DBStart, ifile.Size, ehash, db)
	return nil
}
func nearPartitionFile(fid []byte, db *badger.DB) error {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return err
	}
	// ehash, err := util.GetEvidenceFileHash(pfile.Names[0])
	// if err != nil {
	// 	return err
	// }
	if pfile.DBStart == cnst.IgnoreVar {
		pfile.DBStart = util.GetDBStartOffset(pfile.Start)
		err = dbio.SetFile(fid, pfile, db)
		if err != nil {
			return err
		}
	}
	// return getNear(pfile.Start, pfile.DBStart, pfile.Size, ehash, db)
	return nil
}
func nearEvidenceFile(fid []byte, db *badger.DB) error {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return err
	}
	ehash := bytes.Split(fid, []byte(cnst.EvidenceFileNamespace))[1]
	return getNear(cnst.EviFType, efile.Start, 0, efile.Size, ehash, db)
}

func getNear(ftype string, start, dbstart, size int64, ehash []byte, db *badger.DB) error {
	end := start + size

	for nearIndex := dbstart; nearIndex <= end; nearIndex += cnst.ChonkSize {
		relKey := util.AppendToBytesSlice(cnst.RelationNapespace, ehash, cnst.PipeSeperator, nearIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash)
		revlist, err := dbio.GetReverseRelationNode(ckey, db)
		if err != nil {
			return err
		}

		fmt.Printf("{%s : %d}\n", base64.StdEncoding.EncodeToString(ckey), len(revlist))
	}

	return nil
}
