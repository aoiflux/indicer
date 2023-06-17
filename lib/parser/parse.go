package parser

import (
	"indicer/lib/structs"
	"os"

	"github.com/aoiflux/libxfat"
	"github.com/diskfs/go-diskfs/partition/mbr"
)

func GetPartitions(fhandle *os.File, size int64) []structs.PartitionFile {
	plist := parseMBR(fhandle, size)
	if len(plist) > 0 {
		return plist
	}
	return parsEXFAT(fhandle, size)
}

func parseMBR(fhandle *os.File, size int64) []structs.PartitionFile {
	dimbr, err := mbr.Read(fhandle, 0, 0)
	if err != nil {
		return nil
	}

	plist := []structs.PartitionFile{}
	partitions := dimbr.Partitions
	for _, partition := range partitions {
		if _, err = libxfat.New(fhandle, true, uint64(partition.Start)); err != nil {
			continue
		}
		var pfile structs.PartitionFile
		pfile.Start = partition.GetStart() * int64(libxfat.SECTOR_SIZE)
		pfile.Size = partition.GetSize() * int64(libxfat.SECTOR_SIZE)
		if pfile.Size > size {
			pfile.Size = partition.GetSize()
		}

		plist = append(plist, pfile)
	}

	return plist
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
