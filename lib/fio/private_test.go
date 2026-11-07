package fio

import (
	"bytes"
	"os"
	"runtime"
	"testing"

	"indicer/lib/cnst"
)

// TestStoredFilesArePrivate is the STO-19 regression: chunk blobs and
// containers hold evidence content (in the clear in an unencrypted database)
// and were created readable by every user (0644). They are now the owner's
// only. Windows keeps file access in ACLs, not these bits.
func TestStoredFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not enforced on Windows")
	}
	assertMode := func(what, path string) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s %s has mode %#o, want no access for group or others", what, path, perm)
		}
	}

	dir := newContainerDB(t)
	ckey := append(append([]byte{}, cnst.ChonkNamespace...), bytes.Repeat([]byte{9}, 32)...)
	data := bytes.Repeat([]byte("chunk"), 100)
	path, err := WriteChonk(dir, data, ckey, blobHash)
	if err != nil {
		t.Fatal(err)
	}
	assertMode("new blob", string(path))
	// A torn blob is replaced through a temporary file and a rename.
	if err := os.Truncate(string(path), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteChonk(dir, data, ckey, blobHash); err != nil {
		t.Fatal(err)
	}
	assertMode("replaced blob", string(path))

	cm := NewContainerManager(dir)
	chunks := writeChunks(t, cm, 0, 1)
	closeManager(t, cm)
	assertMode("container", chunks[0].path)
}
