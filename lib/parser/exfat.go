package parser

import (
	"indicer/lib/cnst"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"

	"github.com/aoiflux/libxfat"
	"github.com/dgraph-io/badger/v3"
	"github.com/schollz/progressbar/v3"
)

func IndexEXFAT(db *badger.DB, pfile structs.InputFile, idxChan chan error) {
	startOffset := getStartOffset(uint64(pfile.GetStartIndex()))
	exfatdata, err := libxfat.New(pfile.GetHandle(), true, startOffset)
	if err != nil {
		idxChan <- cnst.ErrIncompatibleFileSystem
	}

	rootEntries, err := exfatdata.ReadRootDir()
	if err != nil {
		idxChan <- err
	}

	allEntries, err := exfatdata.GetAllEntries(rootEntries)
	if err != nil {
		idxChan <- err
	}

	var active int
	var flag bool
	echan := make(chan error)
	bar := progressbar.Default(-1, "indexing files")
	bar.Clear()

	encodedPfileHash, err := pfile.GetEncodedHash()
	if err != nil {
		idxChan <- err
	}

	maxThreadCount := cnst.GetMaxThreadCount() / 2
	for _, entry := range allEntries {
		if !flag {
			flag = checkChannel(idxChan)
		}

		if entry.IsDeleted() || entry.IsDir() || entry.IsInvalid() || entry.HasFatChain() {
			continue
		}

		iname := string(util.AppendToBytesSlice(pfile.GetEviFileHash(), cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, entry.GetName()))
		istart := int64(exfatdata.GetClusterOffset(entry.GetEntryCluster()))
		isize := int64(entry.GetSize())
		ihash, err := util.GetLogicalFileHash(pfile.GetHandle(), istart, isize, false)
		if err != nil {
			idxChan <- err
		}

		ifile := structs.NewInputFile(
			pfile.GetDB(),
			pfile.GetHandle(),
			pfile.GetMappedFile(),
			iname,
			cnst.IdxFileNamespace,
			ihash,
			isize,
			istart,
		)
		pfile.UpdateInternalObjects(istart, isize, ihash)

		go store.Store(ifile, echan)
		active++

		if active > maxThreadCount {
			err = <-echan
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
		if !flag {
			flag = checkChannel(idxChan)
		}

		err = <-echan
		if err != nil {
			idxChan <- err
		}
		active--

		if flag {
			bar.Add(1)
		}
	}

	err = db.Sync()
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
	case _, ok := <-idxChan:
		return ok
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
