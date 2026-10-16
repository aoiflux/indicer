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
