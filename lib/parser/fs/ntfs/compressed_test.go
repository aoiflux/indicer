package ntfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"indicer/lib/parser/partition"
)

// corpusNTFS names real images to check, separated as PATH is (';' on
// Windows, ':' elsewhere):
//
//	go test ./lib/parser/fs/ntfs -run TestDecoderMatchesLibntfs -args -dues.corpus-ntfs=IMAGE
var corpusNTFS = flag.String("dues.corpus-ntfs", "", "real images whose NTFS partitions TestDecoderMatchesLibntfs checks, as a path list")

// TestDecoderMatchesLibntfs: DUES decodes NTFS-compressed streams itself,
// because libntfs will not open a deleted record, and because libntfs v0.3.5
// decoded every compression unit at a quarter of its size (PAR-27). libntfs
// v0.3.6 reads them right, so each live compressed file of the fixtures must
// read the same through both decoders. -dues.corpus-ntfs adds images of the
// real corpus, each NTFS partition of which is checked too.
func TestDecoderMatchesLibntfs(t *testing.T) {
	t.Parallel()
	compressed := 0
	for _, m := range fsfixture.All(t) {
		if !strings.HasPrefix(m.Name, "ntfs/") {
			continue
		}
		raw, err := os.ReadFile(fsfixture.Open(t, m))
		if err != nil {
			t.Fatal(err)
		}
		compressed += checkDecoders(t, m.Name, bytes.NewReader(raw), int64(len(raw)))
	}
	if compressed == 0 {
		t.Fatal("no compressed file in the NTFS fixtures")
	}
	t.Logf("fixtures: %d compressed files", compressed)

	for _, path := range filepath.SplitList(*corpusNTFS) {
		src, err := image.OpenSource(context.Background(), path, image.Options{})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		tbl, err := partition.Parse(src.Image)
		if err != nil {
			src.Image.Close()
			t.Fatalf("%s: %v", path, err)
		}
		n := 0
		for _, p := range tbl.Partitions {
			if p.Size > 0 {
				n += checkDecoders(t, filepath.Base(path), io.NewSectionReader(src.Image, p.Offset, p.Size), p.Size)
			}
		}
		src.Image.Close()
		t.Logf("%s: %d compressed files", path, n)
	}
}

// checkDecoders reads every live compressed file of the NTFS volume on r
// through both decoders and returns how many it compared. A volume that is
// not NTFS is skipped.
func checkDecoders(t *testing.T, name string, r io.ReaderAt, size int64) int {
	t.Helper()
	fsys, err := Open(core.OpenParams{Reader: r, Size: size, ImageSize: size})
	if err != nil {
		return 0
	}
	defer fsys.Close()
	a := fsys.(*ntfsFS)
	compressed := 0
	err = fsys.Walk(context.Background(), func(n core.Node) error {
		if _, stream := n.Ref.(streamRef); n.Kind != core.KindFile || n.IsDeleted || stream {
			return nil
		}
		c, err := a.Content(n)
		if err != nil || c.Kind != core.ContentDecoded {
			return nil
		}
		compressed++
		rc, err := a.Open(n)
		if err != nil {
			t.Errorf("%s %s: open: %v", name, n.Path, err)
			return nil
		}
		ours := sha256.New()
		_, err = io.Copy(ours, rc)
		rc.Close()
		if err != nil {
			t.Errorf("%s %s: DUES decoder: %v", name, n.Path, err)
			return nil
		}
		f, err := a.vol.Open(n.ID)
		if err != nil {
			t.Errorf("%s %s: libntfs open: %v", name, n.Path, err)
			return nil
		}
		theirs := sha256.New()
		if _, err := io.Copy(theirs, io.NewSectionReader(f, 0, c.Size)); err != nil {
			t.Errorf("%s %s: libntfs decoder: %v", name, n.Path, err)
			return nil
		}
		if !bytes.Equal(ours.Sum(nil), theirs.Sum(nil)) {
			t.Errorf("%s %s: the DUES and libntfs decoders differ", name, n.Path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%s: walk: %v", name, err)
	}
	return compressed
}
