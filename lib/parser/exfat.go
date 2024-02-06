package parser

import (
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"

	"github.com/aoiflux/libxfat"
	"github.com/dgraph-io/badger/v4"
	"github.com/schollz/progressbar/v3"
)

func IndexEXFAT(pfile structs.InputFile, idxChan chan error) {
	startOffset := getStartOffset(uint64(pfile.GetStartIndex()))
	exfatdata, err := libxfat.New(pfile.GetHandle(), true, startOffset)
	if err != nil {
		idxChan <- cnst.ErrIncompatibleFileSystem
	}

	rootEntries, err := exfatdata.ReadRootDir()
	if err != nil {
		idxChan <- err
	}

	indexableEntries, err := exfatdata.GetIndexableEntries(rootEntries)
	if err != nil {
		idxChan <- err
	}

	var flag bool
	total := int64(len(indexableEntries))
	bar := progressbar.Default(total, "indexing files")
	bar.Clear()

	encodedPfileHash, err := pfile.GetEncodedHash()
	if err != nil {
		idxChan <- err
	}

	batch, err := util.InitBatch(pfile.GetDB())
	if err != nil {
		idxChan <- err
	}

	var index int
	var entry libxfat.Entry
	idxmap := make(map[string]structs.IndexedFile)
	for index, entry = range indexableEntries {
		indexableEntries = util.Reslice(indexableEntries, 0)

		if !flag {
			flag = checkChannel(idxChan)
			if flag {
				bar.Set(index)
			}
		}

		iname := string(util.AppendToBytesSlice(pfile.GetEviFileHash(), cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, entry.GetName()))
		istart := int64(exfatdata.GetClusterOffset(entry.GetEntryCluster()))
		isize := int64(entry.GetSize())
		ihash, err := util.GetLogicalFileHash(pfile.GetHandle(), istart, isize, false)
		if err != nil {
			idxChan <- err
		}

		if val, ok := idxmap[string(ihash)]; ok {
			if _, ok := val.Names[iname]; !ok {
				val.Names[iname] = struct{}{}
			}
		} else {
			idxmap[string(ihash)] = structs.NewIndexedFile(iname, istart, isize)
		}
		pfile.UpdateInternalObjects(istart, isize, ihash)

		if flag {
			bar.Add(1)
		}
	}

	err = storeIndexedFiles(idxmap, pfile.GetDB(), batch, idxChan)
	if err != nil {
		idxChan <- err
	}

	err = batch.Flush()
	if err != nil {
		idxChan <- err
	}
	if flag {
		bar.Finish()
	}

	pchan := make(chan error)
	go store.Store(pfile, pchan)
	idxChan <- <-pchan
}

func checkChannel(idxChan chan error) bool {
	select {
	case <-idxChan:
		return true
	default:
		return false
	}
}

func getStartOffset(pfileStart uint64) uint64 {
	if pfileStart == 0 {
		return 0
	}
	return uint64(pfileStart) / libxfat.SECTOR_SIZE
}

func parsEXFAT(fhandle *os.File, size int64) []structs.PartitionFile {
	var partition structs.PartitionFile
	partition.Start = 0
	partition.Size = size
	_, err := libxfat.New(fhandle, true)
	if err != nil {
		return nil
	}
	return []structs.PartitionFile{partition}
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
