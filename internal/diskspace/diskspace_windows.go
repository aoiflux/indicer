//go:build windows

package diskspace

import "golang.org/x/sys/windows"

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
