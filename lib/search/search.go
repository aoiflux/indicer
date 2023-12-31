package search

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/near"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

var (
	cmap  *structs.SeenChonkMap
	idmap *structs.SearchIDMap
)

func init() {
	cmap = structs.NewSeenChonkMap()
	idmap = structs.NewSearchIDMap()
}

func Search(query string, db *badger.DB) error {
	start := time.Now()

	bar := progressbar.Default(4, "Searching....")

	err := searchFiles(cnst.IdxFileNamespace, query, db)
	if err != nil {
		return err
	}
	bar.Add(1)

	err = searchFiles(cnst.PartiFileNamespace, query, db)
	if err != nil {
		return err
	}
	bar.Add(1)

	err = searchFiles(cnst.EviFileNamespace, query, db)
	if err != nil {
		return err
	}
	bar.Add(1)

	err = searchReport(query, db)
	if err != nil {
		return err
	}
	bar.Add(1)

	bar.Finish()
	fmt.Println("Done....", time.Since(start))
	return bar.Close()
}

func searchFiles(namespace, query string, db *badger.DB) error {
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 1000
		it := txn.NewIterator(opts)
		defer it.Close()

		errChan := make(chan error)
		var active int

		prefix := []byte(namespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if active > cnst.GetMaxThreadCount() {
				active--
				err := <-errChan
				if err != nil {
					return err
				}
			}

			item := it.Item()
			fid := item.KeyCopy(nil)
			go searchAllFiles(query, fid, db, errChan)
			active++
		}

		for active > 0 {
			active--
			err := <-errChan
			if err != nil {
				return err
			}
		}
		return nil
	})

	return err
}

func searchAllFiles(query string, fid []byte, db *badger.DB, errChan chan error) {
	meta, err := store.GetFileMeta(fid, db)
	if err != nil {
		errChan <- err
	}
	errChan <- searchChonks(string(fid), query, meta, db)
}

func searchChonks(fidStr, query string, meta structs.FileMeta, db *badger.DB) error {
	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	echan := make(chan error)
	var active int
	for sindex := dbstart; sindex < end; sindex += cnst.ChonkSize {
		for active > cnst.GetMaxThreadCount() {
			active--
			err := <-echan
			if err != nil {
				return err
			}
		}
		go searchChonk(sindex, dbstart, end, fidStr, query, meta, db, echan)
		active++
	}

	for active > 0 {
		active--
		err := <-echan
		if err != nil {
			return err
		}
	}

	return nil
}

func searchChonk(sindex, dbstart, end int64, fid, query string, meta structs.FileMeta, db *badger.DB, echan chan error) {
	s1key, state1, err := getChonkState(sindex, dbstart, end, meta, db)
	if err != nil {
		echan <- err
		return
	}

	ok1 := cmap.GetOk(s1key)
	nxtidx := sindex + cnst.ChonkSize
	if nxtidx >= end {
		if !ok1 {
			subBytesChonk(fid, []byte(query), state1)
		}
		echan <- nil
		return
	}
	if !ok1 {
		cmap.Set(s1key)
	}

	s2key, state2, err := getChonkState(nxtidx, dbstart, end, meta, db)
	if err != nil {
		echan <- err
		return
	}

	ok2 := cmap.GetOk(s2key)
	if ok1 && ok2 {
		echan <- err
		return
	}

	if !ok2 {
		cmap.Set(s2key)
	}
	bigState := append(state1, state2...)
	subBytesChonk(fid, []byte(query), bigState)
	echan <- nil
}

func getChonkState(searchIndex, dbstart, end int64, meta structs.FileMeta, db *badger.DB) ([]byte, []byte, error) {
	relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, searchIndex)
	chash, err := dbio.GetNode(relKey, db)
	if err != nil {
		return nil, nil, err
	}
	ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
	state, err := dbio.GetChonkData(searchIndex, meta.Start, meta.Size, dbstart, end, ckey, db)
	return ckey, state, err
}

func subBytesChonk(fidStr string, query, chonk []byte) {
	chonk = bytes.ToLower(chonk)
	count := bytes.Count(chonk, query)
	if count <= 0 {
		return
	}
	idmap.Set(fidStr, count)
}

// track count + location, takes too long to run
// func subBytesChonk(cnum int64, fidStr string, query, chonk []byte) {
// 	chonk = bytes.ToLower(chonk)
// 	cindex := int64(bytes.Index(chonk, query))

// 	var offset int64
// 	for cindex != -1 {
// 		chonk = chonk[cindex:]

// 		cindex = (cnum * cnst.ChonkSize) + (cindex + offset)
// 		if !idmap.GetLocOk(cindex) {
// 			idmap.Set(fidStr, 1)
// 			idmap.SetLoc(cindex)
// 		}

// 		offset = cindex
// 		cindex = int64(bytes.Index(chonk, query))
// 	}
// }

func searchReport(query string, db *badger.DB) error {
	var report structs.SearchReport
	report.Query = query

	for id, count := range idmap.GetData() {
		meta, err := store.GetFileMeta([]byte(id), db)
		if err != nil {
			return err
		}
		chonks := meta.Size / cnst.ChonkSize
		if chonks > 2 {
			count /= 2
		}

		names, err := near.GetNames([]byte(id), db)
		if err != nil {
			return err
		}

		hash := strings.Split(id, cnst.NamespaceSeperator)[1]
		hashData := []byte(hash)
		hashStr := base64.StdEncoding.EncodeToString(hashData)

		var occurance structs.OccuranceData
		occurance.ArtefactHash = hashStr
		occurance.Count = count
		for name := range names {
			occurance.FileNames = append(occurance.FileNames, name)
		}

		report.Occurances = append(report.Occurances, occurance)
	}

	reportData, err := json.MarshalIndent(report, "", "\t")
	if err != nil {
		return err
	}

	return os.WriteFile("report.json", reportData, os.ModePerm)
}
