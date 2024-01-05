package near

import (
	"bytes"
	"encoding/base64"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func countRList(inputHash []byte, idmap *structs.ConcMap, near structs.NearGen, db *badger.DB, echan chan error) {
	for revhash := range near.RevMap {
		if bytes.Equal(inputHash, []byte(revhash)) {
			continue
		}

		err := countEviFile(near.Index, near.Confidence, inputHash, []byte(revhash), idmap, db)
		if err != nil {
			echan <- err
			return
		}
	}
	echan <- nil
}
func countEviFile(index int64, confidence float64, inputHash, revhash []byte, idmap *structs.ConcMap, db *badger.DB) error {
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, revhash)
	efile, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	db.RunValueLogGC(0.5)

	if len(efile.InternalObjects) == 0 {
		idmap.Set(string(eid), confidence)
		return nil
	}
	return countPartiFile(confidence, index, inputHash, eid, efile.InternalObjects, idmap, db)
}
func countPartiFile(confidence float64, ridx int64, inputHash, eid []byte, phashes map[string]structs.InternalOffset, idmap *structs.ConcMap, db *badger.DB) error {
	var pindex int
	for phash, offset := range phashes {
		pid, inRange, err := countFile(ridx, cnst.PartiFileNamespace, inputHash, []byte(phash), offset, db)
		if err != nil {
			return err
		}

		if !inRange && pindex == len(phashes)-1 {
			idmap.Set(string(eid), confidence)
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
			idmap.Set(string(pid), confidence)
			continue
		}
		err = countIdxFile(confidence, ridx, inputHash, pid, pfile.InternalObjects, idmap, db)
		if err != nil {
			return err
		}
		pindex++
	}

	return nil
}

func countIdxFile(confidence float64, ridx int64, inputHash, pid []byte, ihashes map[string]structs.InternalOffset, idmap *structs.ConcMap, db *badger.DB) error {
	var iindex int
	for ihash, offset := range ihashes {
		iid, inRange, err := countFile(ridx, cnst.IdxFileNamespace, inputHash, []byte(ihash), offset, db)
		if err != nil {
			return err
		}

		if !inRange && iindex == len(ihashes)-1 {
			idmap.Set(string(pid), confidence)
			break
		}
		if !inRange {
			continue
		}

		idmap.Set(string(iid), confidence)
		iindex++
	}

	return nil
}
func countFile(ridx int64, namespace string, inputHash, fhash []byte, offset structs.InternalOffset, db *badger.DB) ([]byte, bool, error) {
	if bytes.Equal(fhash, inputHash) {
		return nil, false, nil
	}

	id, err := getIDFromHash(namespace, string(fhash))
	if err != nil {
		return nil, false, nil
	}

	inRange := isInRange(offset.Start, offset.End, ridx)
	return id, inRange, nil
}

func getIDFromHash(namespace, hashStr string) ([]byte, error) {
	hash, err := base64.StdEncoding.DecodeString(hashStr)
	if err != nil {
		return nil, err
	}
	return util.AppendToBytesSlice(namespace, hash), nil
}

func isInRange(start, end, index int64) bool {
	return index >= start && index <= end
}

func partialChonkMatch(chonk []byte, db *badger.DB) ([]byte, float64, error) {
	var confidence float64
	var keyToReturn []byte

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(cnst.ChonkNamespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			decoded, err := cnst.DECODER.DecodeAll(v, nil)
			if err == nil {
				v = decoded
			}

			tempCount := util.PartialMatchConfidence(chonk, v)
			if tempCount == 1 {
				continue
			}
			if tempCount > confidence {
				confidence = tempCount
				keyToReturn = key
			}
		}

		return nil
	})

	return keyToReturn, confidence, err
}
