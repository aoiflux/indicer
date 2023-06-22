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
		if partition.GetSize() == 0 || partition.GetSize()*int64(libxfat.SECTOR_SIZE) > size {
			continue
		}
		if _, err := libxfat.New(fhandle, true, uint64(partition.GetStart())); err != nil {
			continue
		}

		var pfile structs.PartitionFile
		pfile.Start = partition.GetStart() * int64(libxfat.SECTOR_SIZE)
		pfile.Size = partition.GetSize() * int64(libxfat.SECTOR_SIZE)

		plist = append(plist, pfile)
	}

	return plist
}
