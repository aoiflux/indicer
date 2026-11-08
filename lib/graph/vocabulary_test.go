package graph_test

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/enrichment"
	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/migrate"
	"indicer/lib/graph/schema"

	graphstore "github.com/aoiflux/graphene/store"
)

// TestVocabulary_LegacyFixtureIsDetectedAndShimmed is the automatic half of the
// migration: opening a store written by graphene v0.1.3 classifies it, widens
// reads, and stamps a marker — with no user action and nothing rewritten.
func TestVocabulary_LegacyFixtureIsDetectedAndShimmed(t *testing.T) {
	t.Parallel()
	root := legacyFixture(t)

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if got := handle.Vocabulary(); got != migrate.VocabLegacyV1 {
		t.Errorf("Vocabulary = %v, want %v", got, migrate.VocabLegacyV1)
	}
	if !handle.Schema().Legacy() {
		t.Error("registry does not widen reads for a legacy store; every type query " +
			"would return nothing")
	}

	marker, found, err := schema.ReadMarker(handle.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("opening a legacy store did not stamp a schema marker")
	}
	if !marker.MixedVocabulary {
		t.Error("marker does not record that reads must stay widened")
	}
	if marker.MigratedFrom == nil || *marker.MigratedFrom != 1 {
		t.Errorf("marker.MigratedFrom = %v, want 1", marker.MigratedFrom)
	}
}

// Opening must not rewrite records. The shim is a read-side widening, not a
// migration: rewriting stored evidence to correct a client-side constant would
// change every node's version hash.
func TestVocabulary_OpeningALegacyStoreRewritesNothing(t *testing.T) {
	t.Parallel()
	root := legacyFixture(t)
	wal := filepath.Join(root, "graph", "graphene.wal")

	before, err := os.ReadFile(wal)
	if err != nil {
		t.Fatal(err)
	}

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := enrichment.NewGrapheneRepositoryOn(handle)
	if _, err := repo.ReadHierarchy(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(wal)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("the write-ahead log changed size on open and read (%d -> %d bytes); "+
			"opening a legacy store must not rewrite records", len(before), len(after))
	}
}

func TestVocabulary_FreshStoreIsCurrentAndMarked(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if got := handle.Vocabulary(); got != migrate.VocabEmpty {
		t.Errorf("Vocabulary of a new store = %v, want %v", got, migrate.VocabEmpty)
	}
	if handle.Schema().Legacy() {
		t.Error("a new store should not widen reads")
	}

	marker, found, err := schema.ReadMarker(handle.Dir())
	if err != nil || !found {
		t.Fatalf("new store was not marked: found %v, err %v", found, err)
	}
	if marker.MixedVocabulary {
		t.Error("a new store was marked as mixed")
	}
	if marker.SchemaVersion != schema.CurrentVersion {
		t.Errorf("SchemaVersion = %d, want %d", marker.SchemaVersion, schema.CurrentVersion)
	}
}

// Writing to a legacy store makes it genuinely mixed: new nodes carry the
// current encoding while the old ones keep theirs. Both must stay readable.
func TestVocabulary_WritingToALegacyStoreYieldsMixed(t *testing.T) {
	t.Parallel()
	root := legacyFixture(t)

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	repo := enrichment.NewGrapheneRepositoryOn(handle)

	before, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}

	if err := repo.UpsertEvidence(enrichment.EvidenceRecord{
		HashBase64: "bmV3LWV2aWRlbmNl",
		Name:       "second.E01",
		Path:       "/cases/2026-114/second.E01",
		Size:       4096,
	}); err != nil {
		t.Fatal(err)
	}

	after, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.EvidenceFiles) != len(before.EvidenceFiles)+1 {
		t.Fatalf("evidence files: %d before, %d after; want one more — a node written "+
			"in the current encoding alongside legacy ones must still be found",
			len(before.EvidenceFiles), len(after.EvidenceFiles))
	}
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}

	// Reopening classifies it as mixed from the data itself.
	reopened, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Release()

	if got := reopened.Vocabulary(); got != migrate.VocabMixed {
		t.Errorf("Vocabulary after writing = %v, want %v", got, migrate.VocabMixed)
	}
	tree, err := enrichment.NewGrapheneRepositoryOn(reopened).ReadHierarchy()
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.EvidenceFiles) != len(after.EvidenceFiles) {
		t.Errorf("after reopen: %d evidence files, want %d",
			len(tree.EvidenceFiles), len(after.EvidenceFiles))
	}
}

// TestVocabulary_ReRunOnLegacyStoreDoesNotDuplicateEdges guards a regression
// the batch write path could easily have introduced.
//
// EnsureEdge deduplicates through an edge_key index entry, which edges written
// before that key existed do not have. Without the incident-edge fallback, a
// re-run against a legacy store would find no match for any of them and write a
// second copy of every containment edge in the case.
func TestVocabulary_ReRunOnLegacyStoreDoesNotDuplicateEdges(t *testing.T) {
	t.Parallel()
	root := legacyFixture(t)

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	before, err := handle.Graph().EdgesByType(graphstore.EdgeTypeContains)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("fixture holds no containment edges")
	}

	// Re-assert the hierarchy the fixture already contains.
	repo := enrichment.NewGrapheneRepositoryOn(handle)
	if err := repo.UpsertPartition(enrichment.PartitionRecord{
		EvidenceFileID:   "ZXZpZGVuY2UtaGFzaC1kaXNrMQ==",
		EvidenceFileName: "disk1.E01",
		PartitionID:      "cGFydGl0aW9uLTE=",
		PartitionName:    "p1",
	}); err != nil {
		t.Fatal(err)
	}

	after, err := handle.Graph().EdgesByType(graphstore.EdgeTypeContains)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("re-running enrichment on a legacy store changed the containment edge "+
			"count from %d to %d; edges written before edge_key existed were not "+
			"recognised as duplicates", len(before), len(after))
	}
}

// A read-only open must not stamp the marker: inspecting evidence should not
// write to it.
func TestVocabulary_ReadOnlyOpenDoesNotMark(t *testing.T) {
	t.Parallel()
	root := legacyFixture(t)

	handle, err := duesgraph.OpenWith(root, duesgraph.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if _, found, err := schema.ReadMarker(handle.Dir()); err != nil || found {
		t.Errorf("a read-only open wrote a marker (found %v, err %v)", found, err)
	}
	if !handle.Schema().Legacy() {
		t.Error("read-only open of a legacy store does not widen reads")
	}
}
