package graph

import (
	"path/filepath"
	"testing"

	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"

	"github.com/aoiflux/graphene/disk"
	graphstore "github.com/aoiflux/graphene/store"
)

func testWriter(t *testing.T, h *Handle) *Writer {
	t.Helper()
	w, err := h.Writer(ActorRef{Name: "examiner@lab", ActorID: 4711, RoleID: 3, KeyID: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

func evidenceLabels(h *Handle) []graphstore.NodeType {
	return []graphstore.NodeType{h.Schema().NodeType(schema.KindEvidenceFile)}
}

// TestBatch_CommitIsAttributed is the fix for the finding that reordered the
// plan: graphene attaches an actor and a signature to batch commit records and
// nowhere else, so the loose AddNode path DUES used could never be signed or
// attributed however the store was opened.
//
// It reads the log back with InspectWAL rather than trusting the API, because
// the whole point is what lands on disk.
func TestBatch_CommitIsAttributed(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	writer := testWriter(t, handle)
	batch := writer.Batch()
	if !batch.Attributed() {
		t.Error("batch is not attributed despite an actor being supplied")
	}
	if !batch.Atomic() {
		t.Error("batch is not atomic on the disk backend")
	}
	if _, err := batch.Upsert(
		UpsertKey{Key: "evidence_file_id", Value: "abc"},
		evidenceLabels(handle),
		props.Map{"name": "disk1.E01"},
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	dir := handle.Dir()
	if err := handle.Release(); err != nil {
		t.Fatal(err)
	}

	info, err := disk.InspectWAL(filepath.Join(dir, "graphene.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Commits) == 0 {
		t.Fatal("the log holds no batch commit records; writes are still going through " +
			"the unbatched path, which can carry neither an actor nor a signature")
	}

	var sawActor bool
	for _, commit := range info.Commits {
		if commit.ActorID == 4711 {
			sawActor = true
		}
	}
	if !sawActor {
		t.Errorf("no commit records actor 4711; commits: %+v", info.Commits)
	}
}

func TestBatch_UnattributedActorIsReported(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer, err := handle.Writer(ActorRef{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	if writer.Batch().Attributed() {
		t.Error("a batch with no actor reports itself attributed; attribution cannot " +
			"be added retrospectively, so this must be visible at write time")
	}
}

// A batch is all or nothing, including the edge that references a node created
// in the same batch.
func TestBatch_RollbackWritesNothing(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer := testWriter(t, handle)
	batch := writer.Batch()

	parent, err := batch.Upsert(
		UpsertKey{Key: "evidence_file_id", Value: "rolled-back"},
		evidenceLabels(handle), props.Map{"name": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := batch.Upsert(
		UpsertKey{Key: "partition_id", Value: "rolled-back-p1"},
		[]graphstore.NodeType{handle.Schema().NodeType(schema.KindPartition)},
		props.Map{"name": "p1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.EnsureEdge(parent, child,
		[]graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, nil); err != nil {
		t.Fatal(err)
	}

	batch.Rollback()

	hits, err := handle.Graph().NodesByProperty("evidence_file_id", []byte("rolled-back"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("rolled-back batch left %d node(s) behind", len(hits))
	}
	if _, cached := handle.lookupIdentity("evidence_file_id\x00rolled-back"); cached {
		t.Error("a rolled-back node was promoted to the identity cache")
	}
}

// A node and an edge that references it must land in one unit. This is what the
// old read-then-write path could not express.
func TestBatch_EdgeToNodeCreatedInTheSameBatch(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer := testWriter(t, handle)
	batch := writer.Batch()

	parent, err := batch.Upsert(UpsertKey{Key: "evidence_file_id", Value: "e1"},
		evidenceLabels(handle), props.Map{"name": "disk1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	child, err := batch.Upsert(UpsertKey{Key: "partition_id", Value: "p1"},
		[]graphstore.NodeType{handle.Schema().NodeType(schema.KindPartition)},
		props.Map{"name": "p1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.EnsureEdge(parent, child,
		[]graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	neighbours, err := handle.Graph().Neighbours(parent, graphstore.DirectionOutbound,
		[]graphstore.EdgeType{graphstore.EdgeTypeContains})
	if err != nil {
		t.Fatal(err)
	}
	if len(neighbours) != 1 || neighbours[0].Node.ID != child {
		t.Errorf("expected one contains edge to node %d, got %+v", child, neighbours)
	}
}

// Two Upserts on one key inside a batch must yield one node. Before batching,
// the second lookup could not see the first insert.
func TestBatch_UpsertDedupesWithinAndAcrossBatches(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer := testWriter(t, handle)

	first := writer.Batch()
	a, err := first.Upsert(UpsertKey{Key: "evidence_file_id", Value: "same"},
		evidenceLabels(handle), props.Map{"name": "disk1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := first.Upsert(UpsertKey{Key: "evidence_file_id", Value: "same"},
		evidenceLabels(handle), props.Map{"name": "disk1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("within one batch the same key produced nodes %d and %d", a, b)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}

	second := writer.Batch()
	c, err := second.Upsert(UpsertKey{Key: "evidence_file_id", Value: "same"},
		evidenceLabels(handle), props.Map{"name": "disk1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c != a {
		t.Errorf("a later batch created node %d for a key already held by %d", c, a)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}

	hits, err := handle.Graph().NodesByProperty("evidence_file_id", []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Errorf("expected exactly 1 node for the key, got %d", len(hits))
	}
}

// EdgeExists matches only endpoints and type, so two relations that differ in
// relation_type would collapse into one. The discriminators are what keep them
// apart.
func TestBatch_EnsureEdgeDiscriminatesBeyondType(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	relation := []graphstore.EdgeType{handle.Schema().EdgeType(schema.EdgeKindRelation)}
	writer := testWriter(t, handle)
	batch := writer.Batch()

	src, err := batch.Add(evidenceLabels(handle), props.Map{"n": "a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := batch.Add(evidenceLabels(handle), props.Map{"n": "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := batch.EnsureEdge(src, dst, relation, props.Map{"relation_type": "resolves_to"}, nil,
		"resolves_to", "same-identifier"); err != nil {
		t.Fatal(err)
	}
	// Same 4-tuple: must be suppressed.
	if err := batch.EnsureEdge(src, dst, relation, props.Map{"relation_type": "resolves_to"}, nil,
		"resolves_to", "same-identifier"); err != nil {
		t.Fatal(err)
	}
	// Different relation_type: must be kept.
	if err := batch.EnsureEdge(src, dst, relation, props.Map{"relation_type": "connected_to"}, nil,
		"connected_to", "same-identifier"); err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	edges, err := handle.Graph().EdgesOf(src, graphstore.DirectionOutbound, relation)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Errorf("expected 2 relation edges (one per relation_type), got %d", len(edges))
	}

	// Re-running against the committed store must not duplicate either.
	rerun := writer.Batch()
	if err := rerun.EnsureEdge(src, dst, relation, props.Map{"relation_type": "resolves_to"}, nil,
		"resolves_to", "same-identifier"); err != nil {
		t.Fatal(err)
	}
	if err := rerun.Commit(); err != nil {
		t.Fatal(err)
	}
	edges, err = handle.Graph().EdgesOf(src, graphstore.DirectionOutbound, relation)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Errorf("re-running produced %d relation edges, want 2", len(edges))
	}
}

func TestBatch_RefusesUseAfterFinish(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()

	handle, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	writer := testWriter(t, handle)
	batch := writer.Batch()
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := batch.Upsert(UpsertKey{Key: "k", Value: "v"},
		evidenceLabels(handle), nil, nil); err == nil {
		t.Error("Upsert on a committed batch was accepted")
	}
	if err := batch.Commit(); err == nil {
		t.Error("a second Commit was accepted")
	}
}

func TestWriter_RefusedOnReadOnlyHandle(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)

	handle, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if _, err := handle.Writer(ActorRef{ActorID: 1}); err == nil {
		t.Error("a writer was handed out for a read-only handle")
	}
}
