package near

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strconv"
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
		id = base64.StdEncoding.EncodeToString([]byte(id))
		id = namespace + cnst.NamespaceSeperator + id
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

		count++
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

		count++
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

		count++
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

func countRevListNear(fhash []byte, idmap map[string]int64, revlist []structs.ReverseRelation, db *badger.DB) error {
	fhash = []byte(base64.StdEncoding.EncodeToString(fhash))
	tempIDMap, err := countEviFile(fhash, revlist, db)
	if err != nil {
		return err
	}

	for id := range tempIDMap {
		if val, ok := idmap[id]; ok {
			val++
			idmap[id] = val
			continue
		}
		idmap[id] = 1
	}

	return nil
}
func countEviFile(fhash []byte, revlist []structs.ReverseRelation, db *badger.DB) (map[string]struct{}, error) {
	idmap := make(map[string]struct{})

	for i, rev := range revlist {
		fmt.Println(i)

		if bytes.Contains(rev.Value, fhash) {
			continue
		}

		revhash := bytes.Split(rev.Value, []byte(cnst.RelationNamespace))[1]
		eid, err := getIDFromHash(cnst.EviFileNamespace, string(revhash))
		if err != nil {
			return nil, err
		}

		ridx, _, err := getIndicesFromHash(revhash)
		if err != nil {
			return nil, err
		}

		efile, err := dbio.GetEvidenceFile(eid, db)
		if err != nil {
			return nil, err
		}
		if len(efile.InternalObjects) == 0 {
			idmap[string(eid)] = struct{}{}
			continue
		}

		tempIDMap, err := countPartiFile(ridx, fhash, eid, efile.InternalObjects, db)
		if err != nil {
			return nil, err
		}
		for k, v := range tempIDMap {
			idmap[k] = v
		}
	}

	return idmap, nil
}
func countPartiFile(ridx int64, fhash, eid []byte, phashes []string, db *badger.DB) (map[string]struct{}, error) {
	idmap := make(map[string]struct{})

	for pindex, phash := range phashes {
		pid, inRange, err := countFile(ridx, cnst.PartiFileNamespace, fhash, []byte(phash), db)
		if err != nil {
			return nil, err
		}

		if !inRange && pindex == len(phashes)-1 {
			idmap[string(eid)] = struct{}{}
			break
		}
		if !inRange {
			continue
		}

		pfile, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			return nil, err
		}
		if len(pfile.InternalObjects) == 0 {
			idmap[string(pid)] = struct{}{}
			continue
		}

		tempIDMap, err := countIdxFile(ridx, fhash, pid, pfile.InternalObjects, db)
		if err != nil {
			return nil, err
		}
		for k, v := range tempIDMap {
			idmap[k] = v
		}
	}

	return idmap, nil
}
func countIdxFile(ridx int64, fhash, pid []byte, ihashes []string, db *badger.DB) (map[string]struct{}, error) {
	idmap := make(map[string]struct{})

	for iindex, ihash := range ihashes {
		iid, inRange, err := countFile(ridx, cnst.IdxFileNamespace, fhash, []byte(ihash), db)
		if err != nil {
			return nil, err
		}

		if !inRange && iindex == len(ihashes)-1 {
			idmap[string(pid)] = struct{}{}
			break
		}
		if !inRange {
			continue
		}

		idmap[string(iid)] = struct{}{}
	}

	return idmap, nil
}

func countFile(ridx int64, namespace string, filter, fhash []byte, db *badger.DB) ([]byte, bool, error) {
	if bytes.Contains(fhash, filter) {
		return nil, false, nil
	}

	id, err := getIDFromHash(namespace, string(fhash))
	if err != nil {
		return nil, false, err
	}
	start, end, err := getIndicesFromHash(fhash)
	if err != nil {
		return nil, false, err
	}

	inRange := isInRange(start, end, ridx)
	return id, inRange, nil
}

func getIDFromHash(namespace, hashStr string) ([]byte, error) {
	hashStr = strings.Split(hashStr, cnst.DataSeperator)[0]
	hash, err := base64.StdEncoding.DecodeString(hashStr)
	if err != nil {
		return nil, err
	}
	return util.AppendToBytesSlice(namespace, hash), nil
}
func getIndicesFromHash(hash []byte) (int64, int64, error) {
	indices := bytes.Split(hash, []byte(cnst.DataSeperator))[1]
	idxlist := bytes.Split(indices, []byte(cnst.RangeSeperator))

	if strings.HasPrefix(string(idxlist[0]), "|") {
		fmt.Println("test")
	}

	start, err := strconv.ParseInt(string(idxlist[0]), 10, 64)
	if err != nil {
		return cnst.IgnoreVar, cnst.IgnoreVar, err
	}
	if len(idxlist) == 1 {
		return start, cnst.IgnoreVar, nil
	}

	end, err := strconv.ParseInt(string(idxlist[1]), 10, 64)
	if err != nil {
		return cnst.IgnoreVar, cnst.IgnoreVar, err
	}

	if strings.HasPrefix(string(idxlist[0]), "|") {
		fmt.Println("test")
	}

	return start, end, nil
}
func isInRange(start, end, index int64) bool {
	return index >= start && index <= end
}
