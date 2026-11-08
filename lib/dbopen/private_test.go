package dbopen

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"indicer/lib/cnst"
)

// TestNewDataRootIsPrivate is the STO-19 regression for directories: a new
// data root, its badger directory and its blob directory were created 0755,
// so every user could list the database and read whatever its files allowed.
// They are now the owner's only. Windows keeps access in ACLs, not these bits.
func TestNewDataRootIsPrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	root := filepath.Join(t.TempDir(), "case")
	open(t, Request{Path: root, Mode: ReadWrite}).Close()
	for _, dir := range []string{root, filepath.Join(root, cnst.KVDBDIR), filepath.Join(root, cnst.BLOBSDIR)} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s has mode %#o, want no access for group or others", dir, perm)
		}
	}
}
