package graph

import (
	"fmt"
	"reflect"
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

// pendingNode is a node this batch writes: one it creates, or a stored one it
// changes. Its record and index entries are written at Commit, in the
// transaction.
type pendingNode struct {
	created bool
	labels  []graphstore.NodeType
	fields  props.Map
	index   map[string][]byte

	// stored and storedLabels are the record as it was read, for a stored
	// node, so a change that changes nothing writes nothing.
	stored       props.Map
	storedLabels []graphstore.NodeType

	// changed records that a node created in this batch was changed after it
	// was buffered.
	changed bool
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
// Index entries are part of the same commit (GRA-3/GRA-4): they used to be
// written after it, so a crash in between left nodes that were stored but
// could not be found by the key they were written under, and the next run
// created duplicates of them.
type Batch struct {
	writer *Writer
	graph  *graphenedb.Graph
	tx     *graphenedb.Tx
	kinds  *schema.Registry

	// producer stamps every node and edge this batch creates. It is applied
	// here rather than at each call site so that no derived record can be
	// written unattributed by omission.
	producer string

	// local resolves keys to nodes created or found earlier in this same batch.
	// Nodes created here are not yet visible to NodesByProperty, which reads
	// committed state.
	local map[string]graphstore.NodeID

	nodes   map[graphstore.NodeID]*pendingNode
	order   []graphstore.NodeID
	deleted map[graphstore.NodeID]struct{}

	// edgeKeys dedupes within the batch, for the same reason as local.
	edgeKeys map[string]struct{}

	done bool
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
		nodes:    map[graphstore.NodeID]*pendingNode{},
		deleted:  map[graphstore.NodeID]struct{}{},
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
		fields, err := props.Unpack(edge.Properties)
		if err != nil {
			return false, fmt.Errorf("graph: edge %d: %w", edge.ID, err)
		}
		stored := fields.Str(EdgeKeyProperty)
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
func (b *Batch) Ops() int { return b.tx.Ops() + len(b.nodes) }

// Upsert writes the full record of the node identified by key: it creates the
// node if absent, and otherwise merges properties over the stored ones and
// replaces the index entries named in indexPairs, so a value the node no
// longer has stops matching (GRA-2). The key itself is always indexed.
//
// Resolution order is batch-local, then the handle's process-wide identity
// cache, then the store. The cache is safe to keep because graphene never
// reuses IDs and compaction preserves them; a node this process deletes is
// dropped from it.
func (b *Batch) Upsert(
	key UpsertKey,
	labels []graphstore.NodeType,
	properties props.Map,
	indexPairs map[string][]byte,
) (graphstore.NodeID, error) {
	return b.upsert(key, labels, properties, indexPairs, true)
}

// UpsertFill writes a stub of the node identified by key: it creates the node
// if absent and otherwise leaves it as it is, so a stub written while walking
// a file never overwrites the full record of its partition or evidence.
func (b *Batch) UpsertFill(
	key UpsertKey,
	labels []graphstore.NodeType,
	properties props.Map,
	indexPairs map[string][]byte,
) (graphstore.NodeID, error) {
	return b.upsert(key, labels, properties, indexPairs, false)
}

func (b *Batch) upsert(
	key UpsertKey,
	labels []graphstore.NodeType,
	properties props.Map,
	indexPairs map[string][]byte,
	replace bool,
) (graphstore.NodeID, error) {
	if b.done {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert on a finished batch")
	}
	if key.Key == "" || key.Value == "" {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert needs a non-empty index key and value")
	}
	if len(labels) == 0 {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: node %s=%q has no labels", key.Key, key.Value)
	}

	pairs := make(map[string][]byte, len(indexPairs)+1)
	for k, v := range indexPairs {
		pairs[k] = v
	}
	pairs[key.Key] = []byte(key.Value)

	cacheKey := key.cacheKey()
	id, found := b.local[cacheKey]
	if !found {
		id, found = b.writer.handle.lookupIdentity(cacheKey)
	}
	if !found {
		hits, err := b.graph.NodesByProperty(key.Key, []byte(key.Value))
		if err != nil {
			return graphstore.InvalidNodeID, fmt.Errorf("graph: Upsert lookup %s: %w", key.Key, err)
		}
		if len(hits) > 0 {
			id, found = hits[0], true
		}
	}
	if found {
		b.local[cacheKey] = id
		if !replace {
			return id, nil
		}
		if err := b.merge(id, labels, properties, pairs); err != nil {
			return graphstore.InvalidNodeID, err
		}
		return id, nil
	}

	id, err := b.create(labels, properties, pairs)
	if err != nil {
		return id, err
	}
	b.local[cacheKey] = id
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
	if len(labels) == 0 {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: Add of a node with no labels")
	}
	pairs := make(map[string][]byte, len(indexPairs))
	for k, v := range indexPairs {
		pairs[k] = v
	}
	return b.create(labels, properties, pairs)
}

// Update merges properties into the node id, which exists in the store or was
// created by this batch, and replaces the index entries named in indexPairs.
func (b *Batch) Update(id graphstore.NodeID, properties props.Map, indexPairs map[string][]byte) error {
	if b.done {
		return fmt.Errorf("graph: Update on a finished batch")
	}
	if _, gone := b.deleted[id]; gone {
		return fmt.Errorf("graph: Update of node %d, which this batch deletes", id)
	}
	return b.merge(id, nil, properties, indexPairs)
}

// DeleteNode deletes the node id and every edge incident to it.
func (b *Batch) DeleteNode(id graphstore.NodeID) {
	if b.done || id == graphstore.InvalidNodeID {
		return
	}
	if _, gone := b.deleted[id]; gone {
		return
	}
	b.deleted[id] = struct{}{}
	delete(b.nodes, id)
	for k, local := range b.local {
		if local == id {
			delete(b.local, k)
		}
	}
	b.tx.DeleteNode(id)
}

// create buffers a new node. The ID is reserved immediately and is usable as an
// edge endpoint before the commit, which is what lets a node and its
// containment edge land in one atomic unit.
func (b *Batch) create(labels []graphstore.NodeType, properties props.Map, pairs map[string][]byte) (graphstore.NodeID, error) {
	fields := b.stamped(properties)
	packed, err := props.Pack(map[string]any(fields))
	if err != nil {
		return graphstore.InvalidNodeID, fmt.Errorf("graph: pack properties: %w", err)
	}
	id := b.tx.AddNode(&graphstore.Node{Labels: labels, Properties: packed})
	if id == graphstore.InvalidNodeID {
		return id, fmt.Errorf("graph: could not reserve a node id (batch already failed)")
	}
	b.nodes[id] = &pendingNode{created: true, labels: labels, fields: fields, index: pairs}
	b.order = append(b.order, id)
	return id, nil
}

// merge applies properties and index entries to a node this batch writes,
// reading a stored node the first time. labels nil keeps the node's own.
func (b *Batch) merge(id graphstore.NodeID, labels []graphstore.NodeType, properties props.Map, pairs map[string][]byte) error {
	p, ok := b.nodes[id]
	if !ok {
		node, err := b.graph.GetNode(id)
		if err != nil {
			return fmt.Errorf("graph: read node %d: %w", id, err)
		}
		stored, err := props.Unpack(node.Properties)
		if err != nil {
			return fmt.Errorf("graph: node %d: %w", id, err)
		}
		fields := make(props.Map, len(stored)+len(properties))
		for k, v := range stored {
			fields[k] = v
		}
		p = &pendingNode{labels: node.Labels, fields: fields, index: map[string][]byte{}, stored: stored, storedLabels: node.Labels}
		b.nodes[id] = p
		b.order = append(b.order, id)
	}
	for k, v := range b.stamped(properties) {
		p.fields[k] = v
	}
	for k, v := range pairs {
		p.index[k] = v
	}
	if len(labels) > 0 {
		p.labels = labels
	}
	if p.created {
		p.changed = true
	}
	return nil
}

// stamped returns a copy of the property map with the producer version
// applied. A caller that has already set one keeps it, so a re-derivation can
// preserve the stamp of whatever produced the value it is carrying forward.
func (b *Batch) stamped(fields props.Map) props.Map {
	out := make(props.Map, len(fields)+1)
	for k, v := range fields {
		out[k] = v
	}
	if _, already := out[ProducerVersionProperty]; !already {
		out[ProducerVersionProperty] = b.producer
	}
	return out
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
	if len(labels) == 0 {
		return fmt.Errorf("graph: edge %d -> %d has no labels", src, dst)
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
	fields := b.stamped(properties)
	fields[EdgeKeyProperty] = edgeKey
	packed, err := props.Pack(map[string]any(fields))
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
	b.tx.IndexEdgeProperties(id, pairs)
	return nil
}

// Commit applies the batch, with every node's record and index entries, as one
// atomic unit.
func (b *Batch) Commit() error {
	if b.done {
		return fmt.Errorf("graph: Commit on a finished batch")
	}
	b.done = true

	for _, id := range b.order {
		p, ok := b.nodes[id]
		if !ok {
			continue // deleted later in the batch
		}
		if p.created && !p.changed {
			b.tx.IndexNodeProperties(id, p.index)
			continue
		}
		if !p.created && sameFields(p.stored, p.fields) && sameLabels(p.labels, p.storedLabels) {
			// Nothing changed. A re-run over unchanged data writes nothing,
			// because the log is never compacted (D6). The index entries are
			// derived from the properties, so they are unchanged too as long as
			// a producer that changes what it indexes bumps its version.
			continue
		}
		packed, err := props.Pack(map[string]any(p.fields))
		if err != nil {
			_ = b.tx.Rollback()
			return fmt.Errorf("graph: pack properties of node %d: %w", id, err)
		}
		node := &graphstore.Node{ID: id, Labels: p.labels, Properties: packed}
		if p.created {
			// The whole index of a node created here is what this batch gives it.
			b.tx.UpdateNodeIndexed(node, p.index)
		} else {
			b.tx.UpdateNodePartialIndex(node, p.index)
		}
	}

	if err := b.tx.Commit(); err != nil {
		return fmt.Errorf("graph: commit batch: %w", err)
	}

	// Identity is only promoted to the process-wide cache once it is durable.
	// Promoting at Upsert time would leave a rolled-back node cached as real.
	b.writer.handle.forgetIdentities(b.deleted)
	for cacheKey, id := range b.local {
		b.writer.handle.rememberIdentity(cacheKey, id)
	}
	return nil
}

// sameFields reports whether merged holds exactly the stored properties.
// merged is compared as it would read back, since msgpack decodes a value to
// the narrowest type that holds it.
func sameFields(stored, merged props.Map) bool {
	if stored == nil {
		return false
	}
	packed, err := props.Pack(map[string]any(merged))
	if err != nil {
		return false
	}
	back, err := props.Unpack(packed)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(map[string]any(stored), map[string]any(back))
}

func sameLabels(a, b []graphstore.NodeType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
