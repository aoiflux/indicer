package near

import (
	"bytes"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strconv"
	"strings"

	"github.com/dgraph-io/badger/v3"
)

func countRList(fhash []byte, idmap *structs.ConcMap, rim *structs.RimMap, rlist []structs.ReverseRelation, db *badger.DB, echan chan error) {
	fhash = []byte(base64.StdEncoding.EncodeToString(fhash))
	for _, rev := range rlist {
		err := countEviFile(fhash, idmap, rim, rev, db)
		if err != nil {
			echan <- err
		}
	}
	echan <- nil
}
func countEviFile(fhash []byte, idmap *structs.ConcMap, rim *structs.RimMap, rev structs.ReverseRelation, db *badger.DB) error {
	if bytes.Contains(rev.Value, fhash) {
		return nil
	}

	revhash := bytes.Split(rev.Value, []byte(cnst.RelationNamespace))[1]
	eid, err := getIDFromHash(cnst.EviFileNamespace, string(revhash))
	if err != nil {
		return err
	}
	ridx, _, err := getIndicesFromHash(revhash)
	if err != nil {
		return err
	}
	if vs, ok := rim.Get(ridx); ok {
		for _, v := range vs {
			idmap.Set(string(v), 1)
		}
		return nil
	}

	efile, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	db.RunValueLogGC(0.5)

	if len(efile.InternalObjects) == 0 {
		idmap.Set(string(eid), 1)
		rim.Set(ridx, string(eid))
		return nil
	}
	return countPartiFile(ridx, fhash, eid, efile.InternalObjects, idmap, rim, db)
}
func countPartiFile(ridx int64, fhash, eid []byte, phashes []string, idmap *structs.ConcMap, rim *structs.RimMap, db *badger.DB) error {
	for pindex, phash := range phashes {
		pid, inRange, err := countFile(ridx, cnst.PartiFileNamespace, fhash, []byte(phash), db)
		if err != nil {
			return err
		}

		if !inRange && pindex == len(phashes)-1 {
			idmap.Set(string(eid), 1)
			rim.Set(ridx, string(eid))
			break
		}
		if !inRange {
			continue
		}

		pfile, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			return err
		}
		db.RunValueLogGC(0.5)

		if len(pfile.InternalObjects) == 0 {
			idmap.Set(string(pid), 1)
			rim.Set(ridx, string(pid))
			continue
		}
		err = countIdxFile(ridx, fhash, pid, pfile.InternalObjects, idmap, rim, db)
		if err != nil {
			return err
		}
	}

	return nil
}

func countIdxFile(ridx int64, fhash, pid []byte, ihashes []string, idmap *structs.ConcMap, rim *structs.RimMap, db *badger.DB) error {
	for iindex, ihash := range ihashes {
		iid, inRange, err := countFile(ridx, cnst.IdxFileNamespace, fhash, []byte(ihash), db)
		if err != nil {
			return err
		}

		if !inRange && iindex == len(ihashes)-1 {
			idmap.Set(string(pid), 1)
			rim.Set(ridx, string(pid))
			break
		}
		if !inRange {
			continue
		}

		idmap.Set(string(iid), 1)
		rim.Set(ridx, string(iid))
	}

	return nil
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

	return start, end, nil
}
func isInRange(start, end, index int64) bool {
	return index >= start && index <= end
}
