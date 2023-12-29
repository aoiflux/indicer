package search

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/near"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v4"
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
	err := searchFiles(cnst.IdxFileNamespace, query, db)
	if err != nil {
		return err
	}
	err = searchFiles(cnst.PartiFileNamespace, query, db)
	if err != nil {
		return err
	}
	err = searchFiles(cnst.EviFileNamespace, query, db)
	if err != nil {
		return err
	}
	return searchReport(query, db)
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

			meta, err := store.GetFileMeta(fid, db)
			if err != nil {
				return err
			}

			err = searchChonks(string(fid), query, meta, db)
			if err != nil {
				return err
			}
		}

		return nil
	})

	return err
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
		relKey := util.AppendToBytesSlice(cnst.RelationNamespace, meta.EviHash, cnst.DataSeperator, searchIndex)
		chash, err := dbio.GetNode(relKey, db)
		if err != nil {
			return err
		}

		ckey := util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
		if _, ok := cmap[string(ckey)]; ok {
			continue
		}

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

		state2, err = dbio.GetChonkData(searchIndex+cnst.ChonkSize, meta.Start, meta.Size, dbstart, ckey, db)
		if err != nil {
			return err
		}

		bigState := append(state1, state2...)
		found = subBytesChonk(fidStr, []byte(query), bigState)
		if found {
			continue
		}

		subBytesChonk(fidStr, []byte(query), state2)
	}

	return nil
}

func subBytesChonk(fidStr string, query, chonk []byte) bool {
	if bytes.Contains(chonk, query) {
		if val, ok := idmap[fidStr]; ok {
			idmap[fidStr] = val + 1
			return true
		}

		idmap[fidStr] = 1
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
