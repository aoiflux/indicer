package parser

import (
	"encoding/binary"
	"indicer/lib/structs"
	"log"
	"os"

	"github.com/aoiflux/libxfat"
)

const (
	mbrSize       = 512
	partitionSize = 16
)

type PartitionEntry struct {
	Status        byte
	CHSFirst      [3]byte
	PartitionType byte
	CHSLast       [3]byte
	FirstLBA      uint32
	SectorCount   uint32
}

type MBR struct {
	Code       [446]byte
	Partitions [4]PartitionEntry
	Signature  uint16
}

func parseMBR(fhandle *os.File) []structs.PartitionFile {
	mbr := MBR{}
	err := binary.Read(fhandle, binary.LittleEndian, &mbr)
	if err != nil {
		log.Fatal(err)
	}

	plist := []structs.PartitionFile{}
	for _, partition := range mbr.Partitions {
		if _, err = libxfat.New(fhandle, true, uint64(partition.FirstLBA)); err != nil {
			continue
		}

		var pfile structs.PartitionFile
		pfile.Start = int64(partition.FirstLBA) * int64(libxfat.SECTOR_SIZE)
		pfile.Size = int64(partition.SectorCount) * int64(libxfat.SECTOR_SIZE)

		plist = append(plist, pfile)
	}

	return plist
}
