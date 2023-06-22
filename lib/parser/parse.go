package parser

import (
	"indicer/lib/structs"
	"os"
)

func GetPartitions(size int64, fhandle *os.File) []structs.PartitionFile {
	plist := parseMBR(size, fhandle)
	if len(plist) > 0 {
		return plist
	}
	return parsEXFAT(fhandle, size)
}
