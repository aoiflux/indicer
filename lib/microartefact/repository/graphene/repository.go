package graphene

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"indicer/lib/fsx"

	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/ids"
	"indicer/lib/graph/propindex"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/microartefact/model"
	reppkg "indicer/lib/microartefact/repository"

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

// Open acquires a reference to root's graph store, creating it if needed.
// Close releases it.
func Open(root string) (*Repository, error) {
	return OpenWith(root, duesgraph.OpenOptions{})
}

// OpenWith opens root's graph store with opts. A read-only open of a data root
// without one is duesgraph.ErrNoGraph, and nothing is created.
func OpenWith(root string, opts duesgraph.OpenOptions) (*Repository, error) {
	handle, err := duesgraph.OpenWith(root, opts)
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

// Store stores a file's artefacts, unless the file already has some.
func (r *Repository) Store(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation) error {
	if len(artefacts) == 0 {
		return nil
	}
	return r.Record(file, artefacts, relations, reppkg.Result{Status: reppkg.StatusExtracted})
}

// Record stores what extraction found in a file, in one transaction: its
// artefacts and their relations, and on its indexed node the extraction
// status and the artefact count, which the HTML export ranks files by.
//
// With result.Replace, the artefacts an earlier run stored for the file are
// deleted first. Without it, a file that already has artefacts is left as it
// is: --force used to have no effect, because this returned quietly whether or
// not it was asked to replace (GRA-12).
func (r *Repository) Record(file model.FileRecord, artefacts []model.Artefact, relations []model.Relation, result reppkg.Result) error {
	if r == nil || r.graph == nil {
		return fmt.Errorf("micro-artefact repository is not open")
	}
	writer, err := r.handle.Writer(r.actor)
	if err != nil {
		return err
	}
	defer writer.Close()

	batch := writer.Batch()
	if err := r.record(batch, file, artefacts, relations, result); err != nil {
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

func (r *Repository) record(batch *duesgraph.Batch, file model.FileRecord, artefacts []model.Artefact, relations []model.Relation, result reppkg.Result) error {
	if err := validateStoreHierarchy(file); err != nil {
		return err
	}
	indexedFileNode, exists, err := r.findIndexedFileNode(file.IndexedFileID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("micro-artefact store: indexed_file_id=%s not found in graphdb; run dues enrich to backfill file hierarchy first", file.IndexedFileID)
	}

	existing, err := r.fileArtefacts(indexedFileNode)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		if !result.Replace {
			return nil
		}
		for _, id := range existing {
			batch.DeleteNode(id)
		}
	}

	if len(artefacts) > 0 {
		artefactNodesByKey, err := r.storeArtefactNodes(batch, indexedFileNode, file.Hash, artefacts)
		if err != nil {
			return err
		}
		if err := r.storeRelationEdges(batch, artefactNodesByKey, relations); err != nil {
			return err
		}
	}

	count := int64(len(artefacts))
	return batch.Update(indexedFileNode,
		props.Map{ArtefactCountProperty: count, StatusProperty: result.Status},
		map[string][]byte{ArtefactCountProperty: propindex.Int64(count), StatusProperty: []byte(result.Status)})
}

// Properties extraction records on a file's indexed node.
const (
	ArtefactCountProperty = "artefact_count"
	StatusProperty        = "micro_status"
)

// ExtractionStatus reports what an earlier run recorded for the indexed file:
// its extraction status, or StatusExtracted for a file with artefacts stored
// before statuses were recorded, or "" for a file never examined.
func (r *Repository) ExtractionStatus(indexedFileID string) (string, error) {
	if r == nil || r.graph == nil {
		return "", fmt.Errorf("micro-artefact repository is not open")
	}
	id, exists, err := r.findIndexedFileNode(indexedFileID)
	if err != nil || !exists {
		return "", err
	}
	node, err := r.graph.GetNode(id)
	if err != nil {
		return "", err
	}
	fields, err := props.Unpack(node.Properties)
	if err != nil {
		return "", fmt.Errorf("graph node %d: %w", id, err)
	}
	if status := fields.Str(StatusProperty); status != "" {
		return status, nil
	}
	has, err := r.hasFileArtefacts(id)
	if err != nil || !has {
		return "", err
	}
	return reppkg.StatusExtracted, nil
}

// fileArtefacts lists the micro-artefact nodes under an indexed file.
func (r *Repository) fileArtefacts(fileNodeID graphstore.NodeID) ([]graphstore.NodeID, error) {
	neighbours, err := r.graph.Neighbours(fileNodeID, graphstore.DirectionOutbound, []graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		return nil, err
	}
	var out []graphstore.NodeID
	for _, n := range neighbours {
		if n.Node != nil && n.Node.HasLabel(graphstore.NodeTypeMicroArtefact) {
			out = append(out, n.Node.ID)
		}
	}
	return out, nil
}

func validateStoreHierarchy(file model.FileRecord) error {
	// The indexed file is named by its record's own ID, as enrichment names
	// it. A path-derived ID used to be computed here when none was given,
	// which matched no node enrichment wrote (GRA-1).
	if file.IndexedFileID == "" {
		return fmt.Errorf("micro-artefact store: IndexedFileID is required")
	}
	if file.EvidenceFileID == "" {
		return fmt.Errorf("micro-artefact store: EvidenceFileID is required (micro-artefacts must belong to an evidence file → partition → indexed file hierarchy)")
	}
	if file.PartitionID == "" {
		return fmt.Errorf("micro-artefact store: PartitionID is required (micro-artefacts must belong to an evidence file → partition → indexed file hierarchy)")
	}
	return nil
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

// ErrNothingToExport is returned by ExportHTML when no indexed file has
// micro-artefacts. The export used to fail inside the renderer instead.
var ErrNothingToExport = errors.New("micro-artefact export: no indexed file has micro-artefacts")

// ExportHTML writes an interactive HTML view of the topK indexed files with
// the most micro-artefacts (all of them when topK is 0), with their
// artefacts, the partition and evidence above them, and the edges among
// those nodes. It reads only the edges of the nodes it shows: it used to
// load every containment and relation edge in the store (GRA-17).
//
// The file is written beside outPath and renamed into place; an existing
// file is replaced only with overwrite. It returns how many indexed files the
// view shows.
func (r *Repository) ExportHTML(outPath string, topK int, overwrite bool) (int, error) {
	if r == nil || r.graph == nil {
		return 0, fmt.Errorf("micro-artefact export: repository is not open")
	}

	kinds := r.kinds()
	ranked, _, _, err := r.rankArtefactFiles(context.Background(), topK)
	if err != nil {
		return 0, err
	}
	if len(ranked) == 0 {
		return 0, ErrNothingToExport
	}

	nodesByID := map[graphstore.NodeID]*graphstore.Node{}
	edgesByID := map[graphstore.EdgeID]*graphstore.Edge{}
	keep := func(n graphstore.NeighbourResult) {
		if n.Node != nil {
			nodesByID[n.Node.ID] = n.Node
		}
		if n.Edge != nil {
			edgesByID[n.Edge.ID] = n.Edge
		}
	}
	containsOnly := []graphstore.EdgeType{graphstore.EdgeTypeContains}
	var artefactIDs []graphstore.NodeID
	for _, item := range ranked {
		node, err := r.graph.GetNode(item.id)
		if err != nil {
			return 0, err
		}
		nodesByID[item.id] = node

		parents, err := r.graph.Neighbours(item.id, graphstore.DirectionInbound, containsOnly)
		if err != nil {
			return 0, err
		}
		for _, parent := range parents {
			if parent.Node == nil {
				continue
			}
			keep(parent)
			grandParents, err := r.graph.Neighbours(parent.Node.ID, graphstore.DirectionInbound, containsOnly)
			if err != nil {
				return 0, err
			}
			for _, gp := range grandParents {
				keep(gp)
			}
		}
		children, err := r.graph.Neighbours(item.id, graphstore.DirectionOutbound, containsOnly)
		if err != nil {
			return 0, err
		}
		for _, child := range children {
			if child.Node == nil || !child.Node.HasLabel(graphstore.NodeTypeMicroArtefact) {
				continue
			}
			keep(child)
			artefactIDs = append(artefactIDs, child.Node.ID)
		}
	}
	relationTypes := kinds.MatchEdgeTypes(schema.EdgeKindRelation)
	for _, id := range artefactIDs {
		edges, err := r.graph.EdgesOf(id, graphstore.DirectionOutbound, relationTypes)
		if err != nil {
			return 0, err
		}
		for _, e := range edges {
			if e == nil {
				continue
			}
			if _, ok := nodesByID[e.Dst]; ok {
				edgesByID[e.ID] = e
			}
		}
	}

	nodes := make([]*graphstore.Node, 0, len(nodesByID))
	for _, node := range nodesByID {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	edges := make([]*graphstore.Edge, 0, len(edgesByID))
	for _, e := range edgesByID {
		_, src := nodesByID[e.Src]
		_, dst := nodesByID[e.Dst]
		if src && dst {
			edges = append(edges, e)
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })

	tmp, err := os.CreateTemp(filepath.Dir(outPath), "."+filepath.Base(outPath)+".*.tmp")
	if err != nil {
		return 0, fmt.Errorf("micro-artefact export: %w", err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	err = graphviz.ExportInteractiveHTMLWithOptions(nodes, edges, tmpPath, graphviz.ExportOptions{
		Title:    "DUES Micro-Artefact Graph",
		Subtitle: fmt.Sprintf("Lazy graph view over top-%d indexed files (by artefact count)", len(ranked)),
	})
	if err == nil {
		err = fsx.ReplaceFile(tmpPath, outPath, overwrite)
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return 0, fmt.Errorf("micro-artefact export: %w", err)
	}
	return len(ranked), nil
}

// rankedFile is an indexed file node and how many artefacts it holds.
type rankedFile struct {
	id    graphstore.NodeID
	count int
}

// before orders files by artefact count, most first, then by node.
func (a rankedFile) before(b rankedFile) bool {
	if a.count != b.count {
		return a.count > b.count
	}
	return a.id < b.id
}

// fileHeap is a min-heap in that order: its root is the file to drop first.
type fileHeap []rankedFile

func (h fileHeap) Len() int           { return len(h) }
func (h fileHeap) Less(i, j int) bool { return h[j].before(h[i]) }
func (h fileHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *fileHeap) Push(x any)        { *h = append(*h, x.(rankedFile)) }
func (h *fileHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// rankArtefactFiles returns the limit indexed files with the most artefacts
// (every one when limit is 0), most first, and how many files hold artefacts
// and how many they hold in all. It reads every indexed file node once, but
// keeps only the files it returns.
func (r *Repository) rankArtefactFiles(ctx context.Context, limit int) (ranked []rankedFile, files, artefacts int, err error) {
	indexedIDs, err := r.graph.NodesByAnyType(r.kinds().MatchTypes(schema.KindIndexedFile))
	if err != nil {
		return nil, 0, 0, err
	}
	top := &fileHeap{}
	for i, id := range indexedIDs {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		node, err := r.graph.GetNode(id)
		if err != nil {
			return nil, 0, 0, err
		}
		fields, err := props.Unpack(node.Properties)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("graph node %d: %w", id, err)
		}
		count := intProp(fields, ArtefactCountProperty)
		if count == 0 && !fields.Has(StatusProperty) {
			// Stored before counts were recorded.
			arts, err := r.fileArtefacts(id)
			if err != nil {
				return nil, 0, 0, err
			}
			count = len(arts)
		}
		if count == 0 {
			continue
		}
		files++
		artefacts += count
		f := rankedFile{id: id, count: count}
		switch {
		case limit <= 0 || top.Len() < limit:
			heap.Push(top, f)
		case f.before((*top)[0]):
			(*top)[0] = f
			heap.Fix(top, 0)
		}
	}
	ranked = []rankedFile(*top)
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].before(ranked[j]) })
	return ranked, files, artefacts, nil
}

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

// maxRelationEdges bounds the edges one relation becomes. A relation joins
// two values, and each value can be held by many artefacts of the file; every
// pair of them used to get an edge, so a value seen a thousand times related
// to itself became a million edges (GRA-17).
const maxRelationEdges = 64

// relationPairs is the artefact pairs a relation joins. Between artefacts of
// one value (the same group) the first is joined to each other one, a star
// rather than every pair; between two values, pairs are taken in order up to
// the cap.
func relationPairs(from, to []graphstore.NodeID) [][2]graphstore.NodeID {
	var pairs [][2]graphstore.NodeID
	if sameNodes(from, to) {
		for _, id := range to[1:] {
			if len(pairs) == maxRelationEdges {
				break
			}
			pairs = append(pairs, [2]graphstore.NodeID{to[0], id})
		}
		return pairs
	}
	for _, f := range from {
		for _, t := range to {
			if f == t {
				continue
			}
			if len(pairs) == maxRelationEdges {
				return pairs
			}
			pairs = append(pairs, [2]graphstore.NodeID{f, t})
		}
	}
	return pairs
}

func sameNodes(a, b []graphstore.NodeID) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

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

		for _, pair := range relationPairs(fromNodes, toNodes) {
			fromID, toID := pair[0], pair[1]
			{

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
