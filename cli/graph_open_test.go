package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/console"
	"indicer/lib/util"
)

// TestReadCommandsCreateNoGraph: list and micro list open the graph
// read-only, so on a database that has none they list without one and leave
// none behind (CFG-7, GRA-5). They used to create graph/ in the data root.
func TestReadCommandsCreateNoGraph(t *testing.T) {
	_, image := fixtureNamed(t, "fat/fat12")
	dbPath := filepath.Join(t.TempDir(), "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	if err := StoreData(t.Context(), opts, image, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	restore := console.Quiet()
	defer restore()

	if err := ListData(t.Context(), opts, "all"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := ListMicroArtefactsCmd(t.Context(), opts); err != nil {
		t.Fatalf("micro list: %v", err)
	}
	if _, err := os.Stat(util.GraphPath(dbPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a read command created the graph store (%v)", err)
	}
}

// TestListShowsEvidenceTheGraphLacks (GRA-24): with a graph present, list
// still lists from the database, so evidence stored without enrichment is
// listed, and enriched files carry what enrichment derived. It used to list
// only the evidence the graph held.
func TestListShowsEvidenceTheGraphLacks(t *testing.T) {
	_, enriched := fixtureNamed(t, "fat/fat12")
	_, plain := fixtureNamed(t, "ext/ext4")
	dbPath := filepath.Join(t.TempDir(), "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	restore := console.Quiet()
	if err := StoreData(t.Context(), opts, enriched, false, false, false, true); err != nil {
		restore()
		t.Fatal(err)
	}
	if err := StoreData(t.Context(), opts, plain, false, false, false, false); err != nil {
		restore()
		t.Fatal(err)
	}
	restore()

	var out bytes.Buffer
	console.Configure(&out, io.Discard)
	err := ListData(t.Context(), opts, "all")
	console.Reset()
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Evidence []struct {
			Name       string `json:"name"`
			Partitions []struct {
				IndexedFiles []map[string]any `json:"indexedFiles"`
			} `json:"partitions"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil {
		t.Fatalf("listing: %v\n%s", err, out.String())
	}
	names := map[string]bool{}
	annotated := 0
	for _, e := range listing.Evidence {
		names[e.Name] = true
		for _, p := range e.Partitions {
			for _, f := range p.IndexedFiles {
				if _, ok := f["mimeType"]; ok {
					annotated++
				}
			}
		}
	}
	if len(listing.Evidence) != 2 {
		t.Fatalf("listed %d evidence (%v), want both", len(listing.Evidence), names)
	}
	if annotated == 0 {
		t.Error("no indexed file carries what enrichment derived")
	}
}
