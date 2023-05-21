package parser

import (
	"indicer/lib/constant"
	"indicer/lib/structs"
	"indicer/lib/util"
	"os"

	"github.com/aoiflux/libxfat"
	"github.com/diskfs/go-diskfs/partition/mbr"
)

func GetPartitions(fhandle *os.File, size int64) ([]structs.PartitionFile, error) {
	plist, err := parseMBR(fhandle)
	if err == nil {
		return plist, nil
	}
	return parsEXFAT(fhandle, size)
}

func parseMBR(fhandle *os.File) ([]structs.PartitionFile, error) {
	dimbr, err := mbr.Read(fhandle, 0, 0)
	if err != nil {
		return nil, err
	}

	plist := []structs.PartitionFile{}
	partitions := dimbr.Partitions
	for _, partition := range partitions {
		if !util.IsSupported(partition.Type) {
			continue
		}

		var pfile structs.PartitionFile
		pfile.DBStart = constant.IgnoreVar
		pfile.Start = partition.GetStart() * int64(libxfat.SECTOR_SIZE)
		pfile.Size = partition.GetSize() * int64(libxfat.SECTOR_SIZE)
		plist = append(plist, pfile)
	}

	return plist, nil
}

func parsEXFAT(fhandle *os.File, size int64) ([]structs.PartitionFile, error) {
	var partition structs.PartitionFile
	partition.Start = 0
	partition.Size = size
	partition.DBStart = constant.IgnoreVar
	_, err := libxfat.New(fhandle, true)
	return []structs.PartitionFile{partition}, err
}
