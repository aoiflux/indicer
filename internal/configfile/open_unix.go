//go:build !windows

package configfile

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// open opens path for reading. O_NONBLOCK keeps the open of a FIFO from
// waiting for a writer; Load then refuses it as not a regular file.
func open(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// checkAccess applies ssh's rule to the file: settings that another
// account can change are not the user's settings. The file must belong to
// the user or to root, and neither its group nor others may write it.
func checkAccess(info fs.FileInfo) string {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "has no owner to check"
	}
	if uid := uint32(os.Getuid()); st.Uid != uid && st.Uid != 0 {
		return fmt.Sprintf("belongs to uid %d, not to you (uid %d) or root", st.Uid, uid)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Sprintf("can be changed by its group or others (mode %04o); run chmod go-w on it", perm)
	}
	return ""
}
