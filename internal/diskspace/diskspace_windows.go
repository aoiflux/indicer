//go:build windows

package diskspace

import (
	"strings"

	"golang.org/x/sys/windows"
)

var noSpaceErrors = []error{windows.ERROR_DISK_FULL, windows.ERROR_HANDLE_DISK_FULL}

// Free returns the bytes available to this user on the volume holding path.
func Free(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return avail, nil
}

// SameVolume reports whether the existing paths a and b are on one volume,
// so that what is written to both comes out of one free space.
func SameVolume(a, b string) (bool, error) {
	va, err := volumeOf(a)
	if err != nil {
		return false, err
	}
	vb, err := volumeOf(b)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(va, vb), nil
}

func volumeOf(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(p, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}
