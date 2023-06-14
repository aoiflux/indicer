package near

import (
	"bytes"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strconv"

	"github.com/dgraph-io/badger/v3"
)

func NearInFile(fhash string, db *badger.DB) error {
	fid, err := dbio.GuessFileType(fhash, db)
	if err != nil {
		return err
	}
	if bytes.HasPrefix(fid, []byte(cnst.IdxFileNamespace)) {
		return nearIndexFile(fid, db)
	}
	if bytes.HasPrefix(fid, []byte(cnst.PartiFileNamespace)) {
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
	if ifile.DBStart == cnst.IgnoreVar {
		ifile.DBStart = util.GetDBStartOffset(ifile.Start)
		err = dbio.SetFile(fid, ifile, db)
		if err != nil {
			return err
		}
	}
	ihash := bytes.Split(fid, []byte(cnst.IdxFileNamespace))[1]
	fmt.Println(ihash)

	for near := range getNear(ifile.Start, ifile.DBStart, ifile.Size, ehash, db) {
		if near.Err != nil {
			return err
		}
		if len(near.RevList) == 1 {
			continue
		}
		fmt.Println(len(near.RevList))
	}

	return nil
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
	if pfile.DBStart == cnst.IgnoreVar {
		pfile.DBStart = util.GetDBStartOffset(pfile.Start)
		err = dbio.SetFile(fid, pfile, db)
		if err != nil {
			return err
		}
	}
	phash := bytes.Split(fid, []byte(cnst.PartiFileNamespace))[1]
	fmt.Println(phash)

	for near := range getNear(pfile.Start, pfile.DBStart, pfile.Size, ehash, db) {
		if near.Err != nil {
			return err
		}
		if len(near.RevList) == 1 {
			continue
		}
		fmt.Println(len(near.RevList))
	}

	return nil
}
func nearEvidenceFile(fid []byte, db *badger.DB) error {
	efile, err := dbio.GetEvidenceFile(fid, db)
	if err != nil {
		return err
	}
	ehash := bytes.Split(fid, []byte(cnst.EviFileNamespace))[1]

	hashMap := make(map[string]int64)
	for near := range getNear(efile.Start, 0, efile.Size, ehash, db) {
		if near.Err != nil {
			return err
		}
		if len(near.RevList) == 1 {
			continue
		}

		hashList, err := countEviFile(ehash, near.RevList, db)
		if err != nil {
			return err
		}

		for _, hash := range hashList {
			if count, ok := hashMap[hash]; ok {
				count++
				hashMap[hash] = count
			}
		}
	}

	return nil
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

func countEviFile(ehash []byte, revlist []structs.ReverseRelation, db *badger.DB) ([]string, error) {
	for _, rev := range revlist {
		if bytes.Contains(rev.Value, ehash) {
			continue
		}

		revhash := bytes.Split(rev.Value, []byte(cnst.RelationNamespace))[1]
		splits := bytes.Split(revhash, []byte(cnst.DataSeperator))
		revhash = splits[0]
		idxstr := fmt.Sprintf("%v", string(splits[1]))
		index, err := strconv.ParseInt(idxstr, 10, 64)
		if err != nil {
			return nil, err
		}
		eid := util.AppendToBytesSlice(cnst.EviFileNamespace, revhash)

		// db call evi
		// loop over internal objects
		// if no parti then update list -- for evi
		// range check
		// if range check fail then update list -- for evi
		//// db call parti
		//// loop over internal objects
		//// if no idx then update list -- for parti
		//// range check
		//// if range check fail then update list -- for parti
		//// if range check success then update list -- for idx

		fmt.Println(string(eid), index)
	}
	return nil, nil
}
func countPartiFile(phash []byte, revlist []structs.ReverseRelation) error { return nil }
func countIdxFile(ihash []byte, revlist []structs.ReverseRelation) error   { return nil }
