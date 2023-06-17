package parser

import (
	"indicer/lib/structs"
	"os"

	"github.com/aoiflux/libxfat"
)

func GetPartitions(fhandle *os.File, size int64) []structs.PartitionFile {
	plist := parseMBR(fhandle)
	if len(plist) > 0 {
		return plist
	}
	return parsEXFAT(fhandle, size)
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
