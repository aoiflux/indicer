package graphene

import (
	"sort"
	"time"

	"indicer/lib/enrichment"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/hierarchy"
	"indicer/lib/microartefact/model"

	graphstore "github.com/aoiflux/graphene/store"
)

// EvidenceFileHierarchy is the top-level result of ReadHierarchy. It reflects
// the enforced hierarchy: evidence_file → partition_file → indexed_file →
// micro_artefact.
type EvidenceFileHierarchy struct {
	hierarchy.ContainerHierarchy[*EvidenceFileNode]
	TotalArtefacts         int
	TotalRelations         int
	DeterministicRelations int
	ProbabilisticRelations int
}

type EvidenceFileNode = hierarchy.EvidenceFileNode[*PartitionFileNode]
type PartitionFileNode = hierarchy.PartitionFileNode[*IndexedFileArtefactView]

// IndexedFileArtefactView is one indexed file with its contained micro-artefacts.
type IndexedFileArtefactView = enrichment.IndexedFileArtefactView

// MicroArtefactNode is one micro-artefact detected inside a file node.
type MicroArtefactNode = enrichment.FileLevelArtefactNode

// ReadHierarchy walks the graphene store and returns the full
// evidence_file → partition → indexed_file → micro_artefact tree.
func (r *Repository) ReadHierarchy() (*EvidenceFileHierarchy, error) {
	tree := &EvidenceFileHierarchy{}

	kinds := r.kinds()

	// Match every encoding this store uses. On a store written by graphene
	// v0.1.3 the current encoding matches nothing, and a single-type lookup
	// would return an empty tree without raising an error.
	evidenceFileIDs, err := r.graph.NodesByAnyType(kinds.MatchTypes(schema.KindEvidenceFile))
	if err != nil {
		return nil, err
	}

	contEdges := []graphstore.EdgeType{graphstore.EdgeTypeContains}

	for _, evidenceID := range evidenceFileIDs {
		evidenceNode, err := r.graph.GetNode(evidenceID)
		if err != nil {
			return nil, err
		}
		dProps := unpackProps(evidenceNode.Properties)
		evidenceFile := &EvidenceFileNode{
			ID:   strProp(dProps, "evidence_file_id"),
			Name: strProp(dProps, "name"),
		}

		partNeighbours, err := r.graph.Neighbours(evidenceID, graphstore.DirectionOutbound, contEdges)
		if err != nil {
			return nil, err
		}

		for _, pn := range partNeighbours {
			if !kinds.Is(pn.Node, schema.KindPartition) {
				continue
			}
			pProps := unpackProps(pn.Node.Properties)
			partition := &PartitionFileNode{
				ID:   strProp(pProps, "partition_id"),
				Name: strProp(pProps, "name"),
			}

			fileNeighbours, err := r.graph.Neighbours(pn.Node.ID, graphstore.DirectionOutbound, contEdges)
			if err != nil {
				return nil, err
			}

			for _, fn := range fileNeighbours {
				if !kinds.Is(fn.Node, schema.KindIndexedFile) {
					continue
				}
				fProps := unpackProps(fn.Node.Properties)
				ifile := &IndexedFileArtefactView{
					IndexedFileProjection: enrichment.IndexedFileProjection{
						ID:           strProp(fProps, "indexed_file_id"),
						Name:         strProp(fProps, "name"),
						Path:         strProp(fProps, "path"),
						Size:         int64Prop(fProps, "size"),
						Hash:         strProp(fProps, "hash"),
						IsDeleted:    boolProp(fProps, "is_deleted"),
						IsFragmented: boolProp(fProps, "is_fragmented"),
						FileType:     strProp(fProps, "file_type"),
						Entropy:      float64Prop(fProps, "entropy"),
						HasEntropy:   boolProp(fProps, "has_entropy"),
					},
				}

				artNeighbours, err := r.graph.Neighbours(fn.Node.ID, graphstore.DirectionOutbound, contEdges)
				if err != nil {
					return nil, err
				}

				for _, an := range artNeighbours {
					if !an.Node.HasLabel(graphstore.NodeTypeMicroArtefact) {
						continue
					}
					aProps := unpackProps(an.Node.Properties)
					ifile.Artefacts = append(ifile.Artefacts, &MicroArtefactNode{
						MicroArtefactNodeFields: model.MicroArtefactNodeFields{
							Kind:         strProp(aProps, "kind"),
							Detector:     strProp(aProps, "detector"),
							Value:        strProp(aProps, "value"),
							Summary:      strProp(aProps, "summary"),
							Confidence:   float32Prop(aProps, "confidence"),
							Start:        int64Prop(aProps, "start"),
							End:          int64Prop(aProps, "end"),
							ObservedTime: observedTime(aProps),
							Attributes:   stringMapProp(aProps, "attributes"),
						},
					})
					tree.TotalArtefacts++
				}

				sort.Slice(ifile.Artefacts, func(i, j int) bool {
					if ifile.Artefacts[i].Kind != ifile.Artefacts[j].Kind {
						return ifile.Artefacts[i].Kind < ifile.Artefacts[j].Kind
					}
					return ifile.Artefacts[i].Start < ifile.Artefacts[j].Start
				})

				partition.Files = append(partition.Files, ifile)
			}

			sort.Slice(partition.Files, func(i, j int) bool {
				return partition.Files[i].Name < partition.Files[j].Name
			})

			evidenceFile.Partitions = append(evidenceFile.Partitions, partition)
		}

		sort.Slice(evidenceFile.Partitions, func(i, j int) bool {
			return evidenceFile.Partitions[i].Name < evidenceFile.Partitions[j].Name
		})

		tree.EvidenceFiles = append(tree.EvidenceFiles, evidenceFile)
	}

	sort.Slice(tree.EvidenceFiles, func(i, j int) bool {
		return tree.EvidenceFiles[i].Name < tree.EvidenceFiles[j].Name
	})
	relationEdgeIDs, err := r.graph.EdgesByAnyType(kinds.MatchEdgeTypes(schema.EdgeKindRelation))
	if err != nil {
		return nil, err
	}
	if len(relationEdgeIDs) > 0 {
		// Unresolvable IDs come back in the second return rather than as an
		// error; an edge deleted between the type lookup and the fetch is
		// simply not counted.
		relationEdges, _, err := r.graph.GetEdges(relationEdgeIDs)
		if err != nil {
			return nil, err
		}
		for _, edge := range relationEdges {
			if edge == nil {
				continue
			}
			tree.TotalRelations++
			edgeProps := unpackProps(edge.Properties)
			if boolProp(edgeProps, "deterministic_flag") {
				tree.DeterministicRelations++
			} else {
				tree.ProbabilisticRelations++
			}
		}
	}

	return tree, nil
}

