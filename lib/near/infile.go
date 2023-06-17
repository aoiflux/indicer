package near

import (
	"bytes"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v3"
)

func NearInFile(fhash string, db *badger.DB) error {
	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}

	var idmap map[string]int64

	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		idmap, err = nearIndexFile(fid, db)
	} else if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		idmap, err = nearPartitionFile(fid, db)
	} else {
		idmap, err = nearEvidenceFile(fid, db)
	}
	if err != nil {
		return err
	}

	return visualise(fid, idmap, db)
}

func nearIndexFile(fid []byte, db *badger.DB) (map[string]int64, error) {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return nil, err
	}
	idmap, _, err := getNearLogicalFile(ifile.Start, ifile.Size, ifile.Names[0], fid, db)
	return idmap, err
}
func nearPartitionFile(fid []byte, db *badger.DB) (map[string]int64, error) {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return nil, err
	}
	idmap, _, err := getNearLogicalFile(pfile.Start, pfile.Size, pfile.Names[0], fid, db)
	return idmap, err
}
func nearEvidenceFile(fid []byte, db *badger.DB) (map[string]int64, error) {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return nil, err
	}
	ehash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	idmap, _, err := getNearFile(efile.Start, efile.Size, ehash, fid, db)
	return idmap, err
}

func getNearLogicalFile(start, size int64, fname string, fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	ehash, err := util.GetEvidenceFileHash(fname)
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	return getNearFile(start, size, ehash, fid, db)
}
func getNearFile(start, size int64, ehash, fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	fhash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	idmap := make(map[string]int64)
	var count int64

	for near := range getNear(start, size, ehash, db) {
		count++

		if near.Err != nil {
			return nil, cnst.IgnoreVar, near.Err
		}
		if len(near.RevList) == 1 {
			continue
		}

		err := countRevListNear(fhash, idmap, near.RevList, db)
		if err != nil {
			return nil, cnst.IgnoreVar, err
		}
	}

	return idmap, count, nil
}
func getNear(start, size int64, ehash []byte, db *badger.DB) chan structs.NearGen {
	neargenChan := make(chan structs.NearGen)

	var dbstart int64
	if start > 0 {
		dbstart = util.GetDBStartOffset(start)
	}

	go func() {
		defer close(neargenChan)

		end := start + size
		var neargen structs.NearGen

		for nearIndex := dbstart; nearIndex <= end; nearIndex += cnst.ChonkSize {
			relKey := util.AppendToBytesSlice(cnst.RelationNamespace, ehash, cnst.DataSeperator, nearIndex)
			chash, err := dbio.GetNode(relKey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
			}

			ckey := util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash)
			revlist, err := dbio.GetReverseRelationNode(ckey, db)
			if err != nil {
				neargen.Err = err
				neargenChan <- neargen
			}

			neargen.RevList = revlist
			neargenChan <- neargen
		}
	}()

	return neargenChan
}
