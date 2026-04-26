package parser

import (
	"errors"
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/store"
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
			if err := registerIndexedRange(idxmap, pfile, iname, istart, isize); err != nil {
				idxChan <- err
				return
			}
		}
	}
}

func registerIndexedRange(idxmap map[string]structs.IndexedFile, pfile structs.InputFile, iname string, istart, isize int64) error {
	if isize <= 0 {
		return fmt.Errorf("invalid indexed file size %d for %s", isize, iname)
	}

	end := istart + isize
	mapped := pfile.GetMappedFile()
	if istart < 0 || end > int64(len(mapped)) {
		return fmt.Errorf("indexed file range out of bounds for %s: start=%d end=%d mapped=%d", iname, istart, end, len(mapped))
	}

	chunk := mapped[int(istart):int(end)]
	ihash, err := util.GetLogicalFileHash(pfile.GetHandle(), cnst.GetHashAlgo(true), istart, isize, false)
	if err != nil {
		return err
	}

	val, ok := idxmap[string(ihash)]
	if !ok {
		typeHint := util.DetectFileType(iname, chunk)
		idxmap[string(ihash)] = structs.NewIndexedFile(iname, istart, isize, typeHint)
		pfile.UpdateInternalObjects(istart, isize, ihash)
		return nil
	}

	if _, ok := val.Names[iname]; !ok {
		val.Names[iname] = struct{}{}
	}
	if val.IndexedType == "" || val.IndexedType == cnst.UnknownEvidenceType {
		if promotedType := util.DetectFileType(iname, chunk); promotedType != cnst.UnknownEvidenceType {
			val.IndexedType = promotedType
		}
	}
	idxmap[string(ihash)] = val

	pfile.UpdateInternalObjects(istart, isize, ihash)
	return nil
}

func finalizeIndexedFiles(idxmap map[string]structs.IndexedFile, pfile structs.InputFile, batch *badger.WriteBatch, idxChan chan error) error {
	err := storeIndexedFiles(idxmap, pfile.GetDB(), batch, idxChan)
	if err != nil {
		return err
	}

	err = batch.Flush()
	if err != nil {
		return err
	}

	pchan := make(chan error)
	go store.Store(pfile, pchan)
	return <-pchan
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
		if err != nil {
			return err
		}

		typeUpdated := false
		if (oldIdxFile.IndexedType == "" || oldIdxFile.IndexedType == cnst.UnknownEvidenceType) &&
			newIdxfile.IndexedType != "" && newIdxfile.IndexedType != cnst.UnknownEvidenceType {
			oldIdxFile.IndexedType = newIdxfile.IndexedType
			typeUpdated = true
		}

		flag := true
		if len(newIdxfile.Names) < len(oldIdxFile.Names) {
			for newName := range newIdxfile.Names {
				if _, ok := oldIdxFile.Names[newName]; ok {
					continue
				}
				oldIdxFile.Names[newName] = struct{}{}
				flag = false
			}

			unchanged := flag && !typeUpdated
			if unchanged {
				continue
			}
			if err := dbio.SetIndexedFile(id, oldIdxFile, batch); err != nil {
				return err
			}

			continue
		}

		for oldName := range oldIdxFile.Names {
			if _, ok := newIdxfile.Names[oldName]; ok {
				continue
			}
			newIdxfile.Names[oldName] = struct{}{}
			flag = false
		}
		if newIdxfile.IndexedType == "" || newIdxfile.IndexedType == cnst.UnknownEvidenceType {
			newIdxfile.IndexedType = oldIdxFile.IndexedType
		}
		unchanged := flag && !typeUpdated
		if unchanged {
			continue
		}
		if err := dbio.SetIndexedFile(id, newIdxfile, batch); err != nil {
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
