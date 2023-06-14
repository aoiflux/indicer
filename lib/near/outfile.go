package near

import (
	"encoding/hex"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/util"
	"os"
	"time"

	"github.com/dgraph-io/badger/v3"
	"github.com/edsrzf/mmap-go"
	"github.com/klauspost/compress/s2"
)

func NearOutFile(fpath string, db *badger.DB) error {
	size, _, mappedFile, err := outfileSetup(fpath)
	if err != nil {
		return err
	}
	defer mappedFile.Unmap()

	var count int64
	for chonk := range getOutfileChonks(size, mappedFile) {
		_, err := getParitalMatches(chonk, db)
		if err != nil {
			return err
		}

		count++
	}
	fmt.Printf("\n\nNumber of chonks: %d", count)

	return nil
}

func outfileSetup(fpath string) (int64, []byte, mmap.MMap, error) {
	finfo, err := os.Stat(fpath)
	if err != nil {
		return -1, nil, nil, err
	}
	fhandle, err := os.Open(fpath)
	if err != nil {
		return -1, nil, nil, err
	}

	fhash, err := util.GetFileHash(fhandle)
	if err != nil {
		return -1, nil, nil, err
	}

	mappedFile, err := mmap.Map(fhandle, mmap.RDONLY, 0)
	if err != nil {
		return -1, nil, nil, err
	}

	return finfo.Size(), fhash, mappedFile, nil
}

func getOutfileChonks(size int64, mappedFile mmap.MMap) chan []byte {
	chonk := make(chan []byte)
	go func() {
		defer close(chonk)
		for outindex := int64(0); outindex <= size; outindex += cnst.ChonkSize {
			var buffSize int64
			if size-outindex <= cnst.ChonkSize {
				buffSize = size - outindex
			} else {
				buffSize = cnst.ChonkSize
			}
			chonk <- mappedFile[outindex : outindex+buffSize]
		}
	}()
	return chonk
}

func getParitalMatches(chonk []byte, db *badger.DB) ([]string, error) {
	start := time.Now()
	chash, count, err := matchByBytes(chonk, db)
	if err != nil {
		return nil, err
	}
	fmt.Println("Match time: ", time.Since(start))
	chashString := hex.EncodeToString(chash)
	fmt.Printf("%s | %.4f\n", chashString, count)

	// start := time.Now()
	// chash, err := matchByChash(chonk, db)
	// if err != nil && err == badger.ErrKeyNotFound {
	// 	return nil, nil
	// }
	// if err != nil {
	// 	return nil, err
	// }
	// fmt.Println("Match time: ", time.Since(start))
	// chashString := hex.EncodeToString(chash)
	// fmt.Printf("%s | 1\n", chashString)

	return nil, nil
}

func matchByBytes(chonk []byte, db *badger.DB) ([]byte, float64, error) {
	var similarityCount float64
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
			data, err := s2.Decode(nil, v)
			if err != nil {
				return err
			}

			tempCount := util.ByteSimilarityCount(chonk, data)
			if tempCount > similarityCount {
				similarityCount = tempCount
				keyToReturn = key
			}
		}

		return nil
	})

	return keyToReturn, similarityCount, err
}
