package graphene

import (
	"fmt"
	"sort"
	"strconv"

	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/ids"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/microartefact/model"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
	graphviz "github.com/aoiflux/graphene/viz"
)

const StoreDirName = "graph"

// kinds resolves node and edge kinds to the raw type values this particular
// store uses.
//
// This replaces package-level vars derived from graphstore.CustomNodeType and a
// bare graphstore.EdgeType(100). Both were values captured from whichever
// graphene happened to be linked; when the custom band moved from 128 to 32768
// they stopped matching everything already written, silently. Relation edges
// are the awkward case — 100 was a raw literal below the old custom base, so
// its legacy encoding is not base+offset and the registry special-cases it.
func (r *Repository) kinds() *schema.Registry {
	return r.handle.Schema()
}

// Repository holds a reference to the process-wide graph handle. The cached
// graph pointer stays valid for as long as that reference is outstanding.
type Repository struct {
	handle *duesgraph.Handle
	graph  *graphenedb.Graph
	owns   bool

	// actor attributes every write. The zero value writes unattributed.
	actor duesgraph.ActorRef
}

// Open acquires a reference to root's graph store. Close releases it.
func Open(root string) (*Repository, error) {
	handle, err := duesgraph.Open(root)
	if err != nil {
		return nil, err
	}
	return &Repository{handle: handle, graph: handle.Graph(), owns: true}, nil
}

// OpenOn borrows a handle a caller already holds. Close is a no-op on the
// result: the reference belongs to whoever opened it.
func OpenOn(handle *duesgraph.Handle) *Repository {
	if handle == nil {
		return nil
	}
	return &Repository{handle: handle, graph: handle.Graph()}
}

// Close releases this repository's reference. The store itself closes when the
// last reference in the process goes.
func (r *Repository) Close() error {
	if r == nil || !r.owns {
		return nil
	}
	r.owns = false
	r.graph = nil
	handle := r.handle
	r.handle = nil
	return handle.Release()
}

func (r *Repository) Store(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation) error {
	if r == nil || r.graph == nil {
		return nil
	}
	writer, err := r.handle.Writer(r.actor)
	if err != nil {
		return err
	}
	defer writer.Close()

	batch := writer.Batch()
	if err := r.store(batch, file, artefacts, relations); err != nil {
		batch.Rollback()
		return err
	}
	return batch.Commit()
}

// SetActor records who subsequent writes are attributed to.
func (r *Repository) SetActor(actor duesgraph.ActorRef) {
	if r != nil {
		r.actor = actor
	}
}

