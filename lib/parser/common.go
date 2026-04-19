package parser

import (
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

func countFragmented(files []tuskFile) int {
	var n int
	for _, f := range files {
		if f.IsFragmented {
			n++
		}
	}
	return n
}

func buildIdxMap(files []tuskFile, idxmap map[string]structs.IndexedFile, pfile structs.InputFile, encodedPfileHash []byte, idxChan chan error) {
	for _, f := range files {
		if f.IsFragmented {
			continue
		}
		iname := string(util.AppendToBytesSlice(pfile.GetEviFileHash(), cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, []byte(f.Filename)))
		for _, frag := range f.Fragments {
			checkChannel(idxChan)
			istart := frag.StartOffset
			isize := frag.EndOffset - frag.StartOffset + 1
			ihash, err := util.GetLogicalFileHash(pfile.GetHandle(), cnst.GetHashAlgo(true), istart, isize, false)
			if err != nil {
				idxChan <- err
				return
			}
			if val, ok := idxmap[string(ihash)]; ok {
				if _, ok := val.Names[iname]; !ok {
					val.Names[iname] = struct{}{}
				}
			} else {
				idxmap[string(ihash)] = structs.NewIndexedFile(iname, istart, isize)
			}
			pfile.UpdateInternalObjects(istart, isize, ihash)
		}
	}
}

func storeIndexedFiles(idxmap map[string]structs.IndexedFile, db *badger.DB, batch *badger.WriteBatch, idxChan chan error) error {
	var pflag bool
	total := int64(len(idxmap))
	bar := progressbar.Default(total, "indexing files")
	bar.Clear()

	for ihash, newIdxfile := range idxmap {
		delete(idxmap, ihash)
		if !pflag {
			pflag = checkChannel(idxChan)
			if pflag {
				bar.Set(1)
			}
		}

		id := util.AppendToBytesSlice(cnst.IdxFileNamespace, ihash)
		oldIdxFile, err := dbio.GetIndexedFile(id, db)
		if errors.Is(err, badger.ErrKeyNotFound) {
			err = dbio.SetIndexedFile(id, newIdxfile, batch)
			if err != nil {
				return err
			}
			continue
		}
		if err != nil && err != badger.ErrKeyNotFound {
			return err
		}

		flag := true
		if len(newIdxfile.Names) < len(oldIdxFile.Names) {
			for newName := range newIdxfile.Names {
				if _, ok := oldIdxFile.Names[newName]; !ok {
					oldIdxFile.Names[newName] = struct{}{}
					flag = false
				}
			}

			if flag {
				continue
			}
			err = dbio.SetIndexedFile(id, oldIdxFile, batch)
			if err != nil {
				return err
			}

			continue
		}

		for oldName := range oldIdxFile.Names {
			if _, ok := newIdxfile.Names[oldName]; !ok {
				newIdxfile.Names[oldName] = struct{}{}
				flag = false
			}
		}
		if flag {
			continue
		}
		err = dbio.SetIndexedFile(id, newIdxfile, batch)
		if err != nil {
			return err
		}

		if flag {
			bar.Add(1)
		}
	}

	if pflag {
		bar.Finish()
	}
	return nil
}
