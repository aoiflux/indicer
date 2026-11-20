//go:build !windows

package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/dbpackage"
)

// TestExportDirAccess: an export folder outside the root that another
// account may change is refused, as a token file would be, before it is
// locked.
func TestExportDirAccess(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	out := t.TempDir()
	if err := os.Chmod(out, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{Limits: testLimits(), Exports: ExportConfig{Dir: out}}, Deps{Handle: h})
	if err == nil || !strings.Contains(err.Error(), "chmod") {
		t.Fatalf("a folder others may change: %v, want an error naming chmod", err)
	}
	if _, err := os.Stat(filepath.Join(out, exportsLockName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused folder was locked (%v)", err)
	}
}

// TestExportFilesPrivate: a package and its sidecar are readable by the
// server's account only, whatever the folder lets others read.
func TestExportFilesPrivate(t *testing.T) {
	t.Parallel()
	ls, _ := exportServer(t, nil, nil)
	id, _ := exportDone(t, ls, false)
	for _, suffix := range []string{dbpackage.Ext, sidecarSuffix} {
		info, err := os.Stat(ls.exports.file(id, suffix))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != cnst.BlobPerm {
			t.Errorf("%s%s has mode %v, want %v", id, suffix, perm, fs.FileMode(cnst.BlobPerm))
		}
	}
}
