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
	cmap  map[string]struct{}
	idmap map[string]int
)

func init() {
	cmap = make(map[string]struct{})
	idmap = make(map[string]int)
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

		prefix := []byte(namespace)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			fid := item.KeyCopy(nil)
			err := searchAllFiles(query, fid, db)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return err
}

func searchAllFiles(query string, fid []byte, db *badger.DB) error {
	meta, err := store.GetFileMeta(fid, db)
	if err != nil {
		return err
	}
	return searchChonks(string(fid), query, meta, db)
}

func searchChonks(fidStr, query string, meta structs.FileMeta, db *badger.DB) error {
	var state1, state2 []byte

	var dbstart int64
	if meta.Start > 0 {
		dbstart = util.GetDBStartOffset(meta.Start)
	}
	end := meta.Start + meta.Size

	var found bool
	for searchIndex := dbstart; searchIndex < end; searchIndex += cnst.ChonkSize {
		// get state 1
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, searchIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}
		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		if _, ok := cmap[string(ckey)]; ok {
			continue
		}

		// state 1 search
		state1, err = dbio.GetChonkData(searchIndex, meta.Start, meta.Size, dbstart, ckey, db)
		if err != nil {
			return err
		}
		found = subBytesChonk(fidStr, []byte(query), state1)
		if found {
			continue
		}

		if searchIndex+cnst.ChonkSize >= end {
			continue
		}

		// get state 2
		relKey = util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, searchIndex+cnst.ChonkSize)
		chash, err = dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}
		ckey = util.AppendToBytesSlice(cnst.ChonkNamespace, chash)

		// state 2 search
		state2, err = dbio.GetChonkData(searchIndex+cnst.ChonkSize, meta.Start, meta.Size, dbstart, ckey, db)
		if err != nil {
			return err
		}

		found = subBytesChonk(fidStr, []byte(query), state2)
		if found {
			continue
		}

		if _, ok := cmap[string(ckey)]; ok {
			continue
		}

		// state 1+2 search
		bigState := append(state1, state2...)
		subBytesChonk(fidStr, []byte(query), bigState)
	}

	return nil
}

func subBytesChonk(fidStr string, query, chonk []byte) bool {
	chonk = bytes.ToLower(chonk)

	if count := bytes.Count(chonk, query); count > 0 {
		if val, ok := idmap[fidStr]; ok {
			idmap[fidStr] = val + count
			return true
		}

		idmap[fidStr] = count
		return true
	}

	return false
}

func searchReport(query string, db *badger.DB) error {
	var report structs.SearchReport
	report.Query = query

	for id, count := range idmap {
		names, err := near.GetNames([]byte(id), db)
		if err != nil {
			return err
		}

		hash := strings.Split(id, cnst.NamespaceSeperator)[1]
		hashDate := []byte(hash)
		hashStr := base64.StdEncoding.EncodeToString(hashDate)
		report.Occurance.ArtefactHash = hashStr
		report.Occurance.Count = count

		for name := range names {
			report.Occurance.FileNames = append(report.Occurance.FileNames, name)
		}
	}

	reportData, err := json.MarshalIndent(report, "", "\t")
	if err != nil {
		return err
	}

	return os.WriteFile("report.json", reportData, os.ModePerm)
}
