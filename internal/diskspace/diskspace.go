// Package diskspace reports free space and recognises disk-full errors on
// every platform DUES builds for.
package diskspace

import "errors"

// IsNoSpace reports whether err means the disk or the user's quota is full:
// ENOSPC or EDQUOT on Unix, ERROR_DISK_FULL or ERROR_HANDLE_DISK_FULL on
// Windows.
func IsNoSpace(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range noSpaceErrors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
