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
	ehash, err := util.GetEvidenceFileHash(ifile.Names[0])
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	if ifile.DBStart == cnst.IgnoreVar {
		ifile.DBStart = util.GetDBStartOffset(ifile.Start)
		err = dbio.SetFile(fid, ifile, db)
		if err != nil {
			return nil, cnst.IgnoreVar, err
		}
	}
	ihash := bytes.Split(fid, []byte(cnst.IdxFileNamespace))[1]

	idmap := make(map[string]int64)
	var count int64
	for near := range getNear(ifile.Start, ifile.DBStart, ifile.Size, ehash, db) {
		count++

		if near.Err != nil {
			return nil, cnst.IgnoreVar, err
		}
		if len(near.RevList) == 1 {
			continue
		}

		err = countRevListNear(ihash, idmap, near.RevList, db)
		if err != nil {
			return nil, cnst.IgnoreVar, err
		}
	}

	return idmap, count, nil
}
func nearPartitionFile(fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	pfile, err := dbio.GetPartitionFile(fid, db)
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	ehash, err := util.GetEvidenceFileHash(pfile.Names[0])
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	if pfile.DBStart == cnst.IgnoreVar {
		pfile.DBStart = util.GetDBStartOffset(pfile.Start)
		err = dbio.SetFile(fid, pfile, db)
		if err != nil {
			return nil, cnst.IgnoreVar, err
		}
	}
	phash := bytes.Split(fid, []byte(cnst.PartiFileNamespace))[1]

	idmap := make(map[string]int64)
	var count int64
	for near := range getNear(pfile.Start, pfile.DBStart, pfile.Size, ehash, db) {
		count++

		if near.Err != nil {
			return nil, cnst.IgnoreVar, err
		}
		if len(near.RevList) == 1 {
			continue
		}

		err = countRevListNear(phash, idmap, near.RevList, db)
		if err != nil {
			return nil, cnst.IgnoreVar, err
		}
	}

	return idmap, count, nil
}
func nearEvidenceFile(fid []byte, db *badger.DB) (map[string]int64, int64, error) {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return nil, cnst.IgnoreVar, err
	}
	ehash := bytes.Split(fid, []byte(cnst.EviFileNamespace))[1]

	idmap := make(map[string]int64)
	var count int64
	for near := range getNear(efile.Start, 0, efile.Size, ehash, db) {
		count++

		if near.Err != nil {
			return nil, cnst.IgnoreVar, err
		}
		if len(near.RevList) == 1 {
			continue
		}

		err = countRevListNear(ehash, idmap, near.RevList, db)
		if err != nil {
			return nil, cnst.IgnoreVar, err
		}
	}

	return idmap, count, nil
}

func getNear(start, dbstart, size int64, ehash []byte, db *badger.DB) chan structs.NearGen {
	neargenChan := make(chan structs.NearGen)

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
