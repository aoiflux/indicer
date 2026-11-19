package store

import (
	"bytes"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
)

// TestHashContentChunked: content hashed at a chunk size other than its
// database's has the root a database of that chunk size computes, and the
// same plain hash.
func TestHashContentChunked(t *testing.T) {
	t.Parallel()
	const smallKiB, largeKiB = 4, 16
	small := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: smallKiB})
	large := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: largeKiB})
	content := testutil.RandomBytes(13, 3*largeKiB*int(cnst.KB)+7)
	size := int64(len(content))

	want, err := HashContent(t.Context(), small.DB, bytes.NewReader(content), size, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := HashContentChunked(t.Context(), large.DB, bytes.NewReader(content), size, smallKiB*cnst.KB, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Plain, want.Plain) || !bytes.Equal(got.Root, want.Root) {
		t.Fatalf("hashed at %d KiB in a %d KiB database: %x %x, want %x %x", smallKiB, largeKiB, got.Plain, got.Root, want.Plain, want.Root)
	}
	own, err := HashContent(t.Context(), large.DB, bytes.NewReader(content), size, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(own.Root, want.Root) {
		t.Fatal("the root does not depend on the chunk size")
	}
	if _, err := HashContentChunked(t.Context(), large.DB, bytes.NewReader(content), size, 0, false); err == nil {
		t.Fatal("hashed at chunk size 0")
	}
}
