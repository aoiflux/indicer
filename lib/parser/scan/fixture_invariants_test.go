package scan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fixture.Cleanup()
	os.Exit(code)
}

// TestFixtureImageDecodes checks the image layer: every fixture, including
// split EWF and VHD/VHDX containers, decodes to the disk the manifest
// describes.
func TestFixtureImageDecodes(t *testing.T) {
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
	"exfat/exfat|/empty.txt":                          "PAR-3 (zero-length files are not emitted)",
	"exfat/exfat|/sub":                                "PAR-15 (the exFAT adapter emits no directories)",
	"ext/ext4|/inline.txt":                            "PAR-3 (inline data has no runs; D2)",
	"ext/ext4|/prealloc.bin":                          "PAR-5 (unwritten extents read as data)",
	"ext/ext4|/unwritten.bin":                         "PAR-5 (unwritten extents read as data)",
	"hfs/hfs_classic|*":                               "PAR-26 (classic HFS is not detected)",
	"ntfs/ntfs_fsagen|/deep/one/two/three/leaf.txt":   "PAR-3 (resident content has no runs; D2)",
	"ntfs/ntfs_fsagen|/docs/link.txt":                 "PAR-3 (resident content has no runs; D2)",
	"ntfs/ntfs_fsagen|/docs/readme.txt":               "PAR-3 (resident content has no runs; D2)",
	"ntfs/ntfs_fsagen|/docs/resident.txt":             "PAR-3 (resident content has no runs; D2)",
	"ntfs/ntfs_fsagen|/docs/nonresident_ads_host.txt": "PAR-3 (resident content has no runs; D2)",

	// Deleted-file recovery, keyed "deleted:<path>".
	"ext/ext4_fsagen|deleted:/old/tobedeleted.bin":  "PAR-6 (deleted ext inode content not recovered)",
	"fat/fat12|deleted:/deleted.bin":                "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
	"fat/fat16|deleted:/deleted.bin":                "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
	"fat/fat32|deleted:/deleted.bin":                "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
	"fat/fat32_fsagen|deleted:/old/tobedeleted.bin": "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
	"vhd/fat16_dynamic_vhd|deleted:/deleted.bin":    "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
	"vhd/fat16_fixed_vhd|deleted:/deleted.bin":      "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
	"vhd/fat16_vhdx|deleted:/deleted.bin":           "PAR-29 (FAT deleted entry content wrong: cleared chain, no assume-contiguous)",
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
						checkFixtureFile(t, img, found[part.Offset], want)
					})
				}
			}
		})
	}
}

// TestFixtureDeletedRecovery checks that every deleted file the ground truth
// marks fully recoverable is reported as deleted, with its true content.
func TestFixtureDeletedRecovery(t *testing.T) {
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
						if sum, err := runsDigest(img, rec.Fragments, rec.Size); err == nil && sum == want.SHA256 {
							return
						}
					}
					t.Fatalf("no deleted record with the true content; deleted records: %v", names)
				})
			}
		}
	}
}

func checkFixtureFile(t *testing.T, img core.Image, recs map[string]core.FileRecord, want fixture.File) {
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
	sum, err := runsDigest(img, got.Fragments, got.Size)
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if sum != want.SHA256 {
		t.Fatalf("runs digest %s, want %s (%d runs, %d run bytes)", sum, want.SHA256, len(got.Fragments), got.ContentLen())
	}
}

var errRunsShort = errors.New("data runs cover less than the file size")

// runsDigest hashes the file content described by runs, cut to size, with
// sparse runs read as zeros.
func runsDigest(img core.Image, runs []core.AbsRange, size int64) (string, error) {
	h := sha256.New()
	var done int64
	buf := make([]byte, 1<<16)
	for _, r := range runs {
		n := r.Len()
		if done+n > size {
			n = size - done
		}
		for off := int64(0); off < n; {
			k := min(int64(len(buf)), n-off)
			chunk := buf[:k]
			if r.Sparse {
				clear(chunk)
			} else if got, err := img.ReadAt(chunk, r.Start+off); got != len(chunk) {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return "", err
			}
			h.Write(chunk)
			off += k
		}
		done += n
		if done >= size {
			break
		}
	}
	if done < size {
		return "", fmt.Errorf("%w: %d of %d", errRunsShort, done, size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