// ─── property helpers ────────────────────────────────────────────────────────
//
// These delegate to lib/graph/props, which is the union of the three helper
// sets DUES used to carry. See that package's doc for where they disagreed.

func unpackProps(data []byte) props.Map { return props.Unpack(data) }

// observedTime reads an artefact's own timestamp, returning nil when it has
// none. Most artefacts do not: only detectors that parse a structured record
// with a time in it — EVTX, PE headers — can supply one.
func observedTime(m props.Map) *time.Time {
	if !m.Has("observed_time") {
		return nil
	}
	at := m.Time("observed_time")
	if at.IsZero() {
		return nil
	}
	return &at
}

// stringMapProp decodes a detector's attribute map. msgpack hands back
// map[string]any, so the values are converted element-wise.
func stringMapProp(m props.Map, key string) map[string]string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	switch typed := raw.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		if len(typed) == 0 {
			return nil
		}
		out := make(map[string]string, len(typed))
		for k, v := range typed {
			switch s := v.(type) {
			case string:
				out[k] = s
			case []byte:
				out[k] = string(s)
			}
		}
		return out
	default:
		return nil
	}
}

func strProp(m props.Map, key string) string      { return m.Str(key) }
func int64Prop(m props.Map, key string) int64     { return m.Int64(key) }
func float32Prop(m props.Map, key string) float32 { return m.Float32(key) }
func float64Prop(m props.Map, key string) float64 { return m.Float64(key) }
func boolProp(m props.Map, key string) bool       { return m.Bool(key) }
