package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/lib/parser/core"
	"indicer/lib/parser/fs"
	"indicer/lib/parser/image"
	"indicer/lib/parser/partition"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fixture.Cleanup()
	os.Exit(code)
}

// TestFixtureImageDecodes checks the image layer: every fixture, including
// split EWF and VHD/VHDX containers, decodes to the disk the manifest
// describes.
func TestFixtureImageDecodes(t *testing.T) {
	t.Parallel()
	for _, m := range fixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			img, err := image.Open(fixture.Open(t, m))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer img.Close()
			if img.Size() != m.Size {
				t.Fatalf("decoded size %d, manifest %d", img.Size(), m.Size)
			}
			h := sha256.New()
			if _, err := io.Copy(h, io.NewSectionReader(img, 0, img.Size())); err != nil {
				t.Fatalf("read decoded image: %v", err)
			}
			if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
				t.Fatalf("decoded digest %s, manifest %s", got, m.SHA256)
			}
		})
	}
}

// knownFixtureDefects lists ground-truth entries today's parser gets wrong,
// keyed by "<fixture>|<path>" (or "<fixture>|*" for a whole fixture), with
// the bug inventory ID. The fix for each ID deletes its entries.
var knownFixtureDefects = map[string]string{

	// Deleted-file recovery, keyed "deleted:<path>".
	"ext/ext4_fsagen|deleted:/old/tobedeleted.bin": "PAR-6 (ext4 zeroes a deleted inode's size and extent tree; only the journal's copy could recover it, residual)",
}

func knownDefectFor(fixtureName, path string) (string, bool) {
	if id, ok := knownFixtureDefects[fixtureName+"|"+path]; ok {
		return id, true
	}
	if strings.HasPrefix(path, "deleted:") {
		return "", false
	}
	id, ok := knownFixtureDefects[fixtureName+"|*"]
	return id, ok
}

// TestFixtureInvariants walks every fixture and checks each live file in the
// ground truth is found, with the right kind and size, and that its data
// runs, read from the image and cut to its size, hash to the true content.
func TestFixtureInvariants(t *testing.T) {
	t.Parallel()
	for _, m := range fixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			img, err := image.Open(fixture.Open(t, m))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer img.Close()

			found := map[int64]map[string]core.FileRecord{}
			walkErr := WalkImage(img, func(p core.Partition, _ core.FSType, rec core.FileRecord) error {
				if rec.IsDeleted {
					return nil
				}
				if found[p.Offset] == nil {
					found[p.Offset] = map[string]core.FileRecord{}
				}
				found[p.Offset][rec.Path] = rec
				return nil
			})
			if walkErr != nil {
				t.Fatalf("walk: %v", walkErr)
			}

			for _, part := range m.Partitions {
				for _, want := range part.Files {
					t.Run(fmt.Sprintf("%d%s", part.Offset, want.Path), func(t *testing.T) {
						if id, ok := knownDefectFor(m.Name, want.Path); ok {
							t.Skipf("known defect %s", id)
						}
						checkFixtureFile(t, img, part.Offset, found[part.Offset], want)
					})
				}
			}
		})
	}
}

// TestFixtureDeletedRecovery checks that every deleted file the ground truth
// marks fully recoverable is reported as deleted, with its true content.
func TestFixtureDeletedRecovery(t *testing.T) {
	t.Parallel()
	for _, m := range fixture.All(t) {
		for _, part := range m.Partitions {
			for _, want := range part.Deleted {
				if want.Recoverable != "full" {
					continue
				}
				t.Run(fmt.Sprintf("%s/%d%s", m.Name, part.Offset, want.Path), func(t *testing.T) {
					if id, ok := knownDefectFor(m.Name, "deleted:"+want.Path); ok {
						t.Skipf("known defect %s", id)
					}
					img, err := image.Open(fixture.Open(t, m))
					if err != nil {
						t.Fatalf("open: %v", err)
					}
					defer img.Close()
					var match []core.FileRecord
					var names []string
					err = WalkImage(img, func(p core.Partition, _ core.FSType, rec core.FileRecord) error {
						if p.Offset == part.Offset && rec.IsDeleted && rec.Kind == core.KindFile {
							names = append(names, rec.Path)
							if rec.Size == want.Size {
								match = append(match, rec)
							}
						}
						return nil
					})
					if err != nil {
						t.Fatalf("walk: %v", err)
					}
					// Deleted names may be mangled (FAT replaces the first
					// character), so match on content, not on the path.
					for _, rec := range match {
						if rec.Content.Located != rec.Size {
							continue
						}
						if sum, err := contentDigest(img, rec.Content); err == nil && sum == want.SHA256 {
							return
						}
					}
					t.Fatalf("no deleted record with the true content; deleted records: %v", names)
				})
			}
		}
	}
}

