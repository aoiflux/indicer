package parser

import (
	"indicer/lib/structs"
	"os"

	"github.com/aoiflux/libxfat"
	"github.com/diskfs/go-diskfs/partition/mbr"
)

func parseMBR(size int64, fhandle *os.File) []structs.PartitionFile {
	mbrdata, err := mbr.Read(fhandle, 0, 0)
	if err != nil {
		return nil
	}

	var plist []structs.PartitionFile
	for _, partition := range mbrdata.Partitions {
		// go-diskfs reports start and size in bytes; libxfat.New takes the
		// start in 512-byte sectors.
		start, psize := partition.GetStart(), partition.GetSize()
		if psize <= 0 || start < 0 || start > size || psize > size-start {
			continue
		}
		if _, err := libxfat.New(fhandle, true, uint64(start)/libxfat.SECTOR_SIZE); err != nil {
			continue
		}

		var pfile structs.PartitionFile
		pfile.Start = start
		pfile.Size = psize

		plist = append(plist, pfile)
	}

	return plist
}
