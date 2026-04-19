//go:build !(cgo && (windows || linux) && amd64)

package parser

import (
	"errors"

	"indicer/lib/structs"
)

var errTuskFailed = errors.New("libtusk: not available on this platform/architecture")

func tuskAvailable() bool { return false }

func tuskAnalyze(_ string) (string, error) {
	return "", errTuskFailed
}

func tuskGetPartitions(_ string) ([]structs.PartitionFile, error) {
	return nil, errTuskFailed
}
