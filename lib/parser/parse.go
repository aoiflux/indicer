package parser

import (
	"indicer/lib/structs"
	"os"
)

func GetPartitions(fhandle *os.File, size int64) []structs.PartitionFile {
	plist := parseMBR(fhandle)
	if len(plist) > 0 {
		return plist
	}
	return parsEXFAT(fhandle, size)
}
