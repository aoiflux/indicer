package graphene

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"indicer/lib/util"
	"sort"
	"strconv"

	"indicer/lib/microartefact/model"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
	graphviz "github.com/aoiflux/graphene/viz"
	"github.com/vmihailenco/msgpack/v5"
)

const StoreDirName = "graph"

var (
	nodeTypeEvidenceFile = graphstore.CustomNodeType(0)
	nodeTypePartition    = graphstore.CustomNodeType(1)
	nodeTypeIndexedFile  = graphstore.CustomNodeType(2)
	edgeTypeRelation     = graphstore.EdgeType(100)
)

type Repository struct {
	graph *graphenedb.Graph
}

func Open(root string) (*Repository, error) {
	graph, err := graphenedb.Open(util.GraphPath(root))
	if err != nil {
		return nil, err
	}
	return &Repository{graph: graph}, nil
}

func (r *Repository) Close() error {
	if r == nil || r.graph == nil {
		return nil
	}
	return r.graph.Close()
}

func (r *Repository) Store(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation) error {
	if r == nil || r.graph == nil {
		return nil
	}

	if err := validateStoreHierarchy(file); err != nil {
		return err
	}

	indexedFileID := resolveIndexedFileID(file)
	indexedFileNode, exists, err := r.findIndexedFileNode(indexedFileID)
	if err != nil {
		return err
	}

	if !exists {
		return fmt.Errorf("micro-artefact store: indexed_file_id=%s not found in graphdb; run dues enrich to backfill file hierarchy first", indexedFileID)
	}

	if len(artefacts) == 0 {
		return nil
	}

	if exists {
		hasArtefacts, err := r.hasFileArtefacts(indexedFileNode)
		if err != nil {
			return err
		}
		if hasArtefacts {
			return nil
		}
	}

	artefactNodesByKey, err := r.storeArtefactNodes(indexedFileNode, file.Hash, artefacts)
	if err != nil {
		return err
	}

	if err := r.storeRelationEdges(artefactNodesByKey, relations); err != nil {
		return err
	}

	return nil
}

func validateStoreHierarchy(file model.FileRecord) error {
	if file.EvidenceFileID == "" {
		return fmt.Errorf("micro-artefact store: EvidenceFileID is required (micro-artefacts must belong to an evidence file → partition → indexed file hierarchy)")
	}
	if file.PartitionID == "" {
		return fmt.Errorf("micro-artefact store: PartitionID is required (micro-artefacts must belong to an evidence file → partition → indexed file hierarchy)")
	}
	return nil
}

func resolveIndexedFileID(file model.FileRecord) string {
	if file.IndexedFileID != "" {
		return file.IndexedFileID
	}
	return model.BuildIndexedFileID(file.PartitionID, file.Path, file.Hash)
}

