package graph

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"indicer/internal/diskspace"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/util"

	graphenedb "github.com/aoiflux/graphene"
	graphenedisk "github.com/aoiflux/graphene/disk"
	"github.com/aoiflux/graphene/signing"
	graphstore "github.com/aoiflux/graphene/store"
)

// storeWithNodes writes n tag nodes and returns the data root and their IDs.
func storeWithNodes(t *testing.T, n int) (string, []graphstore.NodeID) {
	t.Helper()
	root := t.TempDir()
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	return root, addNodes(t, h, "a", n)
}

func addNodes(t *testing.T, h *Handle, prefix string, n int) []graphstore.NodeID {
	t.Helper()
	w, err := h.Writer(ActorRef{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	b := w.Batch()
	ids := make([]graphstore.NodeID, n)
	for i := range ids {
		name := prefix + string(rune('0'+i))
		if ids[i], err = b.Upsert(UpsertKey{Key: "probe", Value: name},
			[]graphstore.NodeType{graphstore.NodeTypeTag}, props.Map{"name": name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func nodes(t *testing.T, g *graphenedb.Graph, ids []graphstore.NodeID) []*graphstore.Node {
	t.Helper()
	out := make([]*graphstore.Node, len(ids))
	for i, id := range ids {
		n, err := g.GetNode(id)
		if err != nil {
			t.Fatalf("node %d: %v", id, err)
		}
		out[i] = graphstore.CloneNode(n)
	}
	return out
}

func compactRoot(t *testing.T, root string) CompactReport {
	t.Helper()
	h, err := OpenWith(root, OpenOptions{Stages: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	rep, err := Compact(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// TestNewStoreHasImage: a writable open that makes a store gives it an
// empty image at once, recorded in the marker, so no store DUES makes is a
// log alone. A read-only open makes nothing (TestReadOnlyOpenOfMissingStore
// CreatesNothing).
func TestNewStoreHasImage(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	reopen(t, root)
	dir := util.GraphPath(root)
	info, err := graphenedisk.InspectCSR(filepath.Join(dir, ImageFileName))
	if err != nil {
		t.Fatalf("a new store has no image: %v", err)
	}
	if info.NodeCount != 0 || info.EdgeCount != 0 {
		t.Fatalf("the new image holds %d nodes and %d edges", info.NodeCount, info.EdgeCount)
	}
	m, _, err := schema.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Image == nil || m.Image.Nodes != 0 || m.Image.SegmentSeq != 1 {
		t.Fatalf("marker image = %+v, want an empty image at segment 1", m.Image)
	}
	// A store that already has one is not given another.
	reopen(t, root)
	if seq, _ := logSegmentSeq(dir); seq != 1 {
		t.Fatalf("a second open compacted again: log segment %d", seq)
	}
}

// TestImageRefusedByV011: graphene v0.1, which DUES v0.3.0 links, reads an
// image only when it is GCSR version 2, and reads a log without one as an
// empty graph it then appends to. The image every store now has is another
// version, so that release refuses the store instead.
func TestImageRefusedByV011(t *testing.T) {
	cleanRegistry(t)
	root, _ := storeWithNodes(t, 1)
	head := make([]byte, 6)
	f, err := os.Open(filepath.Join(util.GraphPath(root), ImageFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Read(head); err != nil {
		t.Fatal(err)
	}
	if string(head[:4]) != "GCSR" {
		t.Fatalf("image magic %q", head[:4])
	}
	if v := binary.LittleEndian.Uint16(head[4:6]); v == 2 {
		t.Fatal("the image is version 2, which graphene v0.1 reads")
	}
}

// TestCompactionKeepsIDs: after a compaction, and after a reopen, every node
// is where it was, and a new node takes an ID no node had.
func TestCompactionKeepsIDs(t *testing.T) {
	cleanRegistry(t)
	root, ids := storeWithNodes(t, 5)
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	before := nodes(t, h.Graph(), ids)
	h.Release()

	rep := compactRoot(t, root)
	if rep.Image.Nodes != 5 || rep.LogBytes == 0 || rep.ImageBytes == 0 || rep.Segment != "graphene.000001.wal" {
		t.Fatalf("report = %+v", rep)
	}
	if m, _, _ := schema.ReadMarker(util.GraphPath(root)); m.Image == nil || *m.Image != rep.Image {
		t.Fatalf("marker image %+v, report %+v", m.Image, rep.Image)
	}

	h, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if got := nodes(t, h.Graph(), ids); !reflect.DeepEqual(got, before) {
		t.Fatalf("nodes changed:\n%+v\n%+v", got, before)
	}
	fresh := addNodes(t, h, "b", 1)
	for _, id := range ids {
		if fresh[0] == id {
			t.Fatalf("a new node took ID %d", id)
		}
	}
}

// TestCompactRefusals: a compaction the volume or the memory cannot take,
// or one already cancelled, changes nothing; neither does a read-only
// handle.
func TestCompactRefusals(t *testing.T) {
	cleanRegistry(t)
	root, _ := storeWithNodes(t, 3)
	dir := util.GraphPath(root)
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) context.Context
		want  error
	}{
		{"disk", func(t *testing.T) context.Context {
			prev := checkSpace
			checkSpace = func(path string, need uint64) error { return &diskspace.LowSpaceError{Path: path, Need: need} }
			t.Cleanup(func() { checkSpace = prev })
			return t.Context()
		}, diskspace.ErrLow},
		{"memory", func(t *testing.T) context.Context {
			prev := freeMemory
			freeMemory = func() uint64 { return 1 }
			t.Cleanup(func() { freeMemory = prev })
			return t.Context()
		}, ErrCompactMemory},
		{"cancelled", func(t *testing.T) context.Context {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			return ctx
		}, context.Canceled},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := c.setup(t)
			before := dirSnapshot(t, dir)
			h, err := OpenWith(root, OpenOptions{Stages: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = Compact(ctx, h)
			h.Release()
			if !errors.Is(err, c.want) {
				t.Fatalf("Compact = %v, want %v", err, c.want)
			}
			if dirSnapshot(t, dir) != before {
				t.Fatal("a refused compaction changed the store")
			}
		})
	}
	h, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if _, err := Compact(t.Context(), h); err == nil {
		t.Fatal("a read-only handle compacted")
	}
}

// TestLeftoverImageTempFile: the half-built image a crashed compaction
// leaves is ignored by the open and replaced by the next compaction.
func TestLeftoverImageTempFile(t *testing.T) {
	cleanRegistry(t)
	root, _ := storeWithNodes(t, 2)
	tmp := filepath.Join(util.GraphPath(root), ImageFileName+".tmp")
	if err := os.WriteFile(tmp, []byte("GCSR half an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep := compactRoot(t, root); rep.Image.Nodes != 2 {
		t.Fatalf("report = %+v", rep)
	}
	reopen(t, root)
}

// TestInspect: status reads the files and agrees with what was written: the
// log's commits and records, the image, the segments and their chain.
func TestInspect(t *testing.T) {
	cleanRegistry(t)
	root, _ := storeWithNodes(t, 3)
	compactRoot(t, root)
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	addNodes(t, h, "b", 2)
	h.Release()

	st, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Marker == nil || st.Marker.Image == nil || st.Image == nil || st.Image.NodeCount != 3 {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Segments) != 2 || st.ChainErr != nil || st.ImageErr != nil {
		t.Fatalf("segments %d, chain %v, image %v", len(st.Segments), st.ChainErr, st.ImageErr)
	}
	if st.Log.Segment != 2 || st.Log.Commits != 1 || st.Log.Signed != 0 || st.Log.Loose != 0 || st.Log.Records == 0 {
		t.Fatalf("log = %+v", st.Log)
	}
	if _, err := Inspect(t.TempDir()); !errors.Is(err, ErrNoGraph) {
		t.Fatalf("Inspect of a root without a graph = %v", err)
	}

	// A held writer refuses it.
	h, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	if _, err := Inspect(root); err == nil {
		t.Fatal("Inspect read a store a writer holds")
	}
}

// TestInspectCountsSigned: a commit graphene signs is counted signed, at the
// payload size signedCommitPayload names.
func TestInspectCountsSigned(t *testing.T) {
	cleanRegistry(t)
	root := t.TempDir()
	dir := util.GraphPath(root)
	key, _, err := signing.GenerateKey(7)
	if err != nil {
		t.Fatal(err)
	}
	opts := storeOptions(false)
	opts.Signer = key
	g, err := graphenedb.OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	tx := g.Begin()
	tx.AddNode(&graphstore.Node{Labels: []graphstore.NodeType{graphstore.NodeTypeTag}})
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Inspect(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Log.Commits != 1 || st.Log.Signed != 1 {
		t.Fatalf("log = %+v, want one signed commit", st.Log)
	}
}
