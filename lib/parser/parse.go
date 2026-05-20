package parser

import (
	"encoding/json"
	"indicer/lib/structs"
	"os"
)

// ParseImage returns the partition list for an image file.
// Pass the result of TuskAnalysis as tuskJSON/hasTusk to reuse the already-obtained
// libtusk output; this avoids a second C library call.
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

// TuskAnalysis returns the raw JSON from libtusk_analyze and true when the
// library is available. Callers should invoke this once and reuse the result
// across goroutines rather than calling it per-partition.
func TuskAnalysis(imagePath string) (string, bool) {
	if !tuskAvailable() {
		return "", false
	}
	jsonOutput, err := tuskAnalyze(imagePath)
	if err != nil {
		return "", false
	}
	return jsonOutput, true
}
