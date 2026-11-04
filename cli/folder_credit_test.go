package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/ingest"
)

// TestFolderJobsCountSharedChunksInFolderOrder: two evidence of different
// content that share chunks, stored at once with the later one finishing
// first, print what a store of one at a time prints: the chunks they share
// are new for the first in folder order, whichever stored them.
func TestFolderJobsCountSharedChunksInFolderOrder(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	in := filepath.Join(t.TempDir(), "in")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := testutil.RandomBytes(11, 1<<20)
	for _, f := range []struct {
		name string
		seed uint64
		size int
	}{{"a.bin", 12, 512 << 10}, {"b.bin", 13, 768 << 10}} {
		data := append(append([]byte(nil), shared...), testutil.RandomBytes(f.seed, f.size)...)
		if err := os.WriteFile(filepath.Join(in, f.name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	one, _ := storeReport(t, in, 1)

	// Two at a time, a.bin waits until b.bin is stored, so b.bin stores the
	// chunks they share.
	prev := storeOneFile
	t.Cleanup(func() { storeOneFile = prev })
	bStored := make(chan struct{})
	storeOneFile = func(ctx context.Context, w io.Writer, sess *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
		if filepath.Base(path) == "a.bin" {
			select {
			case <-bStored:
			case <-time.After(10 * time.Second):
				t.Error("b.bin was not stored beside a.bin")
			}
		}
		counted, err := prev(ctx, w, sess, path, sopts)
		if filepath.Base(path) == "b.bin" {
			close(bStored)
		}
		return counted, err
	}
	two, _ := storeReport(t, in, 2)
	if one != two {
		t.Fatalf("two at a time printed\n%s\n\none at a time printed\n%s", two, one)
	}
}
