package near

import (
	"bytes"
	"encoding/base64"
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
	"strconv"

	"github.com/dgraph-io/badger/v4"
)

func countRList(inputHash []byte, idmap *structs.ConcMap, near structs.NearGen, db *badger.DB, echan chan error) {
	for nearIndex, revlist := range near.RevMap {
		for _, revhash := range revlist {
			if bytes.Equal(inputHash, []byte(revhash)) {
				continue
			}

			err := countEviFile(nearIndex, near.Confidence, near.MatchMethod, inputHash, []byte(revhash), idmap, db)
			if err != nil {
				echan <- err
				return
			}
		}
	}
	echan <- nil
}
func countEviFile(index int64, confidence float64, method string, inputHash, revhash []byte, idmap *structs.ConcMap, db *badger.DB) error {
	eid := util.AppendToBytesSlice(cnst.EviFileNamespace, revhash)
	efile, err := dbio.GetEvidenceFile(eid, db)
	if err != nil {
		return err
	}

	if len(efile.InternalObjects) == 0 {
		idmap.Set(string(eid), confidence)
		addNearChunkContribution(string(eid), nearChunkContribution{Index: index, Confidence: confidence, Method: method})
		return nil
	}
	return countPartiFile(confidence, index, method, inputHash, eid, efile.InternalObjects, idmap, db)
}
func countPartiFile(confidence float64, ridx int64, method string, inputHash, eid []byte, phashes map[string]structs.InternalOffset, idmap *structs.ConcMap, db *badger.DB) error {
	foundInRange := false
	for phash, offset := range phashes {
		pid, inRange, err := countFile(ridx, cnst.PartiFileNamespace, inputHash, []byte(phash), offset)
		if err != nil {
			return err
		}

		if !inRange {
			continue
		}
		foundInRange = true

		pfile, err := dbio.GetPartitionFile(pid, db)
		if err != nil {
			return err
		}

		if len(pfile.InternalObjects) == 0 {
			idmap.Set(string(pid), confidence)
			addNearChunkContribution(string(pid), nearChunkContribution{Index: ridx, Confidence: confidence, Method: method})
			continue
		}
		err = countIdxFile(confidence, ridx, method, inputHash, pid, pfile.InternalObjects, idmap)
		if err != nil {
			return err
		}
	}

	if !foundInRange {
		idmap.Set(string(eid), confidence)
		addNearChunkContribution(string(eid), nearChunkContribution{Index: ridx, Confidence: confidence, Method: method})
	}

	return nil
}

func countIdxFile(confidence float64, ridx int64, method string, inputHash, pid []byte, ihashes map[string]structs.InternalOffset, idmap *structs.ConcMap) error {
	foundInRange := false
	for ihash, offset := range ihashes {
		iid, inRange, err := countFile(ridx, cnst.IdxFileNamespace, inputHash, []byte(ihash), offset)
		if err != nil {
			return err
		}

		if !inRange {
			continue
		}
		foundInRange = true

		idmap.Set(string(iid), confidence)
		addNearChunkContribution(string(iid), nearChunkContribution{Index: ridx, Confidence: confidence, Method: method})
	}

	if !foundInRange {
		idmap.Set(string(pid), confidence)
		addNearChunkContribution(string(pid), nearChunkContribution{Index: ridx, Confidence: confidence, Method: method})
	}

	return nil
}
func countFile(ridx int64, namespace string, inputHash, fhash []byte, offset structs.InternalOffset) ([]byte, bool, error) {
	if bytes.Equal(fhash, inputHash) {
		return nil, false, nil
	}

	id, err := getIDFromHash(namespace, string(fhash))
	if err != nil {
		return nil, false, err
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

func partialChonkMatch(inhash []byte, inputSig uint64, db *badger.DB) ([]byte, float64, error) {
	var confidence float64
	var keyToReturn []byte
	sigCache := make(map[string]uint64)

	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		prefix := []byte(cnst.RelationNamespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			if bytes.Contains(key, inhash) {
				continue
			}

			chash, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}

			chash, err = cnst.DECODER.DecodeAll(chash, nil)
			if err != nil {
				return err
			}

			temp, err := checkInSignature(inputSig, chash, sigCache, db)
			if errors.Is(err, badger.ErrKeyNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if temp > confidence {
				confidence = temp
				idx, err := parseTrailingRelationIndex(key)
				if err != nil {
					return err
				}
				keyToReturn = util.AppendToBytesSlice(cnst.ReverseRelationNamespace, chash, cnst.DataSeperator, idx)
			}
		}

		return nil
	})

	return keyToReturn, confidence, err
}

func parseTrailingRelationIndex(key []byte) (int64, error) {
	if len(key) == 0 {
		return 0, errors.New("invalid relation key: empty")
	}

	end := len(key) - 1
	for end >= 0 && key[end] >= '0' && key[end] <= '9' {
		end--
	}

	start := end + 1
	if start >= len(key) {
		return 0, errors.New("invalid relation key: missing trailing index")
	}

	idx, err := strconv.ParseInt(string(key[start:]), 10, 64)
	if err != nil {
		return 0, err
	}
	return idx, nil
}

func checkInSignature(inputSig uint64, chash []byte, cache map[string]uint64, db *badger.DB) (float64, error) {
	key := string(chash)
	candidateSig, ok := cache[key]
	if !ok {
		var err error
		candidateSig, err = dbio.GetChonkSignature(chash, db)
		if err != nil {
			return float64(cnst.IgnoreVar), err
		}
		cache[key] = candidateSig
	}

	confidence := util.HammingSimilarity64(inputSig, candidateSig)
	return confidence, nil
}
