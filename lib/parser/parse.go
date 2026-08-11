package parser

import (
	"encoding/json"
	"indicer/lib/structs"
	"os"
)

// ParseImage returns the partition list for an image file.
// Pass the JSON produced by the pure-Go parser (tskcompat) as tuskJSON with
// hasTusk=true to reuse the already-obtained analysis.
// When hasTusk is false ParseImage falls back to MBR then exFAT detection.
func ParseImage(tuskJSON string, hasTusk bool, size int64, fhandle *os.File) []structs.PartitionFile {
	if hasTusk {
		var result tuskResult
		err := json.Unmarshal([]byte(tuskJSON), &result)
		if err == nil {
			if plist := result.toPartitionFiles(); len(plist) > 0 {
				return plist
			}
		}
	}

	plist := parseMBR(size, fhandle)
	if len(plist) > 0 {
		return plist
	}
	return parsEXFAT(fhandle, size)
}
