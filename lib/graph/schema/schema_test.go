package schema

import (
	"os"
	"path/filepath"
	"testing"

	graphstore "github.com/aoiflux/graphene/store"
)

// TestMatchTypes_WidensOnlyForLegacyStores is the shim's contract. A current
// store pays one posting-list lookup per query; a legacy store pays two, and
// only for the four kinds that could ever have reached disk under v1.
func TestMatchTypes_WidensOnlyForLegacyStores(t *testing.T) {
	current := NewRegistry()
	legacy := NewLegacyRegistry()

	if got := current.MatchTypes(KindEvidenceFile); len(got) != 1 {
		t.Errorf("current store: MatchTypes returned %v, want a single value", got)
	}

	got := legacy.MatchTypes(KindEvidenceFile)
	if len(got) != 2 {
		t.Fatalf("legacy store: MatchTypes returned %v, want two values", got)
	}
	if got[0] != graphstore.NodeCustomBase {
		t.Errorf("current encoding = %d, want %d", got[0], graphstore.NodeCustomBase)
	}
	if got[1] != 128 {
		t.Errorf("legacy encoding = %d, want 128", got[1])
	}

	// Kinds that never existed under v1 must not be widened: there is nothing
	// to find at the legacy offset, and the extra lookup would cost every query.
	if got := legacy.MatchTypes(KindProcess); len(got) != 1 {
		t.Errorf("legacy store: MatchTypes(KindProcess) returned %v, want a single value", got)
	}
}

// The four container kinds must keep the offsets the v1 labels were written
// with, or the shim maps old values onto the wrong kind — which is worse than
// not reading them at all.
func TestContainerKinds_OffsetsAreFrozen(t *testing.T) {
	for kind, wantLegacy := range map[NodeKind]graphstore.NodeType{
		KindEvidenceFile: 128,
		KindPartition:    129,
		KindIndexedFile:  130,
		KindFile:         131,
	} {
		got := NewLegacyRegistry().MatchTypes(kind)
		if len(got) != 2 || got[1] != wantLegacy {
			t.Errorf("%s: legacy encoding = %v, want %d",
				NewRegistry().Name(kind), got, wantLegacy)
		}
	}
}

// EdgeType(100) was a raw literal below the old custom base, not an offset from
// it, so it is the one kind whose legacy encoding cannot be derived.
func TestMatchEdgeTypes_RelationIsSpecialCased(t *testing.T) {
	legacy := NewLegacyRegistry()

	got := legacy.MatchEdgeTypes(EdgeKindRelation)
	if len(got) != 2 {
		t.Fatalf("MatchEdgeTypes(relation) = %v, want two values", got)
	}
	if got[0] != graphstore.EdgeCustomBase {
		t.Errorf("current encoding = %d, want %d", got[0], graphstore.EdgeCustomBase)
	}
	if got[1] != 100 {
		t.Errorf("legacy encoding = %d, want 100 (the raw literal DUES wrote)", got[1])
	}

	// Every other edge kind is offset-derived and has no legacy encoding.
	if got := legacy.MatchEdgeTypes(EdgeKindExecuted); len(got) != 1 {
		t.Errorf("MatchEdgeTypes(executed) = %v, want a single value", got)
	}
}

func TestIs_MatchesEitherEncoding(t *testing.T) {
	legacy := NewLegacyRegistry()

	old := &graphstore.Node{Labels: []graphstore.NodeType{130}}
	if !legacy.Is(old, KindIndexedFile) {
		t.Error("a v1-encoded indexed_file node was not recognised")
	}

	fresh := &graphstore.Node{Labels: []graphstore.NodeType{legacy.NodeType(KindIndexedFile)}}
	if !legacy.Is(fresh, KindIndexedFile) {
		t.Error("a v2-encoded indexed_file node was not recognised")
	}

	if legacy.Is(old, KindPartition) {
		t.Error("a v1 indexed_file node was recognised as a partition")
	}
	if legacy.Is(nil, KindFile) {
		t.Error("a nil node matched a kind")
	}

	// A current-only registry must not match legacy labels: that is the whole
	// distinction the marker records.
	if NewRegistry().Is(old, KindIndexedFile) {
		t.Error("a current-only registry matched a v1-encoded label")
	}
}

// No two kinds may share an offset, in either space. A collision would make two
// concepts indistinguishable on disk, permanently.
func TestKindOffsets_AreUnique(t *testing.T) {
	seenNode := map[NodeKind]string{}
	for kind, name := range nodeKindNames {
		if other, clash := seenNode[kind]; clash {
			t.Errorf("node kinds %q and %q share offset %d", other, name, kind)
		}
		seenNode[kind] = name
	}

	seenEdge := map[EdgeKind]string{}
	for kind, name := range edgeKindNames {
		if other, clash := seenEdge[kind]; clash {
			t.Errorf("edge kinds %q and %q share offset %d", other, name, kind)
		}
		seenEdge[kind] = name
	}
}

