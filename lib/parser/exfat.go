package parser

import (
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"
	"time"

	"github.com/aoiflux/libxfat"
	"github.com/schollz/progressbar/v3"
)

// IndexEXFAT indexes the contiguous files of the exFAT volume pfile holds.
// It sends exactly one result on idxChan, which the caller reads once (ING-2).
func IndexEXFAT(pfile structs.InputFile, eviID []byte, idxChan chan error, enableFTS bool, enableEnrichment bool) {
	idxChan <- indexEXFAT(pfile, eviID, enableFTS, enableEnrichment)
}

func indexEXFAT(pfile structs.InputFile, eviID []byte, enableFTS bool, enableEnrichment bool) error {
	startOffset := getStartOffset(uint64(pfile.GetStartIndex()))
	exfatdata, err := libxfat.New(pfile.GetHandle(), true, startOffset)
	if err != nil {
		return cnst.ErrIncompatibleFileSystem
	}

	rootEntries, err := exfatdata.ReadRootDir()
	if err != nil {
		return err
	}

	indexableEntries, err := exfatdata.ContiguousFiles(rootEntries)
	if err != nil {
		return err
	}

	total := int64(len(indexableEntries))
	bar := progressbar.NewOptions64(
		total,
		progressbar.OptionSetDescription("Indexing files"),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)
	bar.Clear()

	encodedPfileHash, err := pfile.GetEncodedHash()
	if err != nil {
		return err
	}

	batch, err := util.InitBatch(pfile.GetDB())
	if err != nil {
		return err
	}
	// A partition that fails commits nothing; after a flush this is a no-op.
	defer batch.Cancel()

	var entry libxfat.Entry
	idxmap := make(map[string]structs.IndexedFile)
	bar.Set(0)
	for _, entry = range indexableEntries {
		indexableEntries = util.Reslice(indexableEntries, 0)

		// An entry whose first cluster the volume cannot address (cluster 0
		// for an empty file) has no content range; libxfat before v1.4.0
		// returned an offset inside the FAT for it.
		off, err := exfatdata.ClusterOffset(entry.FirstCluster())
		if err != nil {
			continue
		}
		iname := string(util.AppendToBytesSlice(eviID, cnst.DataSeperator, encodedPfileHash, cnst.DataSeperator, entry.Name()))
		istart := int64(off)
		isize := int64(entry.Size())
		// exFAT records modification, access and creation times but no change
		// time, matching what lib/parser/fs/exfat reports.
		times := timestampsOf(entry.ModifiedTime(), entry.AccessedTime(), time.Time{}, entry.CreatedTime())
		err = registerIndexedRange(idxmap, pfile, eviID, iname, istart, isize, false, times)
		if err != nil {
			return err
		}
		bar.Add(1)
	}

	if err := finalizeIndexedFiles(idxmap, pfile, batch, eviID, enableFTS, enableEnrichment); err != nil {
		return err
	}
	bar.Finish()
	fmt.Fprintln(console.Err())
	return nil
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
