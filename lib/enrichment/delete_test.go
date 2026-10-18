package enrichment

import (
	"testing"

	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"

	graphstore "github.com/aoiflux/graphene/store"
)

func seedEvidence(t *testing.T, repo *GrapheneRepository, ev string) {
	t.Helper()
	if err := repo.UpsertEvidence(EvidenceRecord{HashBase64: ev, Name: ev}); err != nil {
		t.Fatal(err)
	}
	records := []FileRecord{
		{Level: "evidence_file", EvidenceFileID: ev, EvidenceFileName: ev, FileName: ev},
		{Level: "partition", EvidenceFileID: ev, PartitionID: ev + "-p", PartitionName: "p", FileName: "p"},
		{Level: "indexed_file", EvidenceFileID: ev, PartitionID: ev + "-p", IndexedFileID: ev + "-f1", FileName: "a.txt", Path: "/a.txt"},
		{Level: "indexed_file", EvidenceFileID: ev, PartitionID: ev + "-p", IndexedFileID: ev + "-f2", FileName: "b.txt", Path: "/b.txt"},
	}
	if err := repo.UpsertFiles(records); err != nil {
		t.Fatal(err)
	}
	// Two micro-artefacts under one file, joined by a relation.
	w, err := repo.handle.Writer(duesgraph.ActorRef{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := w.Batch()
	hits, err := repo.graph.NodesByProperty("indexed_file_id", []byte(ev+"-f1"))
	if err != nil || len(hits) == 0 {
		t.Fatalf("indexed node: %v %v", hits, err)
	}
	var arts []graphstore.NodeID
	for _, v := range []string{"x", "y"} {
		id, err := b.Add([]graphstore.NodeType{graphstore.NodeTypeMicroArtefact}, props.Map{"value": v}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.EnsureEdge(hits[0], id, []graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, nil); err != nil {
			t.Fatal(err)
		}
		arts = append(arts, id)
	}
	if err := b.EnsureEdge(arts[0], arts[1], []graphstore.EdgeType{repo.kinds().EdgeType(schema.EdgeKindRelation)}, nil, nil, "rel"); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
}

func countNodes(t *testing.T, repo *GrapheneRepository) uint64 {
	t.Helper()
	n, err := repo.graph.NodeCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestDeleteEvidenceIndex: deleting an evidence's index from the graph
// removes its partitions, indexed files, file nodes and micro-artefacts with
// their edges, keeps the evidence node when asked, and leaves other evidence
// alone.
func TestDeleteEvidenceIndex(t *testing.T) {
	repo, err := OpenGrapheneRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	seedEvidence(t, repo, "ev-a")
	perEvidence := countNodes(t, repo)
	seedEvidence(t, repo, "ev-b")

	n, err := repo.DeleteEvidenceIndex("ev-a", true)
	if err != nil {
		t.Fatal(err)
	}
	// Kept: the evidence node and its own file node.
	if want := int(perEvidence) - 2; n != want {
		t.Errorf("deleted %d nodes, want %d", n, want)
	}
	for _, kv := range [][2]string{{"partition_id", "ev-a-p"}, {"indexed_file_id", "ev-a-f1"}, {"indexed_file_id", "ev-a-f2"}} {
		if hits, _ := repo.graph.NodesByProperty(kv[0], []byte(kv[1])); len(hits) != 0 {
			t.Errorf("%s=%s survived", kv[0], kv[1])
		}
	}
	if hits, _ := repo.graph.NodesByProperty("evidence_file_id", []byte("ev-a")); len(hits) != 1 {
		t.Errorf("the evidence node was not kept: %v", hits)
	}
	if hits, _ := repo.graph.NodesByProperty("level", []byte("evidence_file")); len(hits) != 2 {
		t.Errorf("%d evidence file nodes, want both evidence's", len(hits))
	}
	if got := countNodes(t, repo); got != 2*perEvidence-uint64(n) {
		t.Errorf("%d nodes left, want %d", got, 2*perEvidence-uint64(n))
	}
	edges, err := repo.graph.EdgesByType(repo.kinds().EdgeType(schema.EdgeKindRelation))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 {
		t.Errorf("%d relation edges, want only the other evidence's", len(edges))
	}

	// Without keeping the evidence, nothing of it is left.
	if _, err := repo.DeleteEvidenceIndex("ev-a", false); err != nil {
		t.Fatal(err)
	}
	if got := countNodes(t, repo); got != perEvidence {
		t.Errorf("%d nodes left, want the other evidence's %d", got, perEvidence)
	}
	if hits, _ := repo.graph.NodesByProperty("indexed_file_id", []byte("ev-b-f1")); len(hits) != 1 {
		t.Error("the other evidence's file was deleted")
	}
}
