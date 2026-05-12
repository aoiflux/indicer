package graphene

import (
	"sort"

	"indicer/lib/microartefact/model"

	graphstore "github.com/aoiflux/graphene/store"
	"github.com/vmihailenco/msgpack/v5"
)

// HierarchyTree is the top-level result of ReadHierarchy. It reflects the
// enforced hierarchy: disk_image → partition → indexed_file → micro_artefact.
type HierarchyTree struct {
	DiskImages             []*DiskImageNode
	TotalArtefacts         int
	TotalRelations         int
	DeterministicRelations int
	ProbabilisticRelations int
}

// DiskImageNode represents one evidence image (disk image) in the graph.
type DiskImageNode struct {
	ID         string
	Name       string
	Partitions []*PartitionNode
}

// PartitionNode represents one partition within a disk image.
type PartitionNode struct {
	ID    string
	Name  string
	Files []*FileNode
}

// NodeIdentity contains fields shared by graph node projections.
type NodeIdentity struct {
	ID string
}

// FileNode represents one indexed file within a partition.
type FileNode struct {
	NodeIdentity
	model.FileNodeFields
	Artefacts []*MicroArtefactNode
}

// MicroArtefactNode is one micro-artefact detected inside a file node.
type MicroArtefactNode struct {
	model.MicroArtefactNodeFields
}

// Backward-compatible aliases for older callers.
type IndexedFileNode = FileNode
type ArtefactNode = MicroArtefactNode

// ReadHierarchy walks the graphene store and returns the full
// disk_image → partition → indexed_file → micro_artefact tree.
func (r *Repository) ReadHierarchy() (*HierarchyTree, error) {
	tree := &HierarchyTree{}

	diskImageIDs, err := r.graph.NodesByType(nodeTypeDiskImage)
	if err != nil {
		return nil, err
	}

	contEdges := []graphstore.EdgeType{graphstore.EdgeTypeContains}

	for _, diskID := range diskImageIDs {
		diskNode, err := r.graph.GetNode(diskID)
		if err != nil {
			return nil, err
		}
		dProps := unpackProps(diskNode.Properties)
		di := &DiskImageNode{
			ID:   strProp(dProps, "disk_image_id"),
			Name: strProp(dProps, "name"),
		}

		partNeighbours, err := r.graph.Neighbours(diskID, graphstore.DirectionOutbound, contEdges)
		if err != nil {
			return nil, err
		}

		for _, pn := range partNeighbours {
			if !pn.Node.HasLabel(nodeTypePartition) {
				continue
			}
			pProps := unpackProps(pn.Node.Properties)
			partition := &PartitionNode{
				ID:   strProp(pProps, "partition_id"),
				Name: strProp(pProps, "name"),
			}

			fileNeighbours, err := r.graph.Neighbours(pn.Node.ID, graphstore.DirectionOutbound, contEdges)
			if err != nil {
				return nil, err
			}

			for _, fn := range fileNeighbours {
				if !fn.Node.HasLabel(nodeTypeIndexedFile) {
					continue
				}
				fProps := unpackProps(fn.Node.Properties)
				ifile := &FileNode{
					NodeIdentity: modelNodeID(strProp(fProps, "indexed_file_id")),
					FileNodeFields: model.FileNodeFields{
						Name:         strProp(fProps, "name"),
						Path:         strProp(fProps, "path"),
						Size:         int64Prop(fProps, "size"),
						Hash:         strProp(fProps, "hash"),
						IsDeleted:    boolProp(fProps, "is_deleted"),
						IsFragmented: boolProp(fProps, "is_fragmented"),
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
							Kind:       strProp(aProps, "kind"),
							Detector:   strProp(aProps, "detector"),
							Value:      strProp(aProps, "value"),
							Summary:    strProp(aProps, "summary"),
							Confidence: float32Prop(aProps, "confidence"),
							Start:      int64Prop(aProps, "start"),
							End:        int64Prop(aProps, "end"),
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

			di.Partitions = append(di.Partitions, partition)
		}

		sort.Slice(di.Partitions, func(i, j int) bool {
			return di.Partitions[i].Name < di.Partitions[j].Name
		})

		tree.DiskImages = append(tree.DiskImages, di)
	}

	sort.Slice(tree.DiskImages, func(i, j int) bool {
		return tree.DiskImages[i].Name < tree.DiskImages[j].Name
	})

	relationEdgeIDs, err := r.graph.EdgesByType(edgeTypeRelation)
	if err != nil {
		return nil, err
	}
	if len(relationEdgeIDs) > 0 {
		relationEdges, err := r.graph.GetEdges(relationEdgeIDs)
		if err != nil {
			return nil, err
		}
		for _, edge := range relationEdges {
			if edge == nil {
				continue
			}
			tree.TotalRelations++
			props := unpackProps(edge.Properties)
			if boolProp(props, "deterministic_flag") {
				tree.DeterministicRelations++
			} else {
				tree.ProbabilisticRelations++
			}
		}
	}

	return tree, nil
}

func modelNodeID(id string) NodeIdentity {
	return NodeIdentity{ID: id}
}

// ─── property helpers ────────────────────────────────────────────────────────

func unpackProps(data []byte) map[string]any {
	if len(data) == 0 {
		return nil
	}
	var m map[string]any
	_ = msgpack.Unmarshal(data, &m)
	return m
}

func strProp(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func int64Prop(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int64:
			return n
		case int8:
			return int64(n)
		case int16:
			return int64(n)
		case int32:
			return int64(n)
		case uint64:
			return int64(n)
		case float64:
			return int64(n)
		}
	}
	return 0
}

func float32Prop(m map[string]any, key string) float32 {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case float32:
			return n
		case float64:
			return float32(n)
		}
	}
	return 0
}

func boolProp(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		switch b := v.(type) {
		case bool:
			return b
		case string:
			return b == "true"
		}
	}
	return false
}
