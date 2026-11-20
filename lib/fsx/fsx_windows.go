//go:build windows

package fsx

import (
	"errors"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setPerm does nothing: Windows has no mode bits beyond read-only, and a
// read-only temporary file could not be renamed over its target.
func setPerm(*os.File, fs.FileMode) error { return nil }

// rename moves src to dst with MoveFileEx. Without overwrite, the call itself
// fails when dst exists, so the check is atomic. WRITE_THROUGH makes the call
// return only once the move is on disk, which stands in for a directory sync.
func rename(src, dst string, overwrite bool) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if overwrite {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	err = retryBusy(func() error { return windows.MoveFileEx(from, to, flags) })
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return &fs.PathError{Op: "rename", Path: dst, Err: fs.ErrExist}
	}
	if isBusy(err) {
		return &fs.PathError{Op: "rename", Path: dst,
			Err: errors.New("the file is in use by another process (antivirus or another program); close it and retry")}
	}
	return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
}

// SyncDir does nothing on Windows: directories cannot be flushed, and renames
// in this package use MOVEFILE_WRITE_THROUGH instead.
func SyncDir(string) error { return nil }

// isBusy reports the errors Windows returns while another process holds a
// file open without sharing it. Antivirus scanners and search indexers cause
// these routinely and release the file within moments.
func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// PrivateDir reports whether info, of the directory at path, is a directory
// this process's user owns. Who else may use it is in its ACL, which it
// inherits from its parent.
func PrivateDir(path string, info fs.FileInfo, _ fs.FileMode) bool {
	if !info.IsDir() {
		return false
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	token := windows.GetCurrentProcessToken()
	if user, err := token.GetTokenUser(); err == nil && owner.Equals(user.User.Sid) {
		return true
	}
	// An elevated administrator's new objects may be owned by the
	// Administrators group, the token's default owner.
	def, err := tokenOwner(token)
	return err == nil && owner.Equals(def)
}

// tokenOwner is the owner the token gives the objects it makes.
func tokenOwner(t windows.Token) (*windows.SID, error) {
	n := uint32(tokenOwnerSize)
	for {
		buf := make([]byte, n)
		err := windows.GetTokenInformation(t, windows.TokenOwner, &buf[0], n, &n)
		if err == nil {
			return (*struct{ Owner *windows.SID })(unsafe.Pointer(&buf[0])).Owner, nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, err
		}
	}
}

// tokenOwnerSize fits a TOKEN_OWNER and the SID it points to.
const tokenOwnerSize = 64
