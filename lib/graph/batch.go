package graph

import (
	"fmt"
	"strings"

	"indicer/lib/graph/ids"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"

	graphenedb "github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
)

// EdgeKeyProperty is the index key under which EnsureEdge records an edge's
// identity, so a re-run can find it again.
//
// graphene's EdgeExists only matches on endpoints and type, which is too coarse
// for DUES: two artefacts can be joined by more than one relation, differing in
// relation_type and method. ATTACK_PATH_GRAPH_LLD.md §8.2 wants the 4-tuple,
// and this is how it is enforced with the machinery that already exists.
const EdgeKeyProperty = "edge_key"

// UpsertKey identifies a node by an indexed property.
type UpsertKey struct {
	Key   string
	Value string
}

func (k UpsertKey) cacheKey() string { return k.Key + "\x00" + k.Value }

// indexOp is a property-index registration deferred until after the commit.
type indexOp struct {
	node  graphstore.NodeID
	edge  graphstore.EdgeID
	isB   bool // true for an edge
	pairs map[string][]byte
}

// Batch groups writes into one atomic, durable, attributed commit.
//
// # Why this is a correctness change, not a speed-up
//
// graphene attaches an actor and a signature to a *batch commit* record, and
// nowhere else: AddNode writes a bare WAL record with no commit frame, no
// sequence number and no signature, and RequireSignedCommits only rejects a
// batch that lacks one. So a store written with loose AddNode calls cannot be
// signed at all, whatever options it was opened with. Moving the write path
// onto batches is what makes the signing posture mean anything.
//
// It is also two orders of magnitude fewer fsyncs, but that is the lesser half.
type Batch struct {
	writer *Writer
	graph  *graphenedb.Graph
	tx     *graphenedb.Tx
	kinds  *schema.Registry

	// producer stamps every node and edge this batch creates. It is applied
	// here rather than at each call site so that no derived record can be
	// written unattributed by omission.
	producer string

	// local resolves nodes created earlier in this same batch. They are not yet
	// visible to NodesByProperty, which reads committed state.
	local map[string]graphstore.NodeID

	// edgeKeys dedupes within the batch, for the same reason.
	edgeKeys map[string]struct{}

	deferred []indexOp
	done     bool
}

// Batch begins a transaction. Every write on it lands, or none does.
func (w *Writer) Batch() *Batch {
	return &Batch{
		writer:   w,
		graph:    w.handle.Graph(),
		tx:       w.handle.Graph().Begin().As(w.actor.TxContext()),
		kinds:    w.handle.Schema(),
		producer: w.producer.String(),
		local:    map[string]graphstore.NodeID{},
		edgeKeys: map[string]struct{}{},
	}
}

// hasEquivalentEdge walks the edges of the given types leaving src and reports
// whether one already expresses this relationship.
//
// An edge with no recorded key predates edge_key. Those are treated as a match
// on the coarse identity alone — endpoints and type — which is precisely the
// rule that wrote them, so a re-run neither duplicates them nor invents a
// distinction the stored data cannot support.
func (b *Batch) hasEquivalentEdge(
	src, dst graphstore.NodeID, labels []graphstore.EdgeType, edgeKey string,
) (bool, error) {
	edges, err := b.graph.EdgesOf(src, graphstore.DirectionOutbound, labels)
	if err != nil {
		return false, fmt.Errorf("graph: EnsureEdge scan: %w", err)
	}
	for _, edge := range edges {
		if edge == nil || edge.Dst != dst {
			continue
		}
		stored := props.Unpack(edge.Properties).Str(EdgeKeyProperty)
		if stored == "" || stored == edgeKey {
			return true, nil
		}
	}
	return false, nil
}

// Attributed reports whether this batch records who made the change.
// Attribution cannot be added retrospectively, so an unattributed write is
// permanently anonymous.
func (b *Batch) Attributed() bool { return b.tx.Attributed() }

// Atomic reports whether the backing store applies the batch as one unit.
func (b *Batch) Atomic() bool { return b.tx.Atomic() }

// Ops is the number of pending operations.
func (b *Batch) Ops() int { return b.tx.Ops() }

