package parser

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"

	"github.com/aoiflux/libxfat"
	"github.com/schollz/progressbar/v3"
)

func IndexEXFAT(pfile structs.InputFile, eviID []byte, idxChan chan error, enableFTS bool, enableEnrichment bool) {
	startOffset := getStartOffset(uint64(pfile.GetStartIndex()))
	exfatdata, err := libxfat.New(pfile.GetHandle(), true, startOffset)
	if err != nil {
		idxChan <- cnst.ErrIncompatibleFileSystem
		return
	}

	rootEntries, err := exfatdata.ReadRootDir()
	if err != nil {
		idxChan <- err
		return
	}

	indexableEntries, err := exfatdata.GetIndexableEntries(rootEntries)
	if err != nil {
		idxChan <- err
		return
	}

	var flag bool
	total := int64(len(indexableEntries))
	bar := progressbar.NewOptions64(
		total,
		progressbar.OptionSetDescription("Indexing files"),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)
	bar.Clear()

	encodedPfileHash, err := pfile.GetEncodedHash()
	if err != nil {
		idxChan <- err
		return
	}

	batch, err := util.InitBatch(pfile.GetDB())
	if err != nil {
		idxChan <- err
		return
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

		iname := string(util.AppendToBytesSlice(eviID, cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, entry.GetName()))
		istart := int64(exfatdata.GetClusterOffset(entry.GetEntryCluster()))
		isize := int64(entry.GetSize())
		err = registerIndexedRange(idxmap, pfile, eviID, iname, istart, isize, false)
		if err != nil {
			idxChan <- err
			return
		}

		if flag {
			bar.Add(1)
		}
	}

	err = finalizeIndexedFiles(idxmap, pfile, batch, idxChan, eviID, enableFTS, enableEnrichment)
	if err != nil {
		idxChan <- err
		return
	}
	if flag {
		bar.Finish()
		fmt.Fprintln(os.Stderr)
	}

	idxChan <- nil
}

func checkChannel(idxChan chan error) bool {
	_ = idxChan
	return true
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