func (r *Repository) store(batch *duesgraph.Batch, file model.FileRecord, artefacts []model.Artefact, relations []model.Relation) error {
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

	artefactNodesByKey, err := r.storeArtefactNodes(batch, indexedFileNode, file.Hash, artefacts)
	if err != nil {
		return err
	}

	if err := r.storeRelationEdges(batch, artefactNodesByKey, relations); err != nil {
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

func (r *Repository) storeArtefactNodes(batch *duesgraph.Batch, indexedFileNode graphstore.NodeID, evidenceHash string, artefacts []model.Artefact) (map[string][]graphstore.NodeID, error) {
	artefactNodesByKey := make(map[string][]graphstore.NodeID, len(artefacts))
	parents := make(map[graphstore.NodeID]int, len(artefacts))

	for _, artefact := range artefacts {
		artefactNode, err := r.createArtefactNode(batch, evidenceHash, artefact)
		if err != nil {
			return nil, err
		}

		lookupKey := relationNodeLookupKey(artefact.Kind, artefact.Value)
		artefactNodesByKey[lookupKey] = append(artefactNodesByKey[lookupKey], artefactNode)

		if err := r.createContainsEdge(batch, indexedFileNode, artefactNode, artefact); err != nil {
			return nil, err
		}
		parents[artefactNode]++
	}

	// The one-file-parent invariant is now checked against the batch rather
	// than by re-querying the store after every insert.
	//
	// That re-query cannot work inside a transaction — the containment edge is
	// not visible to Neighbours until the commit, so every artefact would look
	// parentless — and it was O(artefacts) round trips on the ingest path.
	// Every artefact node here is created by this batch, so the batch's own
	// record of what it linked is the authoritative answer.
	for node, count := range parents {
		if count != 1 {
			return nil, fmt.Errorf(
				"micro-artefact node %d must have exactly one file parent edge; this batch created %d",
				node, count)
		}
	}

	return artefactNodesByKey, nil
}

func (r *Repository) createArtefactNode(batch *duesgraph.Batch, evidenceHash string, artefact model.Artefact) (graphstore.NodeID, error) {
	index := map[string][]byte{
		"artefact_kind":       []byte(artefact.Kind),
		"artefact_detector":   []byte(artefact.Detector),
		"artefact_value_hash": []byte(hashText(artefact.Value)),
		"evidence_hash":       []byte(evidenceHash),
	}
	if artefact.Value != "" && len(artefact.Value) <= 256 {
		index["artefact_value"] = []byte(artefact.Value)
	}

	// Add rather than Upsert: an artefact is an observation anchored to a span
	// in one file, so two files carrying the same value are two artefacts.
	// Deduplicating them is the job of the canonical entity layer, not this one.
	return batch.Add(
		[]graphstore.NodeType{graphstore.NodeTypeMicroArtefact},
		props.Map{
			"kind":          artefact.Kind,
			"detector":      artefact.Detector,
			"value":         artefact.Value,
			"summary":       artefact.Summary,
			"confidence":    artefact.Confidence,
			"start":         artefact.Span.Start,
			"end":           artefact.Span.End,
			"attributes":    artefact.Attributes,
			"evidence_hash": evidenceHash,
		},
		index,
	)
}

func (r *Repository) createContainsEdge(batch *duesgraph.Batch, indexedFileNode, artefactNode graphstore.NodeID, artefact model.Artefact) error {
	return batch.EnsureEdge(
		indexedFileNode,
		artefactNode,
		[]graphstore.EdgeType{graphstore.EdgeTypeContains},
		props.Map{
			"detector": artefact.Detector,
			"start":    artefact.Span.Start,
			"end":      artefact.Span.End,
		},
		map[string][]byte{
			"contains_kind": []byte(artefact.Kind),
			// strconv encodings, deliberately unchanged — see the note in
			// lib/enrichment on why these wait for the rebuild.
			"start": []byte(strconv.FormatInt(artefact.Span.Start, 10)),
			"end":   []byte(strconv.FormatInt(artefact.Span.End, 10)),
		},
	)
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

	kinds := r.kinds()
	indexedIDs, err := r.graph.NodesByAnyType(kinds.MatchTypes(schema.KindIndexedFile))
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
	// GetEdges reports IDs it could not resolve rather than failing on the
	// first one. An ID can go missing between the type lookup and the fetch if
	// a concurrent writer deleted the edge, which for an export is a reason to
	// omit it, not to abandon the render.
	containsEdges, _, err := r.graph.GetEdges(containsEdgeIDs)
	if err != nil {
		return err
	}

	relationEdgeIDs, err := r.graph.EdgesByAnyType(kinds.MatchEdgeTypes(schema.EdgeKindRelation))
	if err != nil {
		return err
	}
	relationEdges, _, err := r.graph.GetEdges(relationEdgeIDs)
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

func unpackNodeProps(data []byte) props.Map { return props.Unpack(data) }

func intProp(m props.Map, key string) int { return m.Int(key) }

// VerifyOneFileParent re-checks the containment invariant against committed
// state, for use by an integrity command rather than the ingest path.
//
// storeArtefactNodes enforces it at write time from the batch's own record;
// this is the independent after-the-fact check over what is actually stored.
func (r *Repository) VerifyOneFileParent(artefactNodeID graphstore.NodeID) error {
	parents, err := r.graph.Neighbours(artefactNodeID, graphstore.DirectionInbound,
		[]graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		return err
	}

	fileParentCount := 0
	for _, parent := range parents {
		if parent.Node == nil || !r.kinds().Is(parent.Node, schema.KindIndexedFile) {
			continue
		}
		fileParentCount++
	}

	if fileParentCount != 1 {
		return fmt.Errorf("micro-artefact node %d must have exactly one file parent edge; found %d",
			artefactNodeID, fileParentCount)
	}
	return nil
}

func hashText(value string, rest ...string) string { return ids.HashText(value, rest...) }

func relationNodeLookupKey(kind, value string) string { return ids.ArtefactLookupKey(kind, value) }

func (r *Repository) storeRelationEdges(batch *duesgraph.Batch, artefactNodesByKey map[string][]graphstore.NodeID, relations []model.Relation) error {
	relationLabels := []graphstore.EdgeType{r.kinds().EdgeType(schema.EdgeKindRelation)}

	for _, relation := range relations {
		fromNodes := artefactNodesByKey[relationNodeLookupKey(relation.FromKind, relation.FromValue)]
		toNodes := artefactNodesByKey[relationNodeLookupKey(relation.ToKind, relation.ToValue)]
		if len(fromNodes) == 0 || len(toNodes) == 0 {
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

		for _, fromID := range fromNodes {
			for _, toID := range toNodes {
				if fromID == toID {
					continue
				}

				// relation_type and method discriminate, so two artefacts can
				// carry more than one relation. EdgeExists, which this
				// replaced, matched on endpoints and type alone and so
				// collapsed them into whichever was written first.
				err := batch.EnsureEdge(
					fromID, toID, relationLabels,
					props.Map{
						"relation_type":      relation.RelationType,
						"method":             relation.Method,
						"deterministic_flag": relation.Deterministic,
						"confidence":         relation.Confidence,
						"evidence_fields":    evidence,
					},
					map[string][]byte{
						"relation_type":      []byte(relation.RelationType),
						"relation_method":    []byte(relation.Method),
						"deterministic_flag": []byte(strconv.FormatBool(relation.Deterministic)),
					},
					relation.RelationType, relation.Method,
				)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}