// Upsert returns the node identified by key, creating it if absent.
//
// Resolution order is batch-local, then the handle's process-wide identity
// cache, then the store. The cache is safe to keep because graphene never
// reuses IDs and compaction preserves them, so an ID once resolved stays valid
// for the life of the process.
func (b *Batch) Upsert(
	key UpsertKey,
	labels []graphstore.NodeType,
	properties props.Map,
	indexPairs map[string][]byte,
) (graphstore.NodeID, error) {
	if b.done {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert on a finished batch")
	}
	if key.Key == "" || key.Value == "" {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert needs a non-empty index key and value")
	}

	cacheKey := key.cacheKey()
	if id, ok := b.local[cacheKey]; ok {
		return id, nil
	}
	if id, ok := b.writer.handle.lookupIdentity(cacheKey); ok {
		return id, nil
	}

	hits, err := b.graph.NodesByProperty(key.Key, []byte(key.Value))
	if err != nil {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert lookup %s: %w", key.Key, err)
	}
	if len(hits) > 0 {
		b.local[cacheKey] = hits[0]
		b.writer.handle.rememberIdentity(cacheKey, hits[0])
		return hits[0], nil
	}

	packed, err := props.Pack(map[string]any(b.stamped(properties)))
	if err != nil {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert pack properties: %w", err)
	}

	// The ID is reserved immediately and is usable as an edge endpoint before
	// the commit, which is what lets a node and its containment edge land in
	// one atomic unit.
	id := b.tx.AddNode(&graphstore.Node{Labels: labels, Properties: packed})
	if id == graphstore.InvalidNodeID {
		return id, fmt.Errorf("graph: Upsert could not reserve a node id (batch already failed)")
	}
	b.local[cacheKey] = id

	pairs := map[string][]byte{key.Key: []byte(key.Value)}
	for k, v := range indexPairs {
		pairs[k] = v
	}
	b.deferred = append(b.deferred, indexOp{node: id, pairs: pairs})

	return id, nil
}

// Add creates a node unconditionally, for entities with no natural key.
func (b *Batch) Add(
	labels []graphstore.NodeType,
	properties props.Map,
	indexPairs map[string][]byte,
) (graphstore.NodeID, error) {
	if b.done {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Add on a finished batch")
	}

	packed, err := props.Pack(map[string]any(b.stamped(properties)))
	if err != nil {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Add pack properties: %w", err)
	}

	id := b.tx.AddNode(&graphstore.Node{Labels: labels, Properties: packed})
	if id == graphstore.InvalidNodeID {
		return id, fmt.Errorf("graph: Add could not reserve a node id (batch already failed)")
	}
	if len(indexPairs) > 0 {
		b.deferred = append(b.deferred, indexOp{node: id, pairs: indexPairs})
	}
	return id, nil
}

// stamped returns the property map with the producer version applied. A caller
// that has already set one keeps it, so a re-derivation can preserve the stamp
// of whatever produced the value it is carrying forward.
func (b *Batch) stamped(fields props.Map) props.Map {
	if fields == nil {
		fields = props.Map{}
	}
	if _, already := fields[ProducerVersionProperty]; !already {
		fields[ProducerVersionProperty] = b.producer
	}
	return fields
}

// Index registers property-index entries against a node, whether or not this
// batch created it.
//
// Upsert only registers on insert, which is not always enough: a node can be
// created by one call that knows a single key and later revisited by another
// that knows more. Registering unconditionally is what keeps the second call's
// keys from being lost when the first happened to run earlier.
func (b *Batch) Index(id graphstore.NodeID, pairs map[string][]byte) {
	if b.done || id == graphstore.InvalidNodeID || len(pairs) == 0 {
		return
	}
	b.deferred = append(b.deferred, indexOp{node: id, pairs: pairs})
}

// IndexEdge registers property-index entries against an edge.
func (b *Batch) IndexEdge(id graphstore.EdgeID, pairs map[string][]byte) {
	if b.done || id == graphstore.InvalidEdgeID || len(pairs) == 0 {
		return
	}
	b.deferred = append(b.deferred, indexOp{edge: id, isB: true, pairs: pairs})
}

