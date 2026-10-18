package schema

import (
	"fmt"
	"strconv"

	graphstore "github.com/aoiflux/graphene/store"
)

// CurrentVersion is the schema version this build writes.
//
//	1 — graphene v0.1.3 encoding: custom node band at 128, relation edges at a
//	    raw EdgeType(100). Never written by this build; only read.
//	2 — graphene v0.4.0 encoding: custom bands at 32768.
const CurrentVersion = 2

// Registry resolves kinds to the raw type values a particular store uses.
//
// A registry is per-store, not global, because two stores on one machine can
// legitimately be at different schema versions — one migrated, one not.
type Registry struct {
	version  int
	nodeBase graphstore.NodeType
	edgeBase graphstore.EdgeType

	// legacy records that this store also holds v1-encoded labels, so reads
	// have to match both encodings. Writes always use the current one.
	legacy bool
}

// NewRegistry returns a registry for a store written entirely by this build.
func NewRegistry() *Registry {
	return &Registry{
		version:  CurrentVersion,
		nodeBase: graphstore.NodeCustomBase,
		edgeBase: graphstore.EdgeCustomBase,
	}
}

// NewLegacyRegistry returns a registry for a store that holds v0.1.3-encoded
// labels. Reads match both encodings; writes use the current one, so the store
// becomes mixed as soon as anything is added to it.
func NewLegacyRegistry() *Registry {
	r := NewRegistry()
	r.legacy = true
	return r
}

// Version reports the schema version this registry writes.
func (r *Registry) Version() int { return r.version }

// Legacy reports whether reads have to match the v0.1.3 encoding as well.
func (r *Registry) Legacy() bool { return r != nil && r.legacy }

// NodeBase and EdgeBase report the custom-band bases in use, for the marker and
// for diagnostics.
func (r *Registry) NodeBase() graphstore.NodeType { return r.nodeBase }
func (r *Registry) EdgeBase() graphstore.EdgeType { return r.edgeBase }

// NodeType is the value to write for a kind.
func (r *Registry) NodeType(k NodeKind) graphstore.NodeType {
	return r.nodeBase + graphstore.NodeType(k)
}

// EdgeType is the value to write for a kind.
func (r *Registry) EdgeType(k EdgeKind) graphstore.EdgeType {
	return r.edgeBase + graphstore.EdgeType(k)
}

// MatchTypes returns every on-disk encoding a kind may have in this store,
// newest first. Pass it to NodesByAnyType rather than calling NodesByType with
// a single value.
//
// Only the four container kinds ever reached disk under v1, so only they are
// widened; adding the rest would cost a posting-list lookup per query for
// values that cannot exist.
func (r *Registry) MatchTypes(k NodeKind) []graphstore.NodeType {
	current := r.NodeType(k)
	if !r.legacy || k > legacyMaxNodeKind {
		return []graphstore.NodeType{current}
	}
	return []graphstore.NodeType{current, LegacyNodeBase + graphstore.NodeType(k)}
}

// MatchEdgeTypes returns every on-disk encoding an edge kind may have.
//
// EdgeKindRelation is special: its v1 encoding was a raw EdgeType(100), not an
// offset from the custom base, so it cannot be derived the way node kinds are.
func (r *Registry) MatchEdgeTypes(k EdgeKind) []graphstore.EdgeType {
	current := r.EdgeType(k)
	if !r.legacy || k != EdgeKindRelation {
		return []graphstore.EdgeType{current}
	}
	return []graphstore.EdgeType{current, LegacyRelationEdge}
}

// Is reports whether a node carries the given kind under any encoding this
// store uses. It replaces node.HasLabel(someDerivedConstant), which is the call
// shape that broke silently when the band moved.
func (r *Registry) Is(node *graphstore.Node, k NodeKind) bool {
	if node == nil {
		return false
	}
	for _, t := range r.MatchTypes(k) {
		if node.HasLabel(t) {
			return true
		}
	}
	return false
}

// IsEdge reports whether an edge carries the given kind under any encoding.
func (r *Registry) IsEdge(edge *graphstore.Edge, k EdgeKind) bool {
	if edge == nil {
		return false
	}
	for _, t := range r.MatchEdgeTypes(k) {
		if edge.HasLabel(t) {
			return true
		}
	}
	return false
}

// Name renders a kind for a CLI listing, a query plan or a visualisation
// legend. Unnamed kinds render as their offset rather than collapsing to one
// string, so two different kinds stay distinguishable.
func (r *Registry) Name(k NodeKind) string {
	if name, ok := nodeKindNames[k]; ok {
		return name
	}
	return "node_kind_" + strconv.FormatUint(uint64(k), 10)
}

// EdgeName renders an edge kind.
func (r *Registry) EdgeName(k EdgeKind) string {
	if name, ok := edgeKindNames[k]; ok {
		return name
	}
	return "edge_kind_" + strconv.FormatUint(uint64(k), 10)
}

// ParseKind resolves a name to a node kind, for CLI selectors.
func ParseKind(name string) (NodeKind, bool) {
	for kind, known := range nodeKindNames {
		if known == name {
			return kind, true
		}
	}
	return 0, false
}

// ParseEdgeKind resolves a name to an edge kind.
func ParseEdgeKind(name string) (EdgeKind, bool) {
	for kind, known := range edgeKindNames {
		if known == name {
			return kind, true
		}
	}
	return 0, false
}

// CheckLibraryBases verifies that the bases recorded for a store still match
// what the linked graphene exposes.
//
// This is the generalised fix for the bug that motivated the package: rather
// than trusting that the band has not moved, every open compares the recorded
// base against the live one and refuses on a mismatch. A future move is then a
// loud error at open instead of empty query results in a report.
func CheckLibraryBases(recordedNode uint16, recordedEdge uint16) error {
	if graphstore.NodeType(recordedNode) != graphstore.NodeCustomBase {
		return fmt.Errorf(
			"schema: store records a custom node base of %d but the linked graphene uses %d; "+
				"the library's custom band has moved and this store's labels would be "+
				"misread; rebuild the graph: remove the data root's graph directory, then run 'dues enrich' and 'dues micro extract'",
			recordedNode, graphstore.NodeCustomBase)
	}
	if graphstore.EdgeType(recordedEdge) != graphstore.EdgeCustomBase {
		return fmt.Errorf(
			"schema: store records a custom edge base of %d but the linked graphene uses %d; "+
				"the library's custom band has moved and this store's labels would be "+
				"misread; rebuild the graph: remove the data root's graph directory, then run 'dues enrich' and 'dues micro extract'",
			recordedEdge, graphstore.EdgeCustomBase)
	}
	return nil
}
