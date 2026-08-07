package scan

import (
	"bytes"
	"testing"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

// TestWalkImageNoFilesystem exercises the full orchestration plumbing on an
// image with no partition table and no recognised filesystem: it must fall back
// to a whole-device partition, fail to mount a filesystem, and skip it — with no
// error and no records. (End-to-end file extraction is validated against real
// images in the differential corpus.)
func TestWalkImageNoFilesystem(t *testing.T) {
	img := image.NewRaw(bytes.NewReader(make([]byte, 1<<20)), 1<<20)
	defer img.Close()

	var count int
	err := WalkImage(img, func(core.Partition, core.FSType, core.FileRecord) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkImage: %v", err)
	}
	if count != 0 {
		t.Fatalf("records = %d, want 0 (no recognised filesystem)", count)
	}
}
