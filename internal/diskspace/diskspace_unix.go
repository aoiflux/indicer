//go:build !windows

package diskspace

import "golang.org/x/sys/unix"

var noSpaceErrors = []error{unix.ENOSPC, unix.EDQUOT}

// Free returns the bytes available to this user on the filesystem holding
// path.
func Free(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// SameVolume reports whether the existing paths a and b are on one
// filesystem, so that what is written to both comes out of one free space.
func SameVolume(a, b string) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Stat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Stat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}