func checkFixtureFile(t *testing.T, img core.Image, partOffset int64, recs map[string]core.FileRecord, want fixture.File) {
	t.Helper()
	got, ok := recs[want.Path]
	if !ok {
		t.Fatalf("not found (%d records in partition)", len(recs))
	}
	switch want.Kind {
	case "dir":
		if got.Kind != core.KindDir {
			t.Fatalf("kind %v, want directory", got.Kind)
		}
		return
	case "symlink":
		return // reported in W5 as KindSymlink; today's kind is not asserted
	}
	if got.Kind != core.KindFile {
		t.Fatalf("kind %v, want file", got.Kind)
	}
	if got.Size != want.Size {
		t.Fatalf("size %d, want %d", got.Size, want.Size)
	}
	if got.Content.Kind == core.ContentDecoded {
		// Decoded content exists only through the filesystem's reader.
		sum, err := openDigest(img, partOffset, want.Path)
		if err != nil {
			t.Fatalf("decoded content: %v", err)
		}
		if sum != want.SHA256 {
			t.Fatalf("decoded digest %s, want %s (%s)", sum, want.SHA256, got.Content.Prov)
		}
		return
	}
	if got.Content.Located != got.Size {
		t.Fatalf("located %d of %d bytes (%s, %s)", got.Content.Located, got.Size, got.Content.Kind, got.Content.Prov)
	}
	sum, err := contentDigest(img, got.Content)
	if err != nil {
		t.Fatalf("read content: %v", err)
	}
	if sum != want.SHA256 {
		t.Fatalf("content digest %s, want %s (%s, %d extents)", sum, want.SHA256, got.Content.Kind, len(got.Content.Extents))
	}
	checkFlags(t, got, want)
}

// checkFlags compares what the ground truth says about a file's layout with
// the content the parser reports.
func checkFlags(t *testing.T, got core.FileRecord, want fixture.File) {
	t.Helper()
	var data int
	var sparse, unwritten bool
	for _, e := range got.Content.Extents {
		switch e.Kind {
		case core.ExtentData:
			data++
		case core.ExtentSparse:
			sparse = true
		case core.ExtentUnwritten:
			unwritten = true
		}
	}
	if want.HasFlag("inline") && got.Content.Kind != core.ContentInline {
		t.Errorf("ground truth says inline, content is %s", got.Content.Kind)
	}
	if want.HasFlag("sparse") && !sparse {
		t.Errorf("ground truth says sparse, no hole reported")
	}
	if want.HasFlag("unwritten") && !unwritten && !sparse {
		t.Errorf("ground truth says unwritten, no unwritten or hole extent reported")
	}
	if want.HasFlag("fragmented") && data < 2 {
		t.Errorf("ground truth says fragmented, %d data extents", data)
	}
}

// contentDigest hashes the logical content c describes: extents read from
// the image (holes and unwritten ranges as zeros) or the inline bytes.
func contentDigest(img core.Image, c core.Content) (string, error) {
	h := sha256.New()
	switch c.Kind {
	case core.ContentNone:
	case core.ContentInline:
		h.Write(c.Inline)
	case core.ContentExtents:
		buf := make([]byte, 1<<16)
		for _, e := range c.Extents {
			for off := int64(0); off < e.Length; {
				k := min(int64(len(buf)), e.Length-off)
				chunk := buf[:k]
				if e.Kind != core.ExtentData {
					clear(chunk)
				} else if got, err := img.ReadAt(chunk, e.ImageOffset+off); got != len(chunk) {
					if err == nil {
						err = io.ErrUnexpectedEOF
					}
					return "", err
				}
				h.Write(chunk)
				off += k
			}
		}
	default:
		return "", fmt.Errorf("content is %s, not readable from the image", c.Kind)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// knownOpenDefects lists fixtures whose adapter Open (the library's own read
// path) is known to disagree with Content, keyed like knownFixtureDefects.
var knownOpenDefects = map[string]string{}

// TestFixtureContentMatchesOpen: for every live file the adapter locates in
// full, the bytes Content describes equal the bytes the filesystem library's
// own reader returns. The two paths are independent, so a disagreement means
// one of them is wrong.
func TestFixtureContentMatchesOpen(t *testing.T) {
	t.Parallel()
	for _, m := range fixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			img, err := image.Open(fixture.Open(t, m))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer img.Close()
			table, err := partition.Parse(img)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range table.Allocated() {
				section := io.NewSectionReader(img, p.Offset, p.Size)
				fsys, err := fs.OpenAuto(core.OpenParams{Reader: section, Base: p.Offset, Size: p.Size, ImageSize: img.Size()})
				if err != nil {
					continue
				}
				err = fsys.Walk(context.Background(), func(n core.Node) error {
					if n.Kind != core.KindFile || n.IsDeleted {
						return nil
					}
					if id, ok := knownOpenDefects[m.Name+"|"+n.Path]; ok {
						t.Logf("skip %s: known defect %s", n.Path, id)
						return nil
					}
					c, err := fsys.Content(n)
					if err != nil || c.Located != c.Size || (c.Kind != core.ContentExtents && c.Kind != core.ContentInline) {
						return nil
					}
					want, err := contentDigest(img, c)
					if err != nil {
						t.Errorf("%s: read content: %v", n.Path, err)
						return nil
					}
					rc, err := fsys.Open(n)
					if err != nil {
						t.Errorf("%s: open: %v", n.Path, err)
						return nil
					}
					h := sha256.New()
					k, err := io.Copy(h, io.LimitReader(rc, c.Size+1))
					rc.Close()
					if err != nil || k != c.Size {
						t.Errorf("%s: open read %d of %d bytes: %v", n.Path, k, c.Size, err)
						return nil
					}
					if got := hex.EncodeToString(h.Sum(nil)); got != want {
						t.Errorf("%s: open digest %s, content digest %s (%s)", n.Path, got, want, c.Kind)
					}
					return nil
				})
				fsys.Close()
				if err != nil {
					t.Errorf("walk partition at %d: %v", p.Offset, err)
				}
			}
		})
	}
}

