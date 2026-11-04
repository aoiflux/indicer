package dbopen

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"indicer/lib/dbio"
	"indicer/lib/structs"
)

// TestOpenRecoversWhenAsked: a writable open with Recover fails, before it
// returns, the ingest a crash left pending. Without Recover (repair inspects
// first), or read-only (CFG-11), the open leaves it as it was.
func TestOpenRecoversWhenAsked(t *testing.T) {
	root := setup(t)
	id := []byte("crashed-evidence")
	h := open(t, Request{Path: root, Mode: ReadWrite})
	if err := dbio.SetFile(id, structs.NewEvidenceFile("crashed.dd", 0, 128, nil, "dd", ""), h.DB); err != nil {
		t.Fatal(err)
	}
	h.Close()

	failed := func(req Request) bool {
		t.Helper()
		h := open(t, req)
		defer h.Close()
		e, err := dbio.GetEvidenceFile(id, h.DB)
		if err != nil {
			t.Fatal(err)
		}
		return e.Failed
	}
	if failed(Request{Path: root, Mode: ReadWrite}) {
		t.Fatal("a writable open without Recover recovered the crashed ingest")
	}
	if failed(Request{Path: root, Mode: ReadOnly, Recover: true}) {
		t.Fatal("a read-only open recovered the crashed ingest")
	}
	if !failed(Request{Path: root, Mode: ReadWrite, Recover: true}) {
		t.Fatal("a writable open with Recover left the crashed ingest pending")
	}
}

// TestSmallChunksAdviseContainers: a new database that keeps a file per
// chunk, with chunks under 128 KiB, is advised to use containers. One in
// containers, one with larger chunks, and one that already existed are not.
func TestSmallChunksAdviseContainers(t *testing.T) {
	root := setup(t)
	advised := func(req Request) bool {
		t.Helper()
		h := open(t, req)
		defer h.Close()
		return slices.ContainsFunc(h.Notices, func(n string) bool { return strings.Contains(n, "--container") })
	}
	if !advised(Request{Path: root, Mode: ReadWrite, Layout: Requested{ChunkKiB: 64}}) {
		t.Error("a new database of 64 KiB chunk files was not advised to use containers")
	}
	if advised(Request{Path: root, Mode: ReadWrite}) {
		t.Error("an existing database was advised again")
	}
	if advised(Request{Path: filepath.Join(t.TempDir(), "containers"), Mode: ReadWrite, Layout: Requested{ChunkKiB: 64, Container: true}}) {
		t.Error("a database in containers was advised to use containers")
	}
	if advised(Request{Path: filepath.Join(t.TempDir(), "large"), Mode: ReadWrite}) {
		t.Error("a database of 256 KiB chunks was advised to use containers")
	}
}
