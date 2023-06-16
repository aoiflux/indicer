package near

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strings"

	"github.com/dgraph-io/badger/v3"
)

func NearInFile(fhash string, db *badger.DB) error {
	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}

	var idmap map[string]int64
	var count int64
	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		idmap, count, err = nearIndexFile(fid, db)
	} else if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
		idmap, count, err = nearPartitionFile(fid, db)
	} else {
		idmap, count, err = nearEvidenceFile(fid, db)
	}
	if err != nil {
		return err
	}

	for id, val := range idmap {
		namespace := strings.Split(id, cnst.NamespaceSeperator)[0]
		hash := strings.Split(id, cnst.NamespaceSeperator)[1]
		hash = base64.StdEncoding.EncodeToString([]byte(hash))
		id = namespace + cnst.NamespaceSeperator + hash
		fmt.Println(id, val)
	}
	fmt.Println("Total Chonk Count: ", count)

	return nil
}

func nearIndexFile(fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	ifile, err := dbio.GetIndexedFile(fid, db)
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	return getNearLogicalFile(ifile.Start, ifile.Size, ifile.Names[0], fid, db)
}
func nearPartitionFile(fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	return getNearLogicalFile(pfile.Start, pfile.Size, pfile.Names[0], fid, db)
}
func nearEvidenceFile(fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	ehash := bytes.Split(fid, []byte(cnst.NamespaceSeperator))[1]
	return getNearFile(efile.Start, efile.Size, ehash, fid, db)
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