// EnsureEdge creates an edge unless an equivalent one already exists.
//
// Identity is the endpoints, the labels and any discriminators the caller
// supplies — typically relation_type and method. Passing none reduces to
// "one edge of this type between these two nodes".
func (b *Batch) EnsureEdge(
	src, dst graphstore.NodeID,
	labels []graphstore.EdgeType,
	properties props.Map,
	indexPairs map[string][]byte,
	discriminators ...string,
) error {
	if b.done {
		return fmt.Errorf("graph: EnsureEdge on a finished batch")
	}
	if src == graphstore.InvalidNodeID || dst == graphstore.InvalidNodeID {
		return fmt.Errorf("graph: EnsureEdge needs two valid endpoints (got %d -> %d)", src, dst)
	}

	edgeKey := edgeIdentity(src, dst, labels, discriminators)
	if _, seen := b.edgeKeys[edgeKey]; seen {
		return nil
	}

	// Committed duplicates are found through the index rather than EdgeExists,
	// which cannot see the discriminators.
	existing, err := b.graph.EdgesByProperty(EdgeKeyProperty, []byte(edgeKey))
	if err != nil {
		return fmt.Errorf("graph: EnsureEdge lookup: %w", err)
	}
	if len(existing) > 0 {
		b.edgeKeys[edgeKey] = struct{}{}
		return nil
	}

	// Edges written before edge_key existed carry no such index entry, so on a
	// store that holds any, the index lookup above cannot see them and every
	// re-run would duplicate the lot. Fall back to walking the incident edges,
	// which is what the EdgeExists check this replaced did.
	if b.kinds.Legacy() {
		duplicate, err := b.hasEquivalentEdge(src, dst, labels, edgeKey)
		if err != nil {
			return err
		}
		if duplicate {
			b.edgeKeys[edgeKey] = struct{}{}
			return nil
		}
	}

	// Carry the key in the blob as well as the index so a later run can compare
	// it without consulting the index, which is what the fallback above needs.
	if properties == nil {
		properties = props.Map{}
	}
	properties[EdgeKeyProperty] = edgeKey

	packed, err := props.Pack(map[string]any(b.stamped(properties)))
	if err != nil {
		return fmt.Errorf("graph: EnsureEdge pack properties: %w", err)
	}

	id := b.tx.AddEdge(&graphstore.Edge{Src: src, Dst: dst, Labels: labels, Properties: packed})
	if id == graphstore.InvalidEdgeID {
		return fmt.Errorf("graph: EnsureEdge could not reserve an edge id (batch already failed)")
	}
	b.edgeKeys[edgeKey] = struct{}{}

	pairs := map[string][]byte{EdgeKeyProperty: []byte(edgeKey)}
	for k, v := range indexPairs {
		pairs[k] = v
	}
	b.deferred = append(b.deferred, indexOp{edge: id, isB: true, pairs: pairs})

	return nil
}

// Commit applies the batch and then registers the property-index entries.
//
// # The gap this cannot close
//
// Index registration is not part of the transaction — graphene writes it as a
// separate WAL operation — so a crash between the commit and the flush leaves
// correctly stored nodes with missing index entries. Those nodes are reachable
// by ID and by label but not by property lookup.
//
// This is recoverable but not automatically: RebuildIndexes repairs structure,
// not content, and explicitly cannot recreate property entries, because the
// values were supplied by DUES in an encoding the storage layer never sees. A
// flush failure is therefore reported with the affected IDs rather than
// swallowed, so the operator knows what to re-run.
func (b *Batch) Commit() error {
	if b.done {
		return fmt.Errorf("graph: Commit on a finished batch")
	}
	b.done = true

	if err := b.tx.Commit(); err != nil {
		return fmt.Errorf("graph: commit batch: %w", err)
	}

	for _, op := range b.deferred {
		if op.isB {
			if err := b.graph.IndexEdgeProperties(op.edge, op.pairs); err != nil {
				return fmt.Errorf(
					"graph: batch committed but indexing edge %d failed: %w; "+
						"the edge is stored and reachable by id, but not by property lookup",
					op.edge, err)
			}
			continue
		}
		if err := b.graph.IndexNodeProperties(op.node, op.pairs); err != nil {
			return fmt.Errorf(
				"graph: batch committed but indexing node %d failed: %w; "+
					"the node is stored and reachable by id, but not by property lookup",
				op.node, err)
		}
	}

	// Identity is only promoted to the process-wide cache once it is durable.
	// Promoting at Upsert time would leave a rolled-back node cached as real.
	for cacheKey, id := range b.local {
		b.writer.handle.rememberIdentity(cacheKey, id)
	}

	return nil
}

// Rollback discards the batch. Nothing is written and nothing is cached.
func (b *Batch) Rollback() {
	if b == nil || b.done {
		return
	}
	b.done = true
	_ = b.tx.Rollback()
}

// edgeIdentity derives the stable key an edge is deduplicated on.
func edgeIdentity(src, dst graphstore.NodeID, labels []graphstore.EdgeType, discriminators []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d>%d", src, dst)
	for _, label := range labels {
		fmt.Fprintf(&b, ":%d", label)
	}
	parts := append([]string{b.String()}, discriminators...)
	return ids.HashText(parts[0], parts[1:]...)
}
