package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"indicer/lib/fsx"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// MarkerFileName is the one DUES-owned file inside the graph directory.
//
// Everything else DUES writes — configuration, keys, anchors, retained roots —
// lives outside it, on the same reasoning graphene encodes in
// ErrAnchorInsideStore. This file is the exception because it describes the
// encoding of what is inside, so separating it from the data it describes would
// be the thing that goes wrong.
//
// graphene ignores files it does not recognise, reading only graphene.wal,
// graphene.csr, graphene.NNNNNN.wal and its ledger files.
const MarkerFileName = "dues.schema.json"

// Marker is the on-disk record of how a store's labels are encoded.
type Marker struct {
	SchemaVersion int    `json:"schema_version"`
	WrittenBy     string `json:"written_by"`

	// CustomNodeBase and CustomEdgeBase are recorded explicitly rather than
	// re-derived from the linked library at read time. Recording them is what
	// turns a future band move into a refused open instead of silently empty
	// results.
	CustomNodeBase uint16 `json:"custom_node_base"`
	CustomEdgeBase uint16 `json:"custom_edge_base"`

	// MixedVocabulary records that the store holds both v1- and v2-encoded
	// labels. A mixed store answers correctly but cannot be raised to strict
	// posture or given ordered property keys.
	MixedVocabulary bool `json:"mixed_vocabulary"`

	NodeKinds map[string]NodeKind `json:"node_kinds"`
	EdgeKinds map[string]EdgeKind `json:"edge_kinds"`

	DynamicNodeKinds map[string]NodeKind `json:"dynamic_node_kinds"`

	OrderedNodeKeys []string `json:"ordered_node_keys"`
	OrderedEdgeKeys []string `json:"ordered_edge_keys"`

	MigratedFrom *int `json:"migrated_from"`
}

// NewMarker returns the marker this build writes for a fresh store.
func NewMarker(version string, mixed bool) Marker {
	nodeKinds := make(map[string]NodeKind, len(containerKinds))
	for _, kind := range containerKinds {
		nodeKinds[nodeKindNames[kind]] = kind
	}
	return Marker{
		SchemaVersion:    CurrentVersion,
		WrittenBy:        version,
		CustomNodeBase:   uint16(NewRegistry().NodeBase()),
		CustomEdgeBase:   uint16(NewRegistry().EdgeBase()),
		MixedVocabulary:  mixed,
		NodeKinds:        nodeKinds,
		EdgeKinds:        map[string]EdgeKind{edgeKindNames[EdgeKindRelation]: EdgeKindRelation},
		DynamicNodeKinds: map[string]NodeKind{},
		OrderedNodeKeys:  []string{},
		OrderedEdgeKeys:  []string{},
	}
}

// ReadMarker loads the marker from a graph directory. The second return
// distinguishes "no marker" — which is how every pre-existing store looks —
// from a read failure.
func ReadMarker(dir string) (Marker, bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, MarkerFileName))
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, false, nil
	}
	if err != nil {
		return Marker{}, false, fmt.Errorf("schema: read marker: %w", err)
	}
	if len(raw) == 0 {
		// Earlier releases wrote the marker without syncing it, so a crash
		// could leave it empty, and an empty marker refused every open of an
		// otherwise healthy store (GRA-7). It records nothing, so it is
		// treated as absent: the store is probed, and the next writable open
		// writes it again.
		logging.GetLogger().Warn("schema: marker is empty; treating the store as unmarked",
			zap.String("path", filepath.Join(dir, MarkerFileName)))
		return Marker{}, false, nil
	}

	var marker Marker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return Marker{}, false, fmt.Errorf(
			"schema: %s is unreadable (%w); it describes how this store's labels are "+
				"encoded, so opening without it risks misreading them",
			filepath.Join(dir, MarkerFileName), err)
	}
	return marker, true, nil
}

// WriteMarker persists the marker, replacing any existing one.
//
// The write goes to a unique temporary file that is synced and renamed into
// place, and the directory is synced, so a crash leaves either the old marker
// or the complete new one (GRA-7). The fixed ".tmp" name used before let two
// writers collide, and nothing was synced.
func WriteMarker(dir string, marker Marker) error {
	encoded, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("schema: encode marker: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := fsx.WriteFileAtomic(filepath.Join(dir, MarkerFileName), encoded, 0o644); err != nil {
		return fmt.Errorf("schema: write marker: %w", err)
	}
	return nil
}

// RegistryFor builds the registry a marker describes, after checking that the
// linked library still agrees with the bases it records.
func RegistryFor(marker Marker) (*Registry, error) {
	if err := CheckLibraryBases(marker.CustomNodeBase, marker.CustomEdgeBase); err != nil {
		return nil, err
	}
	if marker.SchemaVersion > CurrentVersion {
		return nil, fmt.Errorf(
			"schema: store is at version %d but this build understands %d; upgrade dues",
			marker.SchemaVersion, CurrentVersion)
	}
	if marker.MixedVocabulary {
		return NewLegacyRegistry(), nil
	}
	return NewRegistry(), nil
}
