//go:build windows

package fsx

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func createDeleteOnClose(dir, pattern string) (*os.File, error) {
	for range 10 {
		name, err := tempName(dir, pattern)
		if err != nil {
			return nil, err
		}
		path, err := windows.UTF16PtrFromString(name)
		if err != nil {
			return nil, err
		}
		h, err := windows.CreateFile(path,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.CREATE_NEW,
			windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
		if errors.Is(err, windows.ERROR_FILE_EXISTS) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fsx: create temporary file %s: %w", name, err)
		}
		return os.NewFile(uintptr(h), name), nil
	}
	return nil, fmt.Errorf("fsx: no unused temporary file name in %s", dir)
}

// tempName returns a new random name in dir for pattern, with the random part
// at the last "*" or at the end.
func tempName(dir, pattern string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	random := hex.EncodeToString(b[:])
	if i := strings.LastIndex(pattern, "*"); i >= 0 {
		return filepath.Join(dir, pattern[:i]+random+pattern[i+1:]), nil
	}
	return filepath.Join(dir, pattern+random), nil
}
