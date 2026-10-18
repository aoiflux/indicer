package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/lib/graph/props"

	"github.com/aoiflux/graphene/disk"
	graphstore "github.com/aoiflux/graphene/store"
)

func commitOne(t *testing.T, h *Handle, fn func(b *Batch) error) {
	t.Helper()
	w := testWriter(t, h)
	b := w.Batch()
	if err := fn(b); err != nil {
		b.Rollback()
		w.Close()
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
}

func storedFields(t *testing.T, h *Handle, id graphstore.NodeID) props.Map {
	t.Helper()
	node, err := h.Graph().GetNode(id)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := props.Unpack(node.Properties)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func holders(t *testing.T, h *Handle, key, value string) []graphstore.NodeID {
	t.Helper()
	ids, err := h.Graph().NodesByProperty(key, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// GRA-2: an upsert of a full record updates a node an earlier stub created,
// and replaces its index entries, so the value it had stops matching. Before,
// the stub was found and returned unchanged, and the extra entries were added
// beside the old ones.
func TestUpsertReplacesStoredNode(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Release() }()
	key := UpsertKey{Key: "evidence_file_id", Value: "e1"}

	var id graphstore.NodeID
	commitOne(t, h, func(b *Batch) (err error) {
		id, err = b.UpsertFill(key, evidenceLabels(h), props.Map{"name": "stub"}, map[string][]byte{"name": []byte("stub")})
		return err
	})
	commitOne(t, h, func(b *Batch) error {
		got, err := b.Upsert(key, evidenceLabels(h), props.Map{"name": "disk.E01", "size": int64(10)}, map[string][]byte{"name": []byte("disk.E01")})
		if got != id {
			t.Errorf("the full record went to node %d, the stub is %d", got, id)
		}
		return err
	})
	fields := storedFields(t, h, id)
	if fields.Str("name") != "disk.E01" || fields.Int64("size") != 10 {
		t.Fatalf("the node kept its stub properties: %v", fields)
	}
	if got := holders(t, h, "name", "disk.E01"); len(got) != 1 || got[0] != id {
		t.Errorf("name=disk.E01 resolves to %v, want [%d]", got, id)
	}
	if got := holders(t, h, "name", "stub"); len(got) != 0 {
		t.Errorf("the stub's name still matches: %v", got)
	}

	// A later full record replaces again; properties it does not name stay.
	commitOne(t, h, func(b *Batch) error {
		_, err := b.Upsert(key, evidenceLabels(h), props.Map{"name": "renamed"}, map[string][]byte{"name": []byte("renamed")})
		return err
	})
	fields = storedFields(t, h, id)
	if fields.Str("name") != "renamed" || fields.Int64("size") != 10 {
		t.Fatalf("after a second record: %v", fields)
	}
	if got := holders(t, h, "name", "disk.E01"); len(got) != 0 {
		t.Errorf("a replaced value still matches: %v", got)
	}

	// The replacement survives a reopen: it is in the log, not only in memory.
	root := filepath.Dir(h.Dir())
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	h, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := holders(t, h, "name", "renamed"); len(got) != 1 || got[0] != id {
		t.Errorf("after a reopen, name=renamed resolves to %v", got)
	}
}

// A stub never overwrites a fuller record.
func TestUpsertFillLeavesExistingNode(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	key := UpsertKey{Key: "partition_id", Value: "p1"}

	var id graphstore.NodeID
	commitOne(t, h, func(b *Batch) (err error) {
		id, err = b.Upsert(key, evidenceLabels(h), props.Map{"name": "full", "size": int64(4096)}, nil)
		return err
	})
	commitOne(t, h, func(b *Batch) error {
		got, err := b.UpsertFill(key, evidenceLabels(h), props.Map{"name": "stub"}, nil)
		if got != id {
			t.Errorf("the stub went to node %d, the record is %d", got, id)
		}
		return err
	})
	if fields := storedFields(t, h, id); fields.Str("name") != "full" || fields.Int64("size") != 4096 {
		t.Fatalf("a stub overwrote the record: %v", fields)
	}
}

// Within one batch, a stub followed by the full record is one node with the
// full record, and its index holds only the final values.
func TestUpsertStubThenRecordInOneBatch(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	key := UpsertKey{Key: "partition_id", Value: "p1"}

	var id graphstore.NodeID
	commitOne(t, h, func(b *Batch) error {
		stub, err := b.UpsertFill(key, evidenceLabels(h), props.Map{"name": "stub"}, map[string][]byte{"name": []byte("stub")})
		if err != nil {
			return err
		}
		id, err = b.Upsert(key, evidenceLabels(h), props.Map{"name": "full"}, map[string][]byte{"name": []byte("full")})
		if id != stub {
			t.Errorf("stub %d and record %d in one batch", stub, id)
		}
		return err
	})
	if fields := storedFields(t, h, id); fields.Str("name") != "full" {
		t.Fatalf("node %d: %v", id, fields)
	}
	if got := holders(t, h, "name", "stub"); len(got) != 0 {
		t.Errorf("the stub's name matches: %v", got)
	}
	if got := holders(t, h, "name", "full"); len(got) != 1 {
		t.Errorf("name=full resolves to %v", got)
	}
}

// GRA-3/GRA-4: index entries are part of the batch commit, so a crash cannot
// leave a node stored but not findable by its key. They used to be written
// after the commit, outside it.
func TestIndexEntriesCommitWithTheBatch(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	commitOne(t, h, func(b *Batch) error {
		src, err := b.Upsert(UpsertKey{Key: "evidence_file_id", Value: "e1"}, evidenceLabels(h), props.Map{"name": "a"}, map[string][]byte{"name": []byte("a")})
		if err != nil {
			return err
		}
		dst, err := b.Add(evidenceLabels(h), props.Map{"name": "b"}, map[string][]byte{"name": []byte("b")})
		if err != nil {
			return err
		}
		return b.EnsureEdge(src, dst, []graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, map[string][]byte{"kind": []byte("x")})
	})
	dir := h.Dir()
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}

	info, err := disk.InspectWAL(filepath.Join(dir, "graphene.wal"))
	if err != nil {
		t.Fatal(err)
	}
	entries := 0
	for _, r := range info.Records {
		if r.TypeName == "node-prop" || r.TypeName == "edge-prop" {
			entries++
			if !r.InBatch {
				t.Errorf("an index entry at offset %d was written outside the batch", r.Offset)
			}
		}
	}
	if entries == 0 {
		t.Fatal("no index entries in the log")
	}
}

// Writing the same record again writes nothing. The graph is never compacted
// (D6), so a re-run that rewrote every node would grow the log forever.
func TestRepeatedUpsertWritesNothing(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	write := func() {
		commitOne(t, h, func(b *Batch) error {
			e, err := b.UpsertFill(UpsertKey{Key: "evidence_file_id", Value: "e1"}, evidenceLabels(h), props.Map{"name": "disk"}, nil)
			if err != nil {
				return err
			}
			p, err := b.Upsert(UpsertKey{Key: "partition_id", Value: "p1"}, evidenceLabels(h),
				props.Map{"name": "p1", "size": int64(512), "tags": []string{"a", "b"}, "nested": map[string]any{"z": 1, "a": "x"}},
				map[string][]byte{"name": []byte("p1")})
			if err != nil {
				return err
			}
			return b.EnsureEdge(e, p, []graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, nil)
		})
	}
	write()
	wal := filepath.Join(h.Dir(), "graphene.wal")
	before, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	write()
	write()
	after, err := os.Stat(wal)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Errorf("writing the same records again grew the log from %d to %d bytes", before.Size(), after.Size())
	}
}

// GRA-10: a node with no labels is refused when it is written, naming it,
// rather than failing the whole commit with graphene's error.
func TestNodesNeedLabels(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	w := testWriter(t, h)
	b := w.Batch()
	defer b.Rollback()
	if _, err := b.Upsert(UpsertKey{Key: "evidence_file_id", Value: "e1"}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "evidence_file_id") {
		t.Errorf("an upsert with no labels: %v, want an error naming the node", err)
	}
	if _, err := b.Add(nil, props.Map{"kind": "url"}, nil); err == nil {
		t.Error("an add with no labels was accepted")
	}
}

// Deleting a node removes it and its edges, and a later upsert of its key
// creates a new node instead of returning the deleted one from the cache.
func TestDeleteNodeForgetsIt(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	key := UpsertKey{Key: "indexed_file_id", Value: "f1"}

	var first, child graphstore.NodeID
	commitOne(t, h, func(b *Batch) (err error) {
		first, err = b.Upsert(key, evidenceLabels(h), props.Map{"name": "f1"}, nil)
		if err != nil {
			return err
		}
		child, err = b.Add([]graphstore.NodeType{graphstore.NodeTypeMicroArtefact}, props.Map{"kind": "url"}, nil)
		if err != nil {
			return err
		}
		return b.EnsureEdge(first, child, []graphstore.EdgeType{graphstore.EdgeTypeContains}, nil, nil)
	})
	commitOne(t, h, func(b *Batch) error {
		b.DeleteNode(first)
		b.DeleteNode(child)
		return nil
	})
	if got := holders(t, h, key.Key, key.Value); len(got) != 0 {
		t.Fatalf("a deleted node is still found by its key: %v", got)
	}
	edges, err := h.Graph().EdgesByType(graphstore.EdgeTypeContains)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Errorf("%d edges of deleted nodes remain", len(edges))
	}

	var second graphstore.NodeID
	commitOne(t, h, func(b *Batch) (err error) {
		second, err = b.Upsert(key, evidenceLabels(h), props.Map{"name": "f1"}, nil)
		return err
	})
	if second == first {
		t.Fatal("the upsert returned the deleted node")
	}
	if got := holders(t, h, key.Key, key.Value); len(got) != 1 || got[0] != second {
		t.Errorf("the key resolves to %v, want [%d]", got, second)
	}
}

// Update merges properties into a node found by ID.
func TestUpdateMergesProperties(t *testing.T) {
	cleanRegistry(t)
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	var id graphstore.NodeID
	commitOne(t, h, func(b *Batch) (err error) {
		id, err = b.Upsert(UpsertKey{Key: "indexed_file_id", Value: "f1"}, evidenceLabels(h), props.Map{"name": "f1"}, nil)
		return err
	})
	commitOne(t, h, func(b *Batch) error {
		return b.Update(id, props.Map{"artefact_count": int64(3)}, map[string][]byte{"artefact_count": []byte("3")})
	})
	fields := storedFields(t, h, id)
	if fields.Str("name") != "f1" || fields.Int64("artefact_count") != 3 {
		t.Fatalf("after Update: %v", fields)
	}
	if got := holders(t, h, "artefact_count", "3"); len(got) != 1 {
		t.Errorf("artefact_count=3 resolves to %v", got)
	}
}
