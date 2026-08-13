package graph_test

import (
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/enrichment"
	"indicer/lib/microartefact"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden files from current output")

// legacyFixture copies the checked-in graphene v0.1.3 store into a temp
// directory and returns its data root. Tests never open the fixture in place:
// opening a graphene store appends to its WAL, which would mutate testdata.
func legacyFixture(t *testing.T) string {
	t.Helper()

	src := filepath.Join("testdata", "legacy-v1")
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("legacy fixture missing (%v); regenerate with:\n"+
			"  go test -tags fixturegen -run TestGenerateLegacyV1Fixture ./lib/graph/\n"+
			"...from a checkout pinned to graphene v0.1.3", err)
	}

	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func assertGolden(t *testing.T, name string, got any) {
	t.Helper()

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("updated golden %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v\nrun: go test ./lib/graph/ -update-golden", path, err)
	}
	if string(want) != string(encoded) {
		t.Errorf("golden %s mismatch\n--- want ---\n%s\n--- got ---\n%s", path, want, encoded)
	}
}

// TestLegacyFixture_EnrichmentHierarchy is one half of the regression that
// catches the graphene v0.1.3 -> v0.4.0 custom-type-base shift (128 -> 32768).
//
// The store opens either way — v0.4.0 reads CSR formats v2..v8 and v0.1.3 wrote
// v3 — so nothing errors when the vocabulary moves. Every NodesByType and every
// Types: filter simply returns zero rows, and this hierarchy comes back empty.
func TestLegacyFixture_EnrichmentHierarchy(t *testing.T) {
	root := legacyFixture(t)

	repo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	tree, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}

	if len(tree.EvidenceFiles) == 0 {
		t.Fatal("enrichment hierarchy is empty; the node-type vocabulary no longer " +
			"matches what the fixture holds on disk")
	}
	assertGolden(t, "golden_enrichment_hierarchy.json", tree)
}

// TestLegacyFixture_MicroArtefactHierarchy is the other half, and additionally
// covers the EdgeType(100) relation edges.
func TestLegacyFixture_MicroArtefactHierarchy(t *testing.T) {
	root := legacyFixture(t)

	repo, err := microartefact.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	tree, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}

	if tree.TotalArtefacts == 0 {
		t.Fatal("micro-artefact hierarchy has no artefacts; the node-type vocabulary " +
			"no longer matches what the fixture holds on disk")
	}
	if tree.TotalRelations == 0 {
		t.Fatal("micro-artefact hierarchy has no relations; the relation edge type " +
			"no longer matches what the fixture holds on disk")
	}
	assertGolden(t, "golden_micro_hierarchy.json", tree)
}

// TestLegacyFixture_OnDiskVocabulary pins the raw label values the fixture
// carries, independently of any DUES-side constant.
//
// This is the test that states the migration problem in one place: these
// numbers are what is on disk, and they do not change when the library's
// CustomNodeType() expression starts evaluating to something else. On v0.1.3
// the custom band starts at 128, so evidence/partition/indexed/file are
// 128/129/130/131; relation edges are a raw EdgeType(100), which was never
// offset-based and so is unaffected by the base moving.
func TestLegacyFixture_OnDiskVocabulary(t *testing.T) {
	root := legacyFixture(t)

	g, err := graphenedb.Open(filepath.Join(root, "graph"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	for _, tc := range []struct {
		name     string
		nodeType uint16
		wantMin  int
	}{
		{"evidence_file", 128, 1},
		{"partition", 129, 2},
		{"indexed_file", 130, 3},
		{"file", 131, 4},
	} {
		ids, err := g.NodesByType(graphstore.NodeType(tc.nodeType))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(ids) < tc.wantMin {
			t.Errorf("on-disk node type %d (%s): got %d nodes, want >= %d",
				tc.nodeType, tc.name, len(ids), tc.wantMin)
		}
	}

	relationIDs, err := g.EdgesByType(graphstore.EdgeType(100))
	if err != nil {
		t.Fatal(err)
	}
	if len(relationIDs) == 0 {
		t.Error("on-disk edge type 100 (relation): got 0 edges, want > 0")
	}

	microIDs, err := g.NodesByType(graphstore.NodeTypeMicroArtefact)
	if err != nil {
		t.Fatal(err)
	}
	if len(microIDs) == 0 {
		t.Error("built-in NodeTypeMicroArtefact: got 0 nodes, want > 0")
	}
}
