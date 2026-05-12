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

func countDeleted(files []tuskFile) int {
	var n int
	for _, f := range files {
		if f.IsDeleted {
			n++
		}
	}
	return n
}

func ensureNameMeta(ifile *structs.IndexedFile) {
	if ifile.NameMeta == nil {
		ifile.NameMeta = make(map[string]structs.IndexedNameMeta)
	}
	for name := range ifile.Names {
		if _, ok := ifile.NameMeta[name]; ok {
			continue
		}
		// Fragmented files are currently skipped by buildIdxMap, so this field
		// is false today but intentionally preserved for future ingestion modes.
		ifile.NameMeta[name] = structs.IndexedNameMeta{IsDeleted: ifile.IsDeleted}
	}
}

func mergeNameMeta(dst map[string]structs.IndexedNameMeta, src map[string]structs.IndexedNameMeta) bool {
	updated := false
	for name, meta := range src {
		existing, ok := dst[name]
		if !ok {
			dst[name] = meta
			updated = true
			continue
		}
		if !existing.IsDeleted && meta.IsDeleted {
			existing.IsDeleted = true
			dst[name] = existing
			updated = true
		}
		if !existing.IsFragmented && meta.IsFragmented {
			existing.IsFragmented = true
			dst[name] = existing
			updated = true
		}
	}
	return updated
}

func buildIdxMap(files []tuskFile, idxmap map[string]structs.IndexedFile, pfile structs.InputFile, encodedPfileHash []byte, idxChan chan error) {
	for _, f := range files {
		if f.IsFragmented {
			// Current parser behavior: fragmented entries are intentionally skipped.
			// We still keep IsFragmented in per-name metadata for forward compatibility.
			continue
		}
		iname := string(util.AppendToBytesSlice(pfile.GetEviFileHash(), cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, []byte(f.Filename)))
		for _, frag := range f.Fragments {
			checkChannel(idxChan)
			istart := frag.StartOffset
			isize := frag.EndOffset - frag.StartOffset + 1
			if err := registerIndexedRange(idxmap, pfile, iname, istart, isize, f.IsDeleted); err != nil {
				idxChan <- err
				return
			}
		}
	}
}

func registerIndexedRange(idxmap map[string]structs.IndexedFile, pfile structs.InputFile, iname string, istart, isize int64, isDeleted bool) error {
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
		idxmap[string(ihash)] = structs.NewIndexedFile(iname, istart, isize, typeHint, isDeleted)
		pfile.UpdateInternalObjects(istart, isize, ihash)
		return nil
	}

	if _, ok := val.Names[iname]; !ok {
		val.Names[iname] = struct{}{}
	}
	if val.NameMeta == nil {
		val.NameMeta = make(map[string]structs.IndexedNameMeta)
	}
	meta := val.NameMeta[iname]
	meta.IsDeleted = meta.IsDeleted || isDeleted
	val.NameMeta[iname] = meta
	if val.IndexedType == "" || val.IndexedType == cnst.UnknownEvidenceType {
		if promotedType := util.DetectFileType(iname, chunk); promotedType != cnst.UnknownEvidenceType {
			val.IndexedType = promotedType
		}
	}
	val.IsDeleted = val.IsDeleted || isDeleted
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
	bar := progressbar.NewOptions64(
		total,
		progressbar.OptionSetDescription("indexing files"),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "#",
			SaucerHead:    ">",
			SaucerPadding: "-",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)
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
		ensureNameMeta(&oldIdxFile)
		ensureNameMeta(&newIdxfile)

		typeUpdated := false
		if (oldIdxFile.IndexedType == "" || oldIdxFile.IndexedType == cnst.UnknownEvidenceType) &&
			newIdxfile.IndexedType != "" && newIdxfile.IndexedType != cnst.UnknownEvidenceType {
			oldIdxFile.IndexedType = newIdxfile.IndexedType
			typeUpdated = true
		}
		deletedUpdated := false
		if !oldIdxFile.IsDeleted && newIdxfile.IsDeleted {
			oldIdxFile.IsDeleted = true
			deletedUpdated = true
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
			nameMetaUpdated := mergeNameMeta(oldIdxFile.NameMeta, newIdxfile.NameMeta)

			unchanged := flag && !typeUpdated && !deletedUpdated && !nameMetaUpdated
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
		nameMetaUpdated := mergeNameMeta(newIdxfile.NameMeta, oldIdxFile.NameMeta)
		mergedDeleted := newIdxfile.IsDeleted || oldIdxFile.IsDeleted
		if mergedDeleted != oldIdxFile.IsDeleted {
			deletedUpdated = true
		}
		newIdxfile.IsDeleted = mergedDeleted
		unchanged := flag && !typeUpdated && !deletedUpdated && !nameMetaUpdated
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
