// Package migrate detects how an existing store encodes its labels, and moves
// stores forward when asked.
package migrate

import (
	"fmt"

	"indicer/lib/graph/schema"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
)

// Vocabulary classifies which label encodings a store holds.
type Vocabulary int

const (
	// VocabUnknown means the store holds nodes but none of the encodings this
	// build recognises. Refuse rather than guess: guessing wrong reads the
	// store as empty, which is the failure this whole mechanism exists to stop.
	VocabUnknown Vocabulary = iota
	// VocabEmpty is a store with no nodes. Safe to stamp as current.
	VocabEmpty
	// VocabLegacyV1 is a store written by graphene v0.1.3 only.
	VocabLegacyV1
	// VocabCurrentV2 is a store written by graphene v0.4.0 only.
	VocabCurrentV2
	// VocabMixed holds both, which is what a v1 store becomes as soon as this
	// build writes to it.
	VocabMixed
)

func (v Vocabulary) String() string {
	switch v {
	case VocabEmpty:
		return "empty"
	case VocabLegacyV1:
		return "legacy-v1"
	case VocabCurrentV2:
		return "current-v2"
	case VocabMixed:
		return "mixed"
	default:
		return "unknown"
	}
}

// Report is what detection found.
type Report struct {
	Vocabulary Vocabulary
	NodeCount  uint64

	// LegacyNodes and CurrentNodes count the probed container kinds under each
	// encoding, so a caller can say how much of a store is in which vocabulary.
	LegacyNodes  map[schema.NodeKind]int
	CurrentNodes map[schema.NodeKind]int

	LegacyRelationEdges  int
	CurrentRelationEdges int

	HasMarker bool
	Marker    schema.Marker
}

// Summary renders one line for a log or a status screen.
func (r Report) Summary() string {
	switch r.Vocabulary {
	case VocabEmpty:
		return "empty store"
	case VocabCurrentV2:
		return fmt.Sprintf("%d nodes, current vocabulary", r.NodeCount)
	case VocabLegacyV1:
		return fmt.Sprintf(
			"%d nodes written by graphene v0.1.3; labels are read through the compatibility shim",
			r.NodeCount)
	case VocabMixed:
		return fmt.Sprintf("%d nodes in mixed vocabulary; reads match both encodings", r.NodeCount)
	default:
		return fmt.Sprintf("%d nodes in an unrecognised vocabulary", r.NodeCount)
	}
}

// Detect classifies a store's label encoding.
//
// It probes rather than trusting the marker alone, because every store written
// before the marker existed has none, and those are exactly the stores that
// need classifying.
func Detect(dir string, g *graphenedb.Graph) (Report, error) {
	report := Report{
		LegacyNodes:  map[schema.NodeKind]int{},
		CurrentNodes: map[schema.NodeKind]int{},
	}

	marker, hasMarker, err := schema.ReadMarker(dir)
	if err != nil {
		return report, err
	}
	report.HasMarker = hasMarker
	report.Marker = marker

	if hasMarker {
		// A recorded base that disagrees with the linked library means the band
		// has moved again. Refuse loudly; do not probe past it.
		if err := schema.CheckLibraryBases(marker.CustomNodeBase, marker.CustomEdgeBase); err != nil {
			return report, err
		}
		if marker.SchemaVersion > schema.CurrentVersion {
			return report, fmt.Errorf(
				"migrate: store is at schema version %d but this build understands %d; upgrade dues",
				marker.SchemaVersion, schema.CurrentVersion)
		}
	}

	count, err := g.NodeCount()
	if err != nil {
		return report, fmt.Errorf("migrate: count nodes: %w", err)
	}
	report.NodeCount = count

	if count == 0 {
		report.Vocabulary = VocabEmpty
		return report, nil
	}

	// Counted by type rather than listed: listing materialised every node ID of
	// the four container kinds, at every open (GRA-6).
	nodeCounts, err := g.CountNodesByType()
	if err != nil {
		return report, fmt.Errorf("migrate: count nodes by type: %w", err)
	}
	edgeCounts, err := g.CountEdgesByType()
	if err != nil {
		return report, fmt.Errorf("migrate: count edges by type: %w", err)
	}

	current := schema.NewRegistry()
	for _, kind := range []schema.NodeKind{
		schema.KindEvidenceFile, schema.KindPartition, schema.KindIndexedFile, schema.KindFile,
	} {
		if n := nodeCounts[schema.LegacyNodeBase+graphstore.NodeType(kind)]; n > 0 {
			report.LegacyNodes[kind] = int(n)
		}
		if n := nodeCounts[current.NodeType(kind)]; n > 0 {
			report.CurrentNodes[kind] = int(n)
		}
	}
	report.LegacyRelationEdges = int(edgeCounts[schema.LegacyRelationEdge])
	report.CurrentRelationEdges = int(edgeCounts[current.EdgeType(schema.EdgeKindRelation)])

	sawLegacy := len(report.LegacyNodes) > 0 || report.LegacyRelationEdges > 0
	sawCurrent := len(report.CurrentNodes) > 0 || report.CurrentRelationEdges > 0

	switch {
	case sawLegacy && sawCurrent:
		report.Vocabulary = VocabMixed
	case sawLegacy:
		report.Vocabulary = VocabLegacyV1
	case sawCurrent:
		report.Vocabulary = VocabCurrentV2
	default:
		// Nodes exist but carry none of the container kinds. A store holding
		// only graphene's own types — micro-artefacts — is the benign case:
		// built-in labels sit below both custom bands and the band move never
		// touched them, so there is nothing to misread, marker or not (GRA-9).
		// Only a custom-band label this build does not know is refused.
		if hasMarker || onlyBuiltinTypes(nodeCounts) {
			report.Vocabulary = VocabCurrentV2
		} else {
			report.Vocabulary = VocabUnknown
		}
	}

	// The marker is authoritative about mixedness once it exists: a store can
	// be mixed with every legacy node currently filtered out of a probe.
	if hasMarker && marker.MixedVocabulary && report.Vocabulary == VocabCurrentV2 {
		report.Vocabulary = VocabMixed
	}

	return report, nil
}

// onlyBuiltinTypes reports whether every node type counted is one of
// graphene's own, below the oldest custom band.
func onlyBuiltinTypes(counts map[graphstore.NodeType]uint64) bool {
	for t, n := range counts {
		if n > 0 && t >= schema.LegacyNodeBase {
			return false
		}
	}
	return true
}

// RegistryFor builds the registry a detection report calls for.
func RegistryFor(report Report) (*schema.Registry, error) {
	switch report.Vocabulary {
	case VocabEmpty, VocabCurrentV2:
		return schema.NewRegistry(), nil
	case VocabLegacyV1, VocabMixed:
		return schema.NewLegacyRegistry(), nil
	default:
		return nil, fmt.Errorf(
			"migrate: store holds %d nodes in an unrecognised label vocabulary; "+
				"refusing to open rather than read it as empty. This build expects "+
				"custom node types based at %d (current) or %d (graphene v0.1.3)",
			report.NodeCount, schema.NewRegistry().NodeBase(), schema.LegacyNodeBase)
	}
}
