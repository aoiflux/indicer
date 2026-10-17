package dbopen

import (
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
)

func containerRequest(path string) Request {
	return Request{Path: path, Mode: ReadWrite, Layout: Requested{Container: true}}
}

// TestSetStorage covers the manifest change `repair --upgrade` makes: a
// hierarchical database becomes a container database, and the change
// survives a reopen.
func TestSetStorage(t *testing.T) {
	root := setup(t)
	h := open(t, containerRequest(root))
	if err := h.SetStorage(dbmeta.StorageContainerBIDX); err != nil {
		t.Fatal(err)
	}
	h.Close()

	h = open(t, Request{Path: root, Mode: ReadWrite})
	if h.Manifest.Layout.Storage != dbmeta.StorageContainerBIDX || !cnst.LegacyBlockIndex {
		t.Fatalf("after reopen: storage %s, legacy block index %v", h.Manifest.Layout.Storage, cnst.LegacyBlockIndex)
	}
	if err := h.SetStorage(dbmeta.StorageContainer); err != nil {
		t.Fatal(err)
	}
	h.Close()

	h = open(t, Request{Path: root, Mode: ReadOnly})
	if h.Manifest.Layout.Storage != dbmeta.StorageContainer || cnst.LegacyBlockIndex {
		t.Fatalf("after upgrade: storage %s, legacy block index %v", h.Manifest.Layout.Storage, cnst.LegacyBlockIndex)
	}
	if err := h.SetStorage(dbmeta.StorageContainerBIDX); err == nil {
		t.Fatal("SetStorage on a read-only open succeeded")
	}
}

// TestStorageChangeInterrupted: the upgrade writes the in-database marker
// and the manifest file one after the other. A crash between them must not
// leave a database that refuses to open.
func TestStorageChangeInterrupted(t *testing.T) {
	root := setup(t)
	h := open(t, containerRequest(root))
	if err := h.SetStorage(dbmeta.StorageContainerBIDX); err != nil {
		t.Fatal(err)
	}
	m := h.Manifest
	h.Close()

	// The manifest file says container; the marker still says container-bidx.
	m.Layout.Storage = dbmeta.StorageContainer
	if err := dbmeta.Save(root, m); err != nil {
		t.Fatal(err)
	}
	h = open(t, Request{Path: root, Mode: ReadOnly})
	if h.Manifest.Layout.Storage != dbmeta.StorageContainer {
		t.Fatalf("storage %s, want the manifest's", h.Manifest.Layout.Storage)
	}
}

// TestBlockFilesKeepLegacyLookup: block index files left beside a database
// whose manifest says container (an upgrade that kept them, or one cut
// short) keep the block index lookup on.
func TestBlockFilesKeepLegacyLookup(t *testing.T) {
	root := setup(t)
	open(t, containerRequest(root)).Close()
	err := testutil.WriteLegacyBlockIndex(filepath.Join(root, cnst.BLOBSDIR), []testutil.LegacyBlockRecord{
		{Hash: make([]byte, 32), Path: "c.blob", Offset: 64, Size: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	open(t, Request{Path: root, Mode: ReadOnly})
	if !cnst.LegacyBlockIndex {
		t.Fatal("block index files present but the lookup is off")
	}
}
