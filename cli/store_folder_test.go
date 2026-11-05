package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	duesgraph "indicer/lib/graph"
	"indicer/lib/ingest"
	"indicer/lib/util"
)

// TestStoreZeroByteFile is an ING-11 regression: an empty file could not be
// memory-mapped, so storing one failed, and a folder holding one stopped
// there.
func TestStoreZeroByteFile(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := t.TempDir()
	opts := optsFor(filepath.Join(dir, "db"), matrixChunkKB, nil)
	empty := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StoreData(t.Context(), opts, empty, false, true, false, false); err != nil {
		t.Fatalf("store of an empty file: %v", err)
	}
	out := filepath.Join(dir, "restored.bin")
	if err := RestoreData(t.Context(), opts, plainHashB64(t, empty), out, RestoreOptions{}); err != nil {
		t.Fatalf("restore of an empty file: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() != 0 {
		t.Fatalf("restored empty file: %v, %v", fi, err)
	}
}

// TestStoreReleasesEvidenceFile is an ING-11/ING-5 regression: the evidence
// file's handle and mapping were released only when a store succeeded, so a
// store that failed kept the file open and mapped until DUES exited. Windows
// refuses to rename a file another handle holds open, so a rename after the
// failed store shows the handle is gone.
func TestStoreReleasesEvidenceFile(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := t.TempDir()
	opts := optsFor(filepath.Join(dir, "db"), matrixChunkKB, nil)
	src := testutil.WriteRandomFile(t, dir, "evidence.bin", 31, 3*matrixChunkKB*1024)

	// An unknown hash strategy fails the store after the file is opened.
	cnst.StoreHashStrategy = "no-such-strategy"
	if err := StoreData(t.Context(), opts, src, false, true, false, false); err == nil {
		t.Fatal("store with an unknown hash strategy succeeded")
	}
	if err := os.Rename(src, src+".moved"); err != nil {
		t.Fatalf("the failed store still holds the evidence file: %v", err)
	}
}

// TestStoreFolderContinuesPastErrors is an ING-11 regression: one file that
// failed to store stopped the whole folder ingest, so every file after it
// was silently left out.
func TestStoreFolderContinuesPastErrors(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	a := testutil.WriteRandomFile(t, in, "a.bin", 41, 5000)
	bad := testutil.WriteRandomFile(t, in, "b-bad.bin", 42, 5000)
	c := testutil.WriteRandomFile(t, in, "sub/c.bin", 43, 5000)

	prev := storeOneFile
	t.Cleanup(func() { storeOneFile = prev })
	storeOneFile = func(ctx context.Context, w io.Writer, sess *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
		if filepath.Base(path) == "b-bad.bin" {
			return ingest.Bytes{}, errors.New("simulated read failure")
		}
		return prev(ctx, w, sess, path, sopts)
	}

	opts := optsFor(filepath.Join(dir, "db"), matrixChunkKB, nil)
	err := StoreData(t.Context(), opts, in, false, true, false, false)
	if err == nil || !strings.Contains(err.Error(), "b-bad.bin") {
		t.Fatalf("folder store = %v, want an error naming the failed file", err)
	}
	for _, p := range []string{a, c} {
		out := filepath.Join(dir, "out-"+filepath.Base(p))
		if err := RestoreData(t.Context(), opts, plainHashB64(t, p), out, RestoreOptions{}); err != nil {
			t.Errorf("%s was not stored after an earlier file failed: %v", filepath.Base(p), err)
		}
	}
	_ = bad
}

// TestStoreFolderOpensStoresOnce: a folder store with the full-text and graph
// features opens the full-text index and the graph once for the whole
// folder; each evidence opened and closed both before.
func TestStoreFolderOpensStoresOnce(t *testing.T) {
	testdb.Isolate(t)
	d := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "in")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fat/fat12", "fat/fat16", "part/mbr_two"} {
		_, path := fixtureNamed(t, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(in, filepath.Base(name)+".img"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sess := ingest.NewSession(d.DB)
	err := storeFolder(t.Context(), io.Discard, sess, in, StoreOptions{FTS: true, Enrich: true, Decode: true, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sess.Opens(), (ingest.Opens{Text: 1, Graph: 1}); got != want {
		t.Fatalf("opens = %+v, want %+v", got, want)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStoreFailsWhenItsSessionDoesNotClose: a store whose graph or full-text
// index does not release cleanly fails, as one whose database does not close
// does (CFG-8), and what it stored stays stored.
func TestStoreFailsWhenItsSessionDoesNotClose(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	_, img := fixtureNamed(t, "fat/fat12")
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "a.img"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// Once the evidence is stored, another holder of the graph releases one
	// reference more than it took, so the session's release finds none left.
	prev := storeOneFile
	t.Cleanup(func() { storeOneFile = prev })
	storeOneFile = func(ctx context.Context, w io.Writer, sess *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
		counted, err := prev(ctx, w, sess, path, sopts)
		h, herr := duesgraph.Open(dbio.KVDir(sess.DB()))
		if herr != nil {
			t.Error(herr)
			return counted, err
		}
		for range 2 {
			if herr := h.Release(); herr != nil {
				t.Error(herr)
			}
		}
		return counted, err
	}
	dbPath := filepath.Join(dir, "db")
	err = StoreDataWithOptions(t.Context(), optsFor(dbPath, matrixChunkKB, nil), in, StoreOptions{Enrich: true, Decode: true, Quiet: true})
	if err == nil || !strings.Contains(err.Error(), "closing the full-text index and graph") {
		t.Fatalf("store = %v, want the session's close error", err)
	}
	if _, rec := onlyEvidence(t, dbPath); !rec.Completed {
		t.Fatalf("the evidence is not stored complete: %+v", rec)
	}
}