func (r *Repository) hasIndexedFile(indexedFileID string) (bool, error) {
	_, exists, err := r.findIndexedFileNode(indexedFileID)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (r *Repository) findIndexedFileNode(indexedFileID string) (graphstore.NodeID, bool, error) {
	hits, err := r.graph.NodesByProperty("indexed_file_id", []byte(indexedFileID))
	if err != nil {
		return 0, false, err
	}
	if len(hits) == 0 {
		return 0, false, nil
	}
	return hits[0], true, nil
}

func (r *Repository) hasFileArtefacts(fileNodeID graphstore.NodeID) (bool, error) {
	neighbours, err := r.graph.Neighbours(fileNodeID, graphstore.DirectionOutbound, []graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		return false, err
	}
	for _, neighbour := range neighbours {
		if neighbour.Node != nil && neighbour.Node.HasLabel(graphstore.NodeTypeMicroArtefact) {
			return true, nil
		}
	}
	return false, nil
}

func (r *Repository) storeArtefactNodes(indexedFileNode graphstore.NodeID, evidenceHash string, artefacts []model.Artefact) (map[string][]graphstore.NodeID, error) {
	artefactNodesByKey := make(map[string][]graphstore.NodeID, len(artefacts))

	for _, artefact := range artefacts {
		artefactNode, err := r.createArtefactNode(evidenceHash, artefact)
		if err != nil {
			return nil, err
		}

		lookupKey := relationNodeLookupKey(artefact.Kind, artefact.Value)
		artefactNodesByKey[lookupKey] = append(artefactNodesByKey[lookupKey], artefactNode)

		if err := r.createContainsEdge(indexedFileNode, artefactNode, artefact); err != nil {
			return nil, err
		}
		if err := r.ensureMicroArtefactHasExactlyOneFileParent(indexedFileNode, artefactNode); err != nil {
			return nil, err
		}
	}

	return artefactNodesByKey, nil
}

func (r *Repository) createArtefactNode(evidenceHash string, artefact model.Artefact) (graphstore.NodeID, error) {
	nodeID, err := r.graph.AddNode(&graphstore.Node{
		Labels: []graphstore.NodeType{graphstore.NodeTypeMicroArtefact},
		Properties: mustPack(map[string]any{
			"kind":          artefact.Kind,
			"detector":      artefact.Detector,
			"value":         artefact.Value,
			"summary":       artefact.Summary,
			"confidence":    artefact.Confidence,
			"start":         artefact.Span.Start,
			"end":           artefact.Span.End,
			"attributes":    artefact.Attributes,
			"evidence_hash": evidenceHash,
		}),
	})
	if err != nil {
		return 0, err
	}

	index := map[string][]byte{
		"artefact_kind":       []byte(artefact.Kind),
		"artefact_detector":   []byte(artefact.Detector),
		"artefact_value_hash": []byte(hashText(artefact.Value)),
		"evidence_hash":       []byte(evidenceHash),
	}
	if artefact.Value != "" && len(artefact.Value) <= 256 {
		index["artefact_value"] = []byte(artefact.Value)
	}
	if err := r.graph.IndexNodeProperties(nodeID, index); err != nil {
		return 0, err
	}

	return nodeID, nil
}

func (r *Repository) createContainsEdge(indexedFileNode, artefactNode graphstore.NodeID, artefact model.Artefact) error {
	edgeID, err := r.graph.AddEdge(&graphstore.Edge{
		Src:    indexedFileNode,
		Dst:    artefactNode,
		Labels: []graphstore.EdgeType{graphstore.EdgeTypeContains},
		Properties: mustPack(map[string]any{
			"detector": artefact.Detector,
			"start":    artefact.Span.Start,
			"end":      artefact.Span.End,
		}),
	})
	if err != nil {
		return err
	}

	return r.graph.IndexEdgeProperties(edgeID, map[string][]byte{
		"contains_kind": []byte(artefact.Kind),
		"start":         []byte(strconv.FormatInt(artefact.Span.Start, 10)),
		"end":           []byte(strconv.FormatInt(artefact.Span.End, 10)),
	})
}

func (r *Repository) ExportInteractiveHTML(outPath string) error {
	return r.ExportInteractiveHTMLTopK(outPath, 0)
}

func (r *Repository) HasIndexedFile(indexedFileID string) (bool, error) {
	if r == nil || r.graph == nil {
		return false, fmt.Errorf("micro-artefact repository is not open")
	}
	hits, err := r.graph.NodesByProperty("indexed_file_id", []byte(indexedFileID))
	if err != nil {
		return false, err
	}
	return len(hits) > 0, nil
}

func (r *Repository) HasMicroArtefacts(indexedFileID string) (bool, error) {
	if r == nil || r.graph == nil {
		return false, fmt.Errorf("micro-artefact repository is not open")
	}
	nodeID, exists, err := r.findIndexedFileNode(indexedFileID)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	return r.hasFileArtefacts(nodeID)
}

func (r *Repository) ExportInteractiveHTMLTopK(outPath string, topK int) error {
	if r == nil || r.graph == nil {
		return fmt.Errorf("micro-artefact export: repository is not open")
	}

	nodesByID := make(map[graphstore.NodeID]*graphstore.Node)

	addNode := func(id graphstore.NodeID) error {
		if _, exists := nodesByID[id]; exists {
			return nil
		}
		node, err := r.graph.GetNode(id)
		if err != nil {
			return err
		}
		nodesByID[id] = node
		return nil
	}

	indexedIDs, err := r.graph.NodesByType(nodeTypeIndexedFile)
	if err != nil {
		return err
	}
	type rankedIndexed struct {
		id            graphstore.NodeID
		artefactCount int
	}
	ranked := make([]rankedIndexed, 0, len(indexedIDs))
	for _, id := range indexedIDs {
		node, err := r.graph.GetNode(id)
		if err != nil {
			return err
		}
		props := unpackNodeProps(node.Properties)
		ranked = append(ranked, rankedIndexed{
			id:            id,
			artefactCount: intProp(props, "artefact_count"),
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].artefactCount != ranked[j].artefactCount {
			return ranked[i].artefactCount > ranked[j].artefactCount
		}
		return ranked[i].id < ranked[j].id
	})
	if topK > 0 && topK < len(ranked) {
		ranked = ranked[:topK]
	}

	containsOnly := []graphstore.EdgeType{graphstore.EdgeTypeContains}
	for _, item := range ranked {
		if err := addNode(item.id); err != nil {
			return err
		}

		parents, err := r.graph.Neighbours(item.id, graphstore.DirectionInbound, containsOnly)
		if err != nil {
			return err
		}
		for _, parent := range parents {
			if parent.Node == nil {
				continue
			}
			if err := addNode(parent.Node.ID); err != nil {
				return err
			}
			grandParents, err := r.graph.Neighbours(parent.Node.ID, graphstore.DirectionInbound, containsOnly)
			if err != nil {
				return err
			}
			for _, grandParent := range grandParents {
				if grandParent.Node == nil {
					continue
				}
				if err := addNode(grandParent.Node.ID); err != nil {
					return err
				}
			}
		}

		artefacts, err := r.graph.Neighbours(item.id, graphstore.DirectionOutbound, containsOnly)
		if err != nil {
			return err
		}
		for _, artefact := range artefacts {
			if artefact.Node == nil {
				continue
			}
			if err := addNode(artefact.Node.ID); err != nil {
				return err
			}
		}
	}

	containsEdgeIDs, err := r.graph.EdgesByType(graphstore.EdgeTypeContains)
	if err != nil {
		return err
	}
	containsEdges, err := r.graph.GetEdges(containsEdgeIDs)
	if err != nil {
		return err
	}

	relationEdgeIDs, err := r.graph.EdgesByType(edgeTypeRelation)
	if err != nil {
		return err
	}
	relationEdges, err := r.graph.GetEdges(relationEdgeIDs)
	if err != nil {
		return err
	}

	nodes := make([]*graphstore.Node, 0, len(nodesByID))
	for _, node := range nodesByID {
		nodes = append(nodes, node)
	}

	edges := make([]*graphstore.Edge, 0, len(containsEdges)+len(relationEdges))
	for _, edge := range containsEdges {
		if edge == nil {
			continue
		}
		if _, ok := nodesByID[edge.Src]; !ok {
			continue
		}
		if _, ok := nodesByID[edge.Dst]; !ok {
			continue
		}
		edges = append(edges, edge)
	}
	for _, edge := range relationEdges {
		if edge == nil {
			continue
		}
		if _, ok := nodesByID[edge.Src]; !ok {
			continue
		}
		if _, ok := nodesByID[edge.Dst]; !ok {
			continue
		}
		edges = append(edges, edge)
	}

	return graphviz.ExportInteractiveHTMLWithOptions(nodes, edges, outPath, graphviz.ExportOptions{
		Title:    "DUES Micro-Artefact Graph",
		Subtitle: fmt.Sprintf("Lazy graph view over top-%d indexed files (by artefact count)", len(ranked)),
	})
}

func unpackNodeProps(data []byte) map[string]any {
	if len(data) == 0 {
		return nil
	}
	var m map[string]any
	if err := msgpack.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

func intProp(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case uint8:
		return int(n)
	case uint16:
		return int(n)
	case uint32:
		return int(n)
	case uint64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func (r *Repository) ensureMicroArtefactHasExactlyOneFileParent(fileNodeID, artefactNodeID graphstore.NodeID) error {
	parents, err := r.graph.Neighbours(artefactNodeID, graphstore.DirectionInbound, []graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		return err
	}

	fileParentCount := 0
	for _, parent := range parents {
		if parent.Node == nil || !parent.Node.HasLabel(nodeTypeIndexedFile) {
			continue
		}
		fileParentCount++
		if parent.Node.ID != fileNodeID {
			return fmt.Errorf("micro-artefact node %d is linked to unexpected file node %d (expected %d)", artefactNodeID, parent.Node.ID, fileNodeID)
		}
	}

	if fileParentCount != 1 {
		return fmt.Errorf("micro-artefact node %d must have exactly one file parent edge; found %d", artefactNodeID, fileParentCount)
	}

	return nil
}

func mustPack(value any) []byte {
	packed, err := msgpack.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("pack microartefact graph properties: %w", err))
	}
	return packed
}

func hashText(value string, rest ...string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(value))
	for _, next := range rest {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(next))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func relationNodeLookupKey(kind, value string) string {
	return kind + "\x00" + value
}

func (r *Repository) storeRelationEdges(artefactNodesByKey map[string][]graphstore.NodeID, relations []model.Relation) error {
	for _, relation := range relations {
		fromNodes := artefactNodesByKey[relationNodeLookupKey(relation.FromKind, relation.FromValue)]
		toNodes := artefactNodesByKey[relationNodeLookupKey(relation.ToKind, relation.ToValue)]
		if len(fromNodes) == 0 || len(toNodes) == 0 {
			continue
		}

		for _, fromID := range fromNodes {
			for _, toID := range toNodes {
				if fromID == toID {
					continue
				}

				exists, err := r.graph.EdgeExists(fromID, toID, []graphstore.EdgeType{edgeTypeRelation})
				if err != nil {
					return err
				}
				if exists {
					continue
				}

				evidence := make([]map[string]string, 0, len(relation.Evidence))
				for _, field := range relation.Evidence {
					evidence = append(evidence, map[string]string{
						"field_name": field.FieldName,
						"parser":     field.Parser,
						"raw_value":  field.RawValue,
					})
				}

				edgeID, err := r.graph.AddEdge(&graphstore.Edge{
					Src:    fromID,
					Dst:    toID,
					Labels: []graphstore.EdgeType{edgeTypeRelation},
					Properties: mustPack(map[string]any{
						"relation_type":      relation.RelationType,
						"method":             relation.Method,
						"deterministic_flag": relation.Deterministic,
						"confidence":         relation.Confidence,
						"evidence_fields":    evidence,
					}),
				})
				if err != nil {
					return err
				}

				if err := r.graph.IndexEdgeProperties(edgeID, map[string][]byte{
					"relation_type":      []byte(relation.RelationType),
					"relation_method":    []byte(relation.Method),
					"deterministic_flag": []byte(strconv.FormatBool(relation.Deterministic)),
				}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
