package store

import (
	"bytes"
	"encoding/base64"
	"indicer/lib/constant"
	"indicer/lib/util"
	"math"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v3"
)

func Restore(db *badger.DB, dst *os.File, fid []byte) error {
	if bytes.HasPrefix(fid, []byte(constant.IndexedFileNamespace)) {
		return restoreIndexedFile(db, dst, fid)
	}

	if bytes.HasPrefix(fid, []byte(constant.PartitionFileNamespace)) {
	}

	return nil
}

func getDBStartOffset(startIndex int64) int64 {
	if startIndex == 0 {
		return 0
	}

	ans := float64(startIndex) / float64(constant.ChonkSize)
	ans = math.Floor(ans)

	offset := int64(ans) * constant.ChonkSize
	return offset
}

// func findDBStartOffset(startIndex int64, db *badger.DB) (int64, error) {
// 	if startIndex == 0 {
// 		return 0, nil
// 	}

// 	var offset int64

// 	err := db.View(func(txn *badger.Txn) error {
// 		opts := badger.DefaultIteratorOptions
// 		opts.PrefetchSize = 1000
// 		it := txn.NewIterator(opts)
// 		defer it.Close()

// 		relPrefix := []byte(constant.RelationNapespace)
// 		for it.Seek(relPrefix); it.ValidForPrefix(relPrefix); it.Next() {
// 			item := it.Item()
// 			k := item.KeyCopy(nil)

// 			kpart := bytes.Split(k, relPrefix)[1]
// 			klist := bytes.Split(kpart, []byte(constant.PipeSeperator))
// 			dbindexString := string(klist[len(klist)-1])
// 			dbindex, err := strconv.ParseInt(dbindexString, 10, 64)
// 			if err != nil {
// 				return err
// 			}

// 			// if dbindex > startIndex {
// 			// 	continue
// 			// }

// 			v, err := item.ValueCopy(nil)
// 			if err != nil {
// 				return err
// 			}
// 			decoded, err := s2.Decode(nil, v)
// 			if err != nil {
// 				return err
// 			}

// 			ckey := append([]byte(constant.ChonkNamespace), decoded...)
// 			item, err = txn.Get(ckey)
// 			if err != nil {
// 				return err
// 			}

// 			v, err = item.ValueCopy(nil)
// 			if err != nil {
// 				return err
// 			}
// 			decoded, err = s2.Decode(nil, v)
// 			if err != nil {
// 				return err
// 			}

// 			datalen := int64(len(decoded))
// 			diff := dbindex + datalen
// 			if (startIndex >= dbindex) && (diff >= startIndex) {
// 				offset = dbindex
// 				break
// 			}
// 		}

// 		return nil
// 	})

// 	return offset, err
// }

func getEvidenceFileHash(fname string) ([]byte, error) {
	eviFileHashString := strings.Split(fname, constant.FilePathSeperator)[0]
	eviFileHash, err := base64.StdEncoding.DecodeString(eviFileHashString)
	if err != nil {
		return nil, err
	}
	return eviFileHash, err
}
func getEvidenceFileID(eviFileHash []byte) []byte {
	return append([]byte(constant.EvidenceFileNamespace), eviFileHash...)
}
func checkCompleted(eid []byte, db *badger.DB) error {
	eviFile, err := getEvidenceFile(eid, db)
	if err != nil {
		return err
	}
	if !eviFile.Completed {
		return constant.ErrIncompleteFile
	}
	return nil
}

func restoreIndexedFile(db *badger.DB, dst *os.File, fid []byte) error {
	indexedFile, err := getIndexedFile(fid, db)
	if err != nil {
		return err
	}
	ehash, err := getEvidenceFileHash(indexedFile.Names[0])
	if err != nil {
		return err
	}
	eid := getEvidenceFileID(ehash)
	err = checkCompleted(eid, db)
	if err != nil {
		return err
	}

	if indexedFile.DBStart == constant.IgnoreVar {
		indexedFile.DBStart = getDBStartOffset(indexedFile.Start)
		err = setFile(fid, indexedFile, db)
		if err != nil {
			return err
		}
	}

	return restoreData(ehash, indexedFile.Start, indexedFile.DBStart, indexedFile.Size, dst, db)
}
func restorePartitionFile() {}
func restoreEvidenceFile()  {}

func restoreData(ehash []byte, start, dbstart, size int64, dst *os.File, db *badger.DB) error {
	end := start + size

	for restoreIndex := dbstart; ; restoreIndex += constant.ChonkSize {
		if restoreIndex > end {
			break
		}

		relKey := util.AppendToBytesSlice(constant.RelationNapespace, ehash, constant.PipeSeperator, restoreIndex)
		chash, err := getNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := append([]byte(constant.ChonkNamespace), chash...)
		data, err := getNode(ckey, db)
		if err != nil {
			return err
		}

		if restoreIndex == dbstart {
			actualStart := start - restoreIndex
			data = data[actualStart:]
		}
		if (restoreIndex + constant.ChonkSize) > end {
			actualEnd := end - restoreIndex
			remaining := actualEnd - int64(len(data))
			data = data[:remaining]
		}

		_, err = dst.Write(data)
		if err != nil {
			return err
		}
	}

	return nil
}
