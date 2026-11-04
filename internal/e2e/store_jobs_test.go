package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/lib/fio"
)

// TestStoreJobsLeaveWhatOneAtATimeLeaves: a folder stored four evidence at a
// time leaves the database a store of one at a time leaves: the same
// evidence, each chunk once, as many bytes on disk, and it verifies. The
// folder holds copies of one content, which must alias one evidence, and
// files that share their first chunks, which evidence stored at once each
// wrote: in container mode as frames nothing points at.
func TestStoreJobsLeaveWhatOneAtATimeLeaves(t *testing.T) {
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	dir := filepath.Join(work, "case")
	shared := testutil.RandomBytes(5, 8<<16) // eight 64 KiB chunks
	for i := range 8 {
		tail := testutil.RandomBytes(uint64(100+i), 2<<16+777*i)
		writeFile(t, filepath.Join(dir, fmt.Sprintf("shared-%d.bin", i)), append(append([]byte{}, shared...), tail...))
	}
	copied := testutil.RandomBytes(6, 3<<16+5)
	for i := range 3 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("copy-%d.bin", i)), copied)
	}
	for i := range 4 {
		testutil.WriteRandomFile(t, dir, fmt.Sprintf("own-%d.bin", i), uint64(200+i), 1<<16+i)
	}

	for _, layout := range [][]string{nil, {"--container"}} {
		name := "file"
		if layout != nil {
			name = "container"
		}
		t.Run(name, func(t *testing.T) {
			var want string
			for _, jobs := range []string{"1", "4"} {
				root := filepath.Join(work, name+"-"+jobs)
				duesMust(t, bin, work, append([]string{"store", dir, "--dbpath", root, "-c", "64", "--store-jobs", jobs}, layout...)...)
				duesMust(t, bin, work, "repair", "--verify", "--dbpath", root)
				var lines []string
				for _, e := range listEvidence(t, bin, work, root) {
					lines = append(lines, e.Hash+" "+e.Name+" "+e.Status)
				}
				slices.Sort(lines)
				n, bytes := chunksOnDisk(t, root)
				got := fmt.Sprintf("%s\n%d chunks, %d bytes", strings.Join(lines, "\n"), n, bytes)
				if jobs == "1" {
					want = got
				} else if got != want {
					t.Fatalf("stored four at a time:\n%s\n\none at a time:\n%s", got, want)
				}
			}
		})
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// chunksOnDisk counts the chunks a data root holds, and their bytes: its
// blob files, or the frames of its containers, whose number varies with how
// the stores that wrote them overlapped.
func chunksOnDisk(t *testing.T, root string) (n int, bytes int64) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "blob", "*.blob"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !strings.HasPrefix(filepath.Base(p), "ctr-") {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			n, bytes = n+1, bytes+fi.Size()
			continue
		}
		if err := fio.ScanContainer(p, func(f fio.ContainerFrame) error {
			n, bytes = n+1, bytes+f.Size
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return n, bytes
}
