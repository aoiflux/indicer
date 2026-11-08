package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/index"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// TestFolderJobsCountSharedChunksInFolderOrder: two evidence of different
// content that share chunks, stored at once with the later one finishing
// first, print what a store of one at a time prints: the chunks they share
// are new for the first in folder order, whichever stored them.
func TestFolderJobsCountSharedChunksInFolderOrder(t *testing.T) {
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

// TestFolderJobsCountWhatAnIndexStoresAfterItsEvidence: content an
// evidence's index stores beside it comes in folder order where that
// evidence does, and counts for no evidence. A later evidence holding that
// content, stored first, prints what a store of one at a time prints: none
// of it new, and the image none of it either.
func TestFolderJobsCountWhatAnIndexStoresAfterItsEvidence(t *testing.T) {
	in := filepath.Join(t.TempDir(), "in")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	_, img := fixtureNamed(t, "ntfs/ntfs_compressed")
	raw, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "a.img"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// The image's NTFS compressed /z/text.txt, which its index stores
	// decoded.
	if err := os.WriteFile(filepath.Join(in, "b.bin"), decodedContent(t, img, "/z/text.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	one, _ := storeReport(t, in, 1)
	if line := storageLineOf(one, "b.bin"); !strings.Contains(line, " 0 B new,") {
		t.Fatalf("one at a time, b.bin printed %q, want none of it new", line)
	}

	// Two at a time, a.img waits until b.bin is stored, so b.bin stores the
	// chunks the index of a.img stores after it.
	prev := storeOneFile
	t.Cleanup(func() { storeOneFile = prev })
	bStored := make(chan struct{})
	storeOneFile = func(ctx context.Context, w io.Writer, sess *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
		if filepath.Base(path) == "a.img" {
			select {
			case <-bStored:
			case <-time.After(10 * time.Second):
				t.Error("b.bin was not stored beside a.img")
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

// decodedContent stores img in a database of its own and returns the content
// its index stored decoded for the file path.
func decodedContent(t *testing.T, img, path string) []byte {
	t.Helper()
	d := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	res, err := ingest.Run(t.Context(), d.DB, ingest.Request{Path: img, Decode: true, Quiet: true})
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	err = index.ForEachIndexed(t.Context(), d.DB, res.EvidenceID, func(key []byte, rec structs.IndexedFile) error {
		if dbio.RecordPath(rec) != path {
			return nil
		}
		if rec.Content == nil || rec.Content.Kind != structs.ContentMaterialized {
			return fmt.Errorf("%s is not stored decoded: %+v", path, rec.Content)
		}
		l, err := dbio.OpenLogical(d.DB, key, dbio.ReadOptions{})
		if err != nil {
			return err
		}
		content, err = io.ReadAll(io.NewSectionReader(l, 0, l.Size()))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(content) == 0 {
		t.Fatalf("the index of %s stored nothing decoded for %s", img, path)
	}
	return content
}

// storageLineOf is the storage line a report printed for the evidence name.
func storageLineOf(report, name string) string {
	_, after, ok := strings.Cut(report, name+"\n")
	if !ok {
		return ""
	}
	for _, line := range strings.Split(after, "\n") {
		if strings.Contains(line, " read, ") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}
