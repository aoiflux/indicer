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
	"golang.org/x/exp/slices"
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
	var active int
	storeChan := make(chan error)
	total := int64(len(indexableEntries))
	bar := progressbar.Default(total, "indexing files")
	bar.Clear()

	encodedPfileHash, err := pfile.GetEncodedHash()
	if err != nil {
		idxChan <- err
	}

	ifile := structs.NewInputFile(
		pfile.GetDB(),
		pfile.GetHandle(),
		pfile.GetMappedFile(),
		"",
		"",
		nil,
		0,
		0,
	)

	err = ifile.SetBatch()
	if err != nil {
		idxChan <- err
	}
	batch, err := ifile.GetBatch()
	if err != nil {
		idxChan <- err
	}

	var index int
	var entry libxfat.Entry
	for index, entry = range indexableEntries {
		if !flag {
			flag = checkChannel(idxChan)
			if flag {
				bar.Set(index)
			}
		}

		iname := string(util.AppendToBytesSlice(pfile.GetEviFileHash(), cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, entry.GetName()))
		istart := int64(exfatdata.GetClusterOffset(entry.GetEntryCluster()))
		isize := int64(entry.GetSize())
		ihash, err := util.GetLogicalFileHash(ifile.GetHandle(), istart, isize, false)
		if err != nil {
			idxChan <- err
		}

		ifile.UpdateInputFile(iname, cnst.IdxFileNamespace, ihash, isize, istart)
		pfile.UpdateInternalObjects(istart, isize, ihash)

		go storeIndexedFile(ifile, batch, storeChan)
		active++

		if active > cnst.GetMaxThreadCount() {
			err = <-storeChan
			if err != nil {
				idxChan <- err
			}
			active--

			if flag {
				bar.Add(1)
			}
		}
	}
	for active > 0 {
		err = <-storeChan
		if err != nil {
			idxChan <- err
		}
		active--

		if flag {
			bar.Add(1)
		}
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

func storeIndexedFile(infile structs.InputFile, batch *badger.WriteBatch, storeChan chan error) {
	indexedFile, err := dbio.GetIndexedFile(infile.GetID(), infile.GetDB())
	if errors.Is(err, badger.ErrKeyNotFound) {
		indexedFile = structs.NewIndexedFile(
			infile.GetName(),
			infile.GetStartIndex(),
			infile.GetSize(),
		)
		storeChan <- dbio.SetIndexedFile(infile.GetID(), indexedFile, batch)
	}
	if err != nil && err != badger.ErrKeyNotFound {
		storeChan <- err
	}

	if slices.Contains(indexedFile.Names, infile.GetName()) {
		storeChan <- nil
	}

	indexedFile.Names = append(indexedFile.Names, infile.GetName())
	storeChan <- dbio.SetIndexedFile(infile.GetID(), indexedFile, batch)
}
