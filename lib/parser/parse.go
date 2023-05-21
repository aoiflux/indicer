package parser

import (
	"indicer/lib/constant"
	"indicer/lib/structs"
	"os"

	"github.com/aoiflux/libxfat"
	diskfs "github.com/diskfs/go-diskfs"
)

func GetPartitions(fhandle *os.File, size int64) ([]structs.PartitionFile, error) {
	plist, err := parseMBR(fhandle)
	if err == nil {
		return plist, nil
	}
	return parsEXFAT(fhandle, size)
}

func parseMBR(fhandle *os.File) ([]structs.PartitionFile, error) {
	disk, err := diskfs.Open(fhandle.Name())
	if err != nil {
		return nil, err
	}

	ptable, err := disk.GetPartitionTable()
	if err != nil {
		return nil, err
	}

	plist := []structs.PartitionFile{}
	partitions := ptable.GetPartitions()
	for _, partition := range partitions {
		var pfile structs.PartitionFile
		pfile.DBStart = constant.IgnoreVar
		pfile.Start = partition.GetStart()
		pfile.End = partition.GetStart() + partition.GetSize()
		pfile.Size = partition.GetSize()
		plist = append(plist, pfile)
	}

	return plist, nil
}

func parsEXFAT(fhandle *os.File, size int64) ([]structs.PartitionFile, error) {
	var partition structs.PartitionFile
	partition.Start = 0
	partition.End = size
	partition.Size = size
	partition.DBStart = constant.IgnoreVar
	_, err := libxfat.New(fhandle, true)
	return []structs.PartitionFile{partition}, err
}
