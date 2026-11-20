//go:build !windows

package duesarc

import (
	"runtime"
	"strings"
)

// checkComponent refuses nothing beyond the portable checks on this system.
func checkComponent(string) error { return nil }

// FoldName is the key two entry names collide on. macOS file systems ignore
// case by default.
func FoldName(local string) string {
	if runtime.GOOS == "darwin" {
		return strings.ToLower(local)
	}
	return local
}
