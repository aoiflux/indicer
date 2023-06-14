package parser

import (
	"indicer/lib/cnst"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/aoiflux/libxfat"
	"github.com/dgraph-io/badger/v3"
)

func IndexEXFAT(db *badger.DB, pfile structs.InputFile) error {
	startOffset := getStartOffset(uint64(pfile.GetStartIndex()))
	exfatdata, err := libxfat.New(pfile.GetHandle(), true, startOffset)
	if err != nil {
		return cnst.ErrIncompatibleFileSystem
	}

	rootEntries, err := exfatdata.ReadRootDir()
	if err != nil {
		return err
	}

	allEntries, err := exfatdata.GetAllEntries(rootEntries)
	if err != nil {
		return err
	}

	var active int
	echan := make(chan error)

	for _, entry := range allEntries {
		if entry.IsDeleted() || entry.IsDir() || entry.IsInvalid() || entry.HasFatChain() {
			continue
		}

		encodedPfileHash, err := pfile.GetEncodedHash()
		if err != nil {
			return err
		}
		iname := string(util.AppendToBytesSlice(pfile.GetEviFileHash(), cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, entry.GetName()))
		istart := int64(exfatdata.GetClusterOffset(entry.GetEntryCluster()))
		isize := int64(entry.GetSize())
		ihash, err := util.GetLogicalFileHash(pfile.GetHandle(), istart, isize)
		if err != nil {
			return err
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

		if active > cnst.MaxThreadCount {
			if <-echan != nil {
				return <-echan
			}
			active--
		}
	}
	for active > 0 {
		if <-echan != nil {
			return <-echan
		}
		active--
	}

	go store.Store(pfile, echan)
	return <-echan
}

func getStartOffset(pfileStart uint64) uint64 {
	if pfileStart == 0 {
		return 0
	}
	return uint64(pfileStart) / libxfat.SECTOR_SIZE
}
