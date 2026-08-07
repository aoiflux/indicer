package scan_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"

	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
	"indicer/lib/parser/scan"
)

// TestE2E_FAT32 builds a real FAT32 filesystem image (via go-diskfs) with known
// files, then runs the whole pure-Go stack over it and asserts:
//   - the filesystem is detected as FAT,
//   - the expected files/dirs are found with correct sizes,
//   - and file content read via the reported absolute fragment offsets matches —
//     i.e. the DataRuns → image-absolute AbsRange contract (the crux of the
//     libtusk replacement) is correct end-to-end.
func TestE2E_FAT32(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e image test in -short mode")
	}

	const diskSize = 100 * 1024 * 1024 // FAT32 minimum is ~33 MiB
	path := filepath.Join(t.TempDir(), "fat32.img")

	d, err := diskfs.Create(path, diskSize, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("diskfs.Create: %v", err)
	}
	fsys, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: "TESTVOL",
	})
	if err != nil {
		t.Fatalf("CreateFilesystem: %v", err)
	}

	writeFile := func(p, content string) {
		f, err := fsys.OpenFile(p, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("OpenFile(%s): %v", p, err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", p, err)
		}
		_ = f.Close()
	}

	const helloContent = "hello world from the pure-go parser stack"
	writeFile("/HELLO.TXT", helloContent)
	if err := fsys.Mkdir("/SUB"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	writeFile("/SUB/NESTED.TXT", "nested file content")

	if err := d.Close(); err != nil {
		t.Fatalf("disk.Close: %v", err)
	}

	// Reopen the raw image and walk it with the pure-Go stack.
	img, err := image.OpenRawFile(path)
	if err != nil {
		t.Fatalf("OpenRawFile: %v", err)
	}
	defer img.Close()

	byName := map[string]core.FileRecord{}
	var fsType core.FSType
	err = scan.WalkImage(img, func(_ core.Partition, ft core.FSType, rec core.FileRecord) error {
		fsType = ft
		byName[strings.ToUpper(rec.Name)] = rec
		return nil
	})
	if err != nil {
		t.Fatalf("WalkImage: %v", err)
	}

	if fsType != core.FSFAT {
		t.Fatalf("detected filesystem = %q, want fat", fsType)
	}

	// HELLO.TXT: size + content-via-absolute-fragments.
	hello, ok := byName["HELLO.TXT"]
	if !ok {
		t.Fatalf("HELLO.TXT not found; got names %v", names(byName))
	}
	if hello.Size != int64(len(helloContent)) {
		t.Fatalf("HELLO.TXT size = %d, want %d", hello.Size, len(helloContent))
	}
	if len(hello.Fragments) == 0 {
		t.Fatal("HELLO.TXT has no fragments")
	}
	if got := readFragments(t, img, hello); got != helloContent {
		t.Fatalf("HELLO.TXT via fragments = %q, want %q", got, helloContent)
	}

	// Nested file and its directory.
	if nested, ok := byName["NESTED.TXT"]; !ok {
		t.Fatalf("NESTED.TXT not found; got names %v", names(byName))
	} else if nested.Size != int64(len("nested file content")) {
		t.Fatalf("NESTED.TXT size = %d, want %d", nested.Size, len("nested file content"))
	}
	if sub, ok := byName["SUB"]; !ok || sub.Kind != core.KindDir {
		t.Fatalf("/SUB directory missing or wrong kind (ok=%v kind=%v)", ok, sub.Kind)
	}
}

// readFragments reassembles a record's content by reading its absolute image
// fragment offsets, trimmed to the file's logical size.
func readFragments(t *testing.T, img core.Image, rec core.FileRecord) string {
	t.Helper()
	var buf bytes.Buffer
	for _, fr := range rec.Fragments {
		n := fr.End - fr.Start + 1
		if fr.Sparse {
			buf.Write(make([]byte, n))
			continue
		}
		b := make([]byte, n)
		if _, err := img.ReadAt(b, fr.Start); err != nil && err != io.EOF {
			t.Fatalf("image.ReadAt(%d): %v", fr.Start, err)
		}
		buf.Write(b)
	}
	out := buf.Bytes()
	if int64(len(out)) > rec.Size {
		out = out[:rec.Size]
	}
	return string(out)
}

func names(m map[string]core.FileRecord) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