// TestFixtureStreamsAndLinks: every alternate data stream in the ground truth
// is emitted as a "path:stream" record with its true content, and every file
// with more than one name is flagged a hard link.
func TestFixtureStreamsAndLinks(t *testing.T) {
	t.Parallel()
	for _, m := range fixture.All(t) {
		var streams, links int
		for _, part := range m.Partitions {
			for _, f := range part.Files {
				streams += len(f.Streams)
				if len(f.Links) > 1 {
					links++
				}
			}
		}
		if streams == 0 && links == 0 {
			continue
		}
		t.Run(m.Name, func(t *testing.T) {
			img, err := image.Open(fixture.Open(t, m))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer img.Close()
			found := map[int64]map[string]core.FileRecord{}
			if err := WalkImage(img, func(p core.Partition, _ core.FSType, rec core.FileRecord) error {
				if found[p.Offset] == nil {
					found[p.Offset] = map[string]core.FileRecord{}
				}
				found[p.Offset][rec.Path] = rec
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, part := range m.Partitions {
				for _, f := range part.Files {
					if len(f.Links) > 1 {
						if rec, ok := found[part.Offset][f.Path]; !ok || !rec.Prov.Has(core.ProvHardLink) {
							t.Errorf("%s: hard link not flagged (prov %s)", f.Path, rec.Prov)
						}
					}
					for _, s := range f.Streams {
						rec, ok := found[part.Offset][f.Path+":"+s.Name]
						if !ok {
							t.Errorf("%s:%s: stream not emitted", f.Path, s.Name)
							continue
						}
						if rec.Size != s.Size || rec.Content.Located != s.Size {
							t.Errorf("%s:%s: size %d located %d, want %d", f.Path, s.Name, rec.Size, rec.Content.Located, s.Size)
							continue
						}
						if sum, err := contentDigest(img, rec.Content); err != nil || sum != s.SHA256 {
							t.Errorf("%s:%s: digest %s (%v), want %s", f.Path, s.Name, sum, err, s.SHA256)
						}
					}
				}
			}
		})
	}
}

// openDigest mounts the partition at partOffset, finds path and hashes what
// the filesystem's own Open returns for it.
func openDigest(img core.Image, partOffset int64, path string) (string, error) {
	table, err := partition.Parse(img)
	if err != nil {
		return "", err
	}
	for _, p := range table.Allocated() {
		if p.Offset != partOffset {
			continue
		}
		fsys, err := fs.OpenAuto(core.OpenParams{Reader: io.NewSectionReader(img, p.Offset, p.Size), Base: p.Offset, Size: p.Size, ImageSize: img.Size()})
		if err != nil {
			return "", err
		}
		defer fsys.Close()
		var sum string
		var serr error
		found := errors.New("found")
		err = fsys.Walk(context.Background(), func(n core.Node) error {
			if n.Path != path {
				return nil
			}
			info, err := fsys.Stat(n)
			if err != nil {
				serr = err
				return found
			}
			rc, err := fsys.Open(n)
			if err != nil {
				serr = err
				return found
			}
			defer rc.Close()
			h := sha256.New()
			if k, err := io.Copy(h, io.LimitReader(rc, info.Size+1)); err != nil || k != info.Size {
				serr = fmt.Errorf("read %d of %d bytes: %v", k, info.Size, err)
				return found
			}
			sum = hex.EncodeToString(h.Sum(nil))
			return found
		})
		if !errors.Is(err, found) {
			return "", fmt.Errorf("%s not found (%v)", path, err)
		}
		return sum, serr
	}
	return "", fmt.Errorf("no partition at %d", partOffset)
}
