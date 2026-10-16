package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/fsfixture"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fsfixture.Cleanup()
	os.Exit(code)
}

// listedFile is one indexed file in `dues list` output.
type listedFile struct {
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	Size      int64  `json:"size"`
	IsDeleted bool   `json:"isDeleted"`
}

type listOutput struct {
	Evidence []struct {
		Name       string `json:"name"`
		Hash       string `json:"hash"`
		Completed  bool   `json:"completed"`
		Partitions []struct {
			FileName     string       `json:"fileName"`
			IndexedFiles []listedFile `json:"indexedFiles"`
		} `json:"partitions"`
	} `json:"evidence"`
}

// parseList extracts the JSON document from `dues list` output, which may be
// preceded by banner lines.
func parseList(out string) (listOutput, error) {
	var doc listOutput
	i := strings.Index(out, "{")
	j := strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return doc, fmt.Errorf("no JSON in list output")
	}
	return doc, json.Unmarshal([]byte(out[i:j+1]), &doc)
}

// ingestKnownDefects maps "<fixture>|<path>" (or "<fixture>|*") to the
// inventory ID that keeps a ground-truth file from being indexed and
// restored correctly today. The fix for each ID removes its entries.
var ingestKnownDefects = map[string]string{
	"ewf/fat12_multiseg|*":      "ING-7 (E01 indexed over the raw container)",
	"vhd/fat16_dynamic_vhd|*":   "ING-7 (VHD indexed over the raw container)",
	"vhd/fat16_vhdx|*":          "ING-7 (VHDX indexed over the raw container)",
	"hfs/hfs_classic|*":         "PAR-26 (classic HFS is not detected)",
	"exfat/exfat|/empty.txt":    "PAR-3 (zero-length files are not indexed)",
	"ext/ext4|/empty.txt":       "PAR-3 (zero-length files are not indexed)",
	"ext/ext4|/inline.txt":      "PAR-3 (inline data is not indexed; D2)",
	"ext/ext4|/sparse.bin":      "PAR-3 (files with holes are not indexed; D2)",
	"ext/ext4|/prealloc.bin":    "PAR-5 (unwritten extents read as data)",
	"ext/ext4|/unwritten.bin":   "PAR-5 (unwritten extents read as data)",
	"xfs/xfs|/sparse.bin":       "PAR-3 (files with holes are not indexed; D2)",
	"xfs/xfs|/multi.bin":        "PAR-3 (fragmented files are not indexed; D2)",
	"xfs/xfs|/unwritten.bin":    "PAR-3 (files with holes are not indexed; D2)",
	"xfs/xfs|/empty.txt":        "PAR-3 (zero-length files are not indexed)",
	"xfs/xfs|/hello.txt":        "PAR-4 (XFS content includes block slack)",
	"xfs/xfs|/hardlink.bin":     "PAR-4 (XFS content includes block slack)",
	"xfs/xfs|/slack.bin":        "PAR-4 (XFS content includes block slack)",
	"xfs/xfs|/sub/a.bin":        "PAR-4 (XFS content includes block slack)",
	"xfs/xfs|/sub/deeper/b.bin": "PAR-4 (XFS content includes block slack)",
	"ntfs/ntfs_fsagen|*small*":  "PAR-3 (resident content is not indexed; D2)",
}

func ingestDefect(fixture string, f fsfixture.File) (string, bool) {
	if id, ok := ingestKnownDefects[fixture+"|"+f.Path]; ok {
		return id, true
	}
	if id, ok := ingestKnownDefects[fixture+"|*"]; ok {
		return id, true
	}
	if f.Size == 0 {
		return "PAR-3 (zero-length files are not indexed)", true
	}
	if strings.HasPrefix(fixture, "ntfs/") && f.Size <= 700 {
		return ingestKnownDefects["ntfs/ntfs_fsagen|*small*"], true
	}
	return "", false
}

// TestIngestFixtures stores every fixture image with default settings, then
// checks that each live file in the ground truth is listed as an indexed
// file and restores to its true content.
func TestIngestFixtures(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the dues binary")
	}
	bin := testutil.BuildDues(t)
	for _, m := range fsfixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			image := fsfixture.Open(t, m)
			work := t.TempDir()
			db := filepath.Join(work, "db")

			store := testutil.RunDues(t, bin, work, 5*time.Minute, "store", image, "--dbpath", db)
			list := testutil.RunDues(t, bin, work, time.Minute, "list", "--dbpath", db)
			doc, err := parseList(list.Stdout)
			if err != nil {
				t.Fatalf("list: %v (exit %d)\nstore: %s\nlist: %s", err, list.ExitCode, store.LastLine(), list.LastLine())
			}
			indexed := map[string]listedFile{}
			for _, e := range doc.Evidence {
				for _, p := range e.Partitions {
					for _, f := range p.IndexedFiles {
						if !f.IsDeleted {
							indexed[f.Path] = f
						}
					}
				}
			}

			for _, part := range m.Partitions {
				for _, want := range part.Files {
					if want.Kind != "file" {
						continue
					}
					t.Run(want.Path, func(t *testing.T) {
						got, ok := indexed[want.Path]
						if !ok {
							if id, known := ingestDefect(m.Name, want); known {
								t.Skipf("known defect %s: not indexed (store exit %d: %s)", id, store.ExitCode, store.LastLine())
							}
							t.Fatalf("not indexed (store exit %d: %s)", store.ExitCode, store.LastLine())
						}
						out := filepath.Join(work, "restored.bin")
						_ = os.Remove(out)
						res := testutil.RunDues(t, bin, work, time.Minute, "restore", got.Hash, "-f", out, "--dbpath", db)
						outcome := fmt.Sprintf("restore exit %d: %s", res.ExitCode, res.LastLine())
						if res.ExitCode == 0 {
							if sum := testutil.FileSHA256(t, out); sum == want.SHA256 {
								return
							}
							if fi, err := os.Stat(out); err == nil {
								outcome = fmt.Sprintf("restored %d bytes, want %d, content differs", fi.Size(), want.Size)
							}
						}
						if id, known := ingestDefect(m.Name, want); known {
							t.Skipf("known defect %s: %s", id, outcome)
						}
						t.Fatal(outcome)
					})
				}
			}
		})
	}
}
