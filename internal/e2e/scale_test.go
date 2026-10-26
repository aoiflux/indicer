package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
)

// TestFolderOfTenThousandFiles stores a folder of 10,000 files, a tenth of
// them empty and a hundred copies of others, with an E01 set among them. The
// segments are stored once, as one decoded evidence; every distinct content
// once; and a sample of the files restores to its content.
func TestFolderOfTenThousandFiles(t *testing.T) {
	testutil.LongTest(t)
	bin := testutil.BuildDues(t)
	work := t.TempDir()
	dir := filepath.Join(work, "case")
	const distinct, empty, copies = 9000, 900, 99
	sums := map[string]string{} // path -> sha256
	for _, d := range []string{"empty", "z-copies", "image"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := range distinct {
		sub := filepath.Join(dir, fmt.Sprintf("d%02d", i%50))
		if i < 50 {
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		data := testutil.RandomBytes(uint64(i+1), 1+(i*37)%8192)
		p := filepath.Join(sub, fmt.Sprintf("f%05d.bin", i))
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		sums[p] = testutil.BytesSHA256(data)
	}
	for i := range empty {
		p := filepath.Join(dir, "empty", fmt.Sprintf("e%04d", i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The copies sort after their originals, so the originals name the
	// evidence and the copies are its aliases.
	for i := range copies {
		src := filepath.Join(dir, fmt.Sprintf("d%02d", i%50), fmt.Sprintf("f%05d.bin", i))
		copyFile(t, src, filepath.Join(dir, "z-copies", fmt.Sprintf("c%03d.bin", i)))
	}
	e01 := fsfixtureImage(t, "ewf/fat12_multiseg")
	segs, err := filepath.Glob(strings.TrimSuffix(e01, filepath.Ext(e01)) + ".E0?")
	if err != nil || len(segs) < 2 {
		t.Fatalf("E01 segments: %v, %v", segs, err)
	}
	for _, s := range segs {
		copyFile(t, s, filepath.Join(dir, "image", filepath.Base(s)))
	}

	root := filepath.Join(work, "db")
	start := time.Now()
	r := testutil.RunDues(t, bin, work, 30*time.Minute, "store", dir, "--dbpath", root, "--no-index")
	if r.ExitCode != 0 {
		t.Fatalf("store: exit %d\n%s", r.ExitCode, r.LastLine())
	}
	t.Logf("stored %d files in %s", distinct+empty+copies+len(segs), time.Since(start).Round(time.Second))

	listed := listEvidence(t, bin, work, root)
	byName := map[string]listedEvidence{}
	for _, e := range listed {
		if e.Status != "completed" {
			t.Fatalf("%s is %s", e.Name, e.Status)
		}
		byName[e.Name] = e
	}
	// Each distinct content once, all empty files as one, the set as one.
	if want := distinct + 1 + 1; len(listed) != want {
		t.Fatalf("%d evidence listed, want %d", len(listed), want)
	}
	n := 0
	for p, sum := range sums {
		if n == 25 {
			break
		}
		n++
		e, ok := byName[filepath.Base(p)]
		if !ok {
			t.Fatalf("%s is not listed", p)
		}
		out := filepath.Join(work, "restored.bin")
		_ = os.Remove(out)
		duesMust(t, bin, work, "restore", e.Hash, "-f", out, "--dbpath", root)
		if testutil.FileSHA256(t, out) != sum {
			t.Fatalf("%s restored to other content", p)
		}
	}
	img, ok := byName[filepath.Base(segs[0])]
	if !ok {
		t.Fatalf("the E01 set is not listed by its first segment's name: %v", segs)
	}
	duesMust(t, bin, work, "restore", img.Hash, "-f", filepath.Join(work, "image.raw"), "--verify-ewf", "--dbpath", root)
}
