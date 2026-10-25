//go:build !windows

package compression_product

import (
	"runtime"
	"strings"
)

// checkComponent refuses nothing beyond the portable checks on this system.
func checkComponent(string) error { return nil }

// foldName is the key two entry names collide on. macOS file systems ignore
// case by default.
func foldName(local string) string {
	if runtime.GOOS == "darwin" {
		return strings.ToLower(local)
	}
	return local
}