// Every named kind must fit graphene's custom band. CustomNodeType panics above
// the maximum offset, so a kind past it would crash at the first write.
func TestKindOffsets_FitTheCustomBand(t *testing.T) {
	maxOffset := uint16(graphstore.NodeCustomEnd)
	for kind, name := range nodeKindNames {
		if uint16(kind) > maxOffset {
			t.Errorf("node kind %q has offset %d, past the maximum %d", name, kind, maxOffset)
		}
	}
	for kind, name := range edgeKindNames {
		if uint16(kind) > maxOffset {
			t.Errorf("edge kind %q has offset %d, past the maximum %d", name, kind, maxOffset)
		}
	}

	// The registry must agree with the library's own derivation for a kind in
	// range; if these ever diverge the shim is resolving to the wrong values.
	if got, want := NewRegistry().NodeType(KindIndexedFile), graphstore.CustomNodeType(uint16(KindIndexedFile)); got != want {
		t.Errorf("registry resolves indexed_file to %d, graphene derives %d", got, want)
	}
}

func TestName_RendersUnnamedKindsDistinctly(t *testing.T) {
	r := NewRegistry()
	if got := r.Name(KindIndexedFile); got != "indexed_file" {
		t.Errorf("Name = %q, want %q", got, "indexed_file")
	}
	first, second := r.Name(NodeKind(9000)), r.Name(NodeKind(9001))
	if first == second {
		t.Errorf("two unnamed kinds both render as %q; they must stay distinguishable", first)
	}
}

func TestParseKind_RoundTrips(t *testing.T) {
	r := NewRegistry()
	for kind := range nodeKindNames {
		parsed, ok := ParseKind(r.Name(kind))
		if !ok || parsed != kind {
			t.Errorf("ParseKind(%q) = %d, %v; want %d, true", r.Name(kind), parsed, ok, kind)
		}
	}
	if _, ok := ParseKind("not_a_kind"); ok {
		t.Error("ParseKind accepted an unknown name")
	}
}

// This is the generalised fix: a store records the base it was written with, so
// a future band move is a refused open rather than empty query results.
func TestCheckLibraryBases_RefusesAMovedBand(t *testing.T) {
	if err := CheckLibraryBases(uint16(graphstore.NodeCustomBase), uint16(graphstore.EdgeCustomBase)); err != nil {
		t.Errorf("current bases were rejected: %v", err)
	}
	if err := CheckLibraryBases(128, uint16(graphstore.EdgeCustomBase)); err == nil {
		t.Error("a stale node base was accepted; a moved band must refuse the open")
	}
	if err := CheckLibraryBases(uint16(graphstore.NodeCustomBase), 128); err == nil {
		t.Error("a stale edge base was accepted")
	}
}

func TestMarker_RoundTrips(t *testing.T) {
	dir := t.TempDir()

	if _, found, err := ReadMarker(dir); err != nil || found {
		t.Fatalf("ReadMarker on an unmarked directory = found %v, err %v; want false, nil", found, err)
	}

	want := NewMarker("dues test", true)
	if err := WriteMarker(dir, want); err != nil {
		t.Fatal(err)
	}

	got, found, err := ReadMarker(dir)
	if err != nil || !found {
		t.Fatalf("ReadMarker = found %v, err %v", found, err)
	}
	if got.SchemaVersion != CurrentVersion {
		t.Errorf("SchemaVersion = %d, want %d", got.SchemaVersion, CurrentVersion)
	}
	if !got.MixedVocabulary {
		t.Error("MixedVocabulary was not preserved")
	}
	if got.CustomNodeBase != uint16(graphstore.NodeCustomBase) {
		t.Errorf("CustomNodeBase = %d, want %d", got.CustomNodeBase, graphstore.NodeCustomBase)
	}
	if got.NodeKinds["indexed_file"] != KindIndexedFile {
		t.Errorf("node kind table did not round-trip: %v", got.NodeKinds)
	}

	// No stray temporary file from the atomic write.
	if _, err := os.Stat(filepath.Join(dir, MarkerFileName+".tmp")); !os.IsNotExist(err) {
		t.Error("WriteMarker left its temporary file behind")
	}
}

func TestMarker_UnreadableIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MarkerFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMarker(dir); err == nil {
		t.Error("a corrupt marker was accepted; it describes the label encoding, so " +
			"opening without understanding it risks misreading the store")
	}
}

func TestRegistryFor_HonoursMixedAndRefusesTheFuture(t *testing.T) {
	mixed := NewMarker("t", true)
	r, err := RegistryFor(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Legacy() {
		t.Error("a mixed marker produced a registry that does not widen reads")
	}

	clean := NewMarker("t", false)
	r, err = RegistryFor(clean)
	if err != nil {
		t.Fatal(err)
	}
	if r.Legacy() {
		t.Error("a clean marker produced a widening registry")
	}

	future := NewMarker("t", false)
	future.SchemaVersion = CurrentVersion + 1
	if _, err := RegistryFor(future); err == nil {
		t.Error("a marker from a newer build was accepted")
	}
}
