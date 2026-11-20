package cli

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/console"
	"indicer/internal/lifecycle"
	"indicer/lib/microartefact"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
)

// fat32Image builds a FAT32 image holding files.
func fat32Image(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "micro.img")
	d, err := diskfs.Create(path, 40<<20, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatal(err)
	}
	fsys, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: "MICRO"})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		f, err := fsys.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func artefactTotal(t *testing.T, dbPath string) int {
	t.Helper()
	repo, err := microartefact.OpenGrapheneRepositoryReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	tree, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}
	return tree.TotalArtefacts
}

// TestMicroExtract: micro extract finds the artefacts of a stored image's
// files, enriching the evidence first when it was stored without; it used to
// look files up under an ID enrichment never wrote, extract nothing and exit
// 0 (GRA-1). A second run skips the files, --force replaces their artefacts
// rather than adding to them (GRA-12), and the HTML view is not overwritten
// unless asked.
func TestMicroExtract(t *testing.T) {
	image := fat32Image(t, map[string]string{
		"/NOTES.TXT": "download from https://example.org/tool.zip\napi_key=abc123\n",
		"/PLAIN.TXT": "nothing to see here\n",
	})
	dbPath := filepath.Join(t.TempDir(), "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	restore := console.Quiet()
	defer restore()
	if err := StoreData(t.Context(), opts, image, false, false, false, false); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "view.html")
	if err := MicroArtefactCmd(t.Context(), opts, MicroOptions{Out: out}); err != nil {
		t.Fatalf("micro extract: %v", err)
	}
	first := artefactTotal(t, dbPath)
	if first == 0 {
		t.Fatal("no micro-artefacts extracted")
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("no HTML view: %v", err)
	}

	var usage *lifecycle.UsageError
	if err := MicroArtefactCmd(t.Context(), opts, MicroOptions{Out: out}); !errors.As(err, &usage) {
		t.Fatalf("a run over an existing HTML view: %v, want a usage error", err)
	}
	if err := MicroArtefactCmd(t.Context(), opts, MicroOptions{Out: out, Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if got := artefactTotal(t, dbPath); got != first {
		t.Fatalf("a second run changed the artefacts from %d to %d", first, got)
	}
	if err := MicroArtefactCmd(t.Context(), opts, MicroOptions{Out: out, Overwrite: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := artefactTotal(t, dbPath); got != first {
		t.Fatalf("--force left %d artefacts, want the %d it replaced", got, first)
	}
}
