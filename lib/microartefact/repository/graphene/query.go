package graphene

import (
	"context"
	"fmt"
	"sort"

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
		dProps, err := unpackProps(evidenceNode)
		if err != nil {
			return nil, err
		}
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
			pProps, err := unpackProps(pn.Node)
			if err != nil {
				return nil, err
			}
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
				ifile, err := r.indexedFile(fn.Node)
				if err != nil {
					return nil, err
				}
				ifile.ArtefactCount = len(ifile.Artefacts)
				tree.TotalArtefacts += ifile.ArtefactCount

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
			edgeProps, err := props.Unpack(edge.Properties)
			if err != nil {
				return nil, fmt.Errorf("graph edge %d: %w", edge.ID, err)
			}
			if boolProp(edgeProps, "deterministic_flag") {
				tree.DeterministicRelations++
			} else {
				tree.ProbabilisticRelations++
			}
		}
	}

	return tree, nil
}

// indexedFileView is an indexed file node's projection, without artefacts.
func indexedFileView(fProps props.Map) *IndexedFileArtefactView {
	return &IndexedFileArtefactView{
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
}

// indexedFile reads an indexed file node with the micro-artefacts it
// contains, ordered by kind, then offset. Enrichment keeps a file's name and
// path on the file node the indexed node contains, not on the indexed node
// itself; they are taken from there.
func (r *Repository) indexedFile(node *graphstore.Node) (*IndexedFileArtefactView, error) {
	fProps, err := unpackProps(node)
	if err != nil {
		return nil, err
	}
	view := indexedFileView(fProps)
	neighbours, err := r.graph.Neighbours(node.ID, graphstore.DirectionOutbound, []graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		return nil, err
	}
	var out []*MicroArtefactNode
	for _, an := range neighbours {
		if an.Node == nil {
			continue
		}
		if r.kinds().Is(an.Node, schema.KindFile) {
			if view.Name != "" && view.Path != "" {
				continue
			}
			nProps, err := unpackProps(an.Node)
			if err != nil {
				return nil, err
			}
			if view.Name == "" {
				view.Name = strProp(nProps, "name")
			}
			if view.Path == "" {
				view.Path = strProp(nProps, "path")
			}
			continue
		}
		if !an.Node.HasLabel(graphstore.NodeTypeMicroArtefact) {
			continue
		}
		aProps, err := unpackProps(an.Node)
		if err != nil {
			return nil, err
		}
		out = append(out, &MicroArtefactNode{
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
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Start < out[j].Start
	})
	view.Artefacts = out
	return view, nil
}

// TopFiles is a bounded view of a store's micro-artefacts.
type TopFiles struct {
	// Tree holds the files shown, most artefacts first, under their
	// partitions and evidence.
	Tree *EvidenceFileHierarchy
	// Files and Artefacts count every file with artefacts, and the
	// artefacts they hold, shown or not; Shown is how many files Tree holds.
	Files, Artefacts, Shown int
}

// TopFiles reads the limit files with the most micro-artefacts, at most
// perFile artefacts of each, under their partitions and evidence.
// ReadHierarchy reads every indexed file, with artefacts or without, and
// every relation edge, which a screen cannot show (TUI-5).
func (r *Repository) TopFiles(ctx context.Context, limit, perFile int) (*TopFiles, error) {
	ranked, files, artefacts, err := r.rankArtefactFiles(ctx, limit)
	if err != nil {
		return nil, err
	}
	kinds := r.kinds()
	contains := []graphstore.EdgeType{graphstore.EdgeTypeContains}
	tree := &EvidenceFileHierarchy{TotalArtefacts: artefacts}
	evidence := map[graphstore.NodeID]*EvidenceFileNode{}
	partitions := map[graphstore.NodeID]*PartitionFileNode{}

	// parentOf finds the node of the given kind holding child, if any.
	parentOf := func(child graphstore.NodeID, kind schema.NodeKind) (*graphstore.Node, error) {
		parents, err := r.graph.Neighbours(child, graphstore.DirectionInbound, contains)
		if err != nil {
			return nil, err
		}
		for _, p := range parents {
			if p.Node != nil && kinds.Is(p.Node, kind) {
				return p.Node, nil
			}
		}
		return nil, nil
	}
	// evidenceOf is the tree's node for an evidence node; nil stands for a
	// partition no evidence holds.
	evidenceOf := func(node *graphstore.Node) (*EvidenceFileNode, error) {
		var id graphstore.NodeID
		if node != nil {
			id = node.ID
		}
		if ev, ok := evidence[id]; ok {
			return ev, nil
		}
		ev := &EvidenceFileNode{Name: "(no evidence)"}
		if node != nil {
			eProps, err := unpackProps(node)
			if err != nil {
				return nil, err
			}
			ev = &EvidenceFileNode{ID: strProp(eProps, "evidence_file_id"), Name: strProp(eProps, "name")}
		}
		evidence[id] = ev
		tree.EvidenceFiles = append(tree.EvidenceFiles, ev)
		return ev, nil
	}
	// partitionOf is the tree's node for the partition holding a file.
	partitionOf := func(file graphstore.NodeID) (*PartitionFileNode, error) {
		pnode, err := parentOf(file, schema.KindPartition)
		if err != nil {
			return nil, err
		}
		var id graphstore.NodeID
		if pnode != nil {
			id = pnode.ID
		}
		if part, ok := partitions[id]; ok {
			return part, nil
		}
		part := &PartitionFileNode{Name: "(no partition)"}
		var enode *graphstore.Node
		if pnode != nil {
			pProps, err := unpackProps(pnode)
			if err != nil {
				return nil, err
			}
			part = &PartitionFileNode{ID: strProp(pProps, "partition_id"), Name: strProp(pProps, "name")}
			if enode, err = parentOf(pnode.ID, schema.KindEvidenceFile); err != nil {
				return nil, err
			}
		}
		ev, err := evidenceOf(enode)
		if err != nil {
			return nil, err
		}
		partitions[id] = part
		ev.Partitions = append(ev.Partitions, part)
		return part, nil
	}

	for _, rf := range ranked {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, err := r.graph.GetNode(rf.id)
		if err != nil {
			return nil, err
		}
		file, err := r.indexedFile(node)
		if err != nil {
			return nil, err
		}
		file.ArtefactCount = rf.count
		if perFile > 0 && len(file.Artefacts) > perFile {
			file.Artefacts = file.Artefacts[:perFile]
		}
		part, err := partitionOf(rf.id)
		if err != nil {
			return nil, err
		}
		part.Files = append(part.Files, file)
	}
	sort.SliceStable(tree.EvidenceFiles, func(i, j int) bool { return tree.EvidenceFiles[i].Name < tree.EvidenceFiles[j].Name })
	for _, ev := range tree.EvidenceFiles {
		sort.SliceStable(ev.Partitions, func(i, j int) bool { return ev.Partitions[i].Name < ev.Partitions[j].Name })
	}
	return &TopFiles{Tree: tree, Files: files, Artefacts: artefacts, Shown: len(ranked)}, nil
}

// ─── property helpers ────────────────────────────────────────────────────────
//
// These delegate to lib/graph/props, which is the union of the three helper
// sets DUES used to carry. See that package's doc for where they disagreed.

// unpackProps decodes a node's properties, naming the node when they do not
// decode (GRA-10).
func unpackProps(node *graphstore.Node) (props.Map, error) {
	fields, err := props.Unpack(node.Properties)
	if err != nil {
		return nil, fmt.Errorf("graph node %d: %w", node.ID, err)
	}
	return fields, nil
}

func strProp(m props.Map, key string) string      { return m.Str(key) }
func int64Prop(m props.Map, key string) int64     { return m.Int64(key) }
func float32Prop(m props.Map, key string) float32 { return m.Float32(key) }
func float64Prop(m props.Map, key string) float64 { return m.Float64(key) }
func boolProp(m props.Map, key string) bool       { return m.Bool(key) }
