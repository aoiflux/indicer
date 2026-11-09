package graph

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"indicer/lib/graph/schema"
	"indicer/lib/util"

	graphenedb "github.com/aoiflux/graphene"
	graphenedisk "github.com/aoiflux/graphene/disk"
	graphstore "github.com/aoiflux/graphene/store"
)

// logOnlyStore makes a store holding one node and no image, as builds
// before the stub image left them, and returns the data root. graphene
// writes the node; a writable open then writes the marker, and leaves the
// store without an image because it is not empty.
func logOnlyStore(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	g, err := graphenedb.OpenWithOptions(util.GraphPath(root), storeOptions(false))
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
	reopen(t, root)
	if _, err := os.Stat(filepath.Join(util.GraphPath(root), ImageFileName)); !os.IsNotExist(err) {
		t.Fatalf("a store holding a node was given an image (%v)", err)
	}
	return root
}

// compactedStore makes a log-only store holding one node and compacts it
// through a handle, so with the handle's own options, and returns the data
// root.
func compactedStore(t *testing.T) string {
	t.Helper()
	root := logOnlyStore(t)
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Graph().Compact(); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCompactionKeepsItsSegment: a compaction through a handle keeps the
// log it retires, as graphene.000000.wal, and numbers the new log 1 in its
// header, at the offset logSegmentSeq reads. graphene's own listing agrees.
func TestCompactionKeepsItsSegment(t *testing.T) {
	cleanRegistry(t)
	dir := util.GraphPath(compactedStore(t))

	segs, err := graphenedisk.ListSegments(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) != 1 || segs[0].Sequence != 0 || filepath.Base(segs[0].Path) != "graphene.000000.wal" {
		t.Fatalf("segments = %+v, want graphene.000000.wal alone", segs)
	}
	header, err := os.ReadFile(filepath.Join(dir, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(header) < logHeaderSize || string(header[:4]) != "GWAL" ||
		binary.LittleEndian.Uint32(header[46:50]) != crc32.ChecksumIEEE(header[:46]) {
		t.Fatalf("the log has no GWAL header: % x", header[:min(len(header), logHeaderSize)])
	}
	if got := binary.LittleEndian.Uint64(header[6:14]); got != segs[0].Sequence+1 {
		t.Fatalf("header bytes 6-14 hold %d, want %d", got, segs[0].Sequence+1)
	}
	if seq, err := logSegmentSeq(dir); err != nil || seq != 1 {
		t.Fatalf("logSegmentSeq = %d, %v; want 1", seq, err)
	}
}

// TestImageIsRecordedInTheMarker: the writable open after a compaction
// records the image in the marker, as a crash between graphene's commit and
// the marker write would also leave it; a read-only open records nothing.
func TestImageIsRecordedInTheMarker(t *testing.T) {
	cleanRegistry(t)
	root := compactedStore(t)
	dir := util.GraphPath(root)
	m, _, err := schema.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Image != nil {
		// The compaction ran on an open handle, so nothing recorded it yet.
		t.Fatalf("image recorded before a reopen: %+v", m.Image)
	}

	ro, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Release(); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := schema.ReadMarker(dir); m.Image != nil {
		t.Fatal("a read-only open recorded the image")
	}

	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	m, _, err = schema.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Image == nil || m.Image.Nodes != 1 || m.Image.SegmentSeq != 1 || m.Image.CSRVersion == 0 || m.Image.CompactedAt.IsZero() {
		t.Fatalf("image = %+v, want 1 node at segment 1", m.Image)
	}
}

// TestMissingImageRefused: a store something says was compacted, and whose
// image is gone, is refused by every open; each sign is enough alone. A
// store nothing says was compacted opens without one.
func TestMissingImageRefused(t *testing.T) {
	cleanRegistry(t)
	stripMarkerImage := func(t *testing.T, dir string) {
		t.Helper()
		m, found, err := schema.ReadMarker(dir)
		if err != nil || !found {
			t.Fatalf("marker: found %v, %v", found, err)
		}
		m.Image = nil
		if err := schema.WriteMarker(dir, m); err != nil {
			t.Fatal(err)
		}
	}
	removeSegments := func(t *testing.T, dir string) {
		t.Helper()
		segs, err := graphenedisk.ListSegments(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range segs {
			if err := os.Remove(s.Path); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range []struct {
		name    string
		make    func(t *testing.T) string
		refused bool
	}{
		{"all signs", func(t *testing.T) string {
			root := compactedStore(t)
			reopen(t, root)
			return root
		}, true},
		{"marker", func(t *testing.T) string {
			root := logOnlyStore(t)
			dir := util.GraphPath(root)
			m, _, err := schema.ReadMarker(dir)
			if err != nil {
				t.Fatal(err)
			}
			m.Image = &schema.ImageRecord{CSRVersion: 9}
			if err := schema.WriteMarker(dir, m); err != nil {
				t.Fatal(err)
			}
			return root
		}, true},
		{"log header", func(t *testing.T) string {
			root := compactedStore(t)
			reopen(t, root)
			removeSegments(t, util.GraphPath(root))
			stripMarkerImage(t, util.GraphPath(root))
			return root
		}, true},
		{"segment", func(t *testing.T) string {
			root := logOnlyStore(t)
			if err := os.WriteFile(filepath.Join(util.GraphPath(root), "graphene.000003.wal"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return root
		}, true},
		{"none", logOnlyStore, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := c.make(t)
			dir := util.GraphPath(root)
			if err := os.Remove(filepath.Join(dir, ImageFileName)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			before := dirSnapshot(t, dir)
			for _, ro := range []bool{true, false} {
				h, err := OpenWith(root, OpenOptions{ReadOnly: ro})
				if !c.refused {
					if err != nil {
						t.Fatalf("read-only %v: %v", ro, err)
					}
					_ = h.Release()
					continue
				}
				if err == nil {
					_ = h.Release()
					t.Fatalf("read-only %v: opened a store whose image is missing", ro)
				}
				if !errors.Is(err, ErrImageMissing) || !strings.Contains(err.Error(), "dues enrich") {
					t.Fatalf("read-only %v: %v", ro, err)
				}
			}
			if c.refused && dirSnapshot(t, dir) != before {
				t.Fatal("a refused open changed the store")
			}
		})
	}
}

func reopen(t *testing.T, root string) {
	t.Helper()
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestIsSegmentName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"graphene.000000.wal": true,
		"graphene.12.wal":     true,
		"graphene.wal":        false,
		"graphene..wal":       false,
		"graphene.00a0.wal":   false,
		"graphene.000000.csr": false,
		"dues.000000.wal":     false,
	} {
		if got := isSegmentName(name); got != want {
			t.Errorf("isSegmentName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestV2MarkerIsRestampedV3: a writable open raises a v2 marker to the
// current version, recording what it came from and keeping what the v2
// marker declared; a read-only open leaves it.
func TestV2MarkerIsRestampedV3(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)
	dir := util.GraphPath(root)
	m, _, err := schema.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion, m.MigratedFrom = 2, nil
	m.OrderedNodeKeys = []string{"size"}
	if err := schema.WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenWith(root, OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Release(); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := schema.ReadMarker(dir); got.SchemaVersion != 2 {
		t.Fatalf("a read-only open restamped the marker to %d", got.SchemaVersion)
	}

	reopen(t, root)
	got, _, err := schema.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 3 || got.MigratedFrom == nil || *got.MigratedFrom != 2 || !reflect.DeepEqual(got.OrderedNodeKeys, []string{"size"}) {
		t.Fatalf("marker = version %d, migrated from %v, ordered keys %v", got.SchemaVersion, got.MigratedFrom, got.OrderedNodeKeys)
	}

	// Recording an image restamps a current marker without claiming a
	// migration.
	h, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Graph().Compact(); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	reopen(t, root)
	got, _, _ = schema.ReadMarker(dir)
	if got.Image == nil || got.Image.Nodes != 1 || got.MigratedFrom == nil || *got.MigratedFrom != 2 {
		t.Fatalf("after recording the image: image %+v, migrated from %v", got.Image, got.MigratedFrom)
	}
}

// TestNewerMarkerIsRefused: a marker from a newer schema is refused by
// every open, and left as it was.
func TestNewerMarkerIsRefused(t *testing.T) {
	cleanRegistry(t)
	root := storeWithNode(t)
	dir := util.GraphPath(root)
	m, _, err := schema.ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = schema.CurrentVersion + 1
	if err := schema.WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
	for _, ro := range []bool{true, false} {
		h, err := OpenWith(root, OpenOptions{ReadOnly: ro})
		if err == nil {
			_ = h.Release()
			t.Fatalf("read-only %v: opened a store at schema version %d", ro, m.SchemaVersion)
		}
		if !strings.Contains(err.Error(), "upgrade dues") {
			t.Fatalf("read-only %v: %v", ro, err)
		}
	}
	if got, _, _ := schema.ReadMarker(dir); got.SchemaVersion != schema.CurrentVersion+1 {
		t.Fatalf("a refused open rewrote the marker to %d", got.SchemaVersion)
	}
}

// TestStoreOptionsAreGraphenes: the options every open passes are
// graphene's Open and OpenReadOnly options with every segment kept, and a
// store reads the same through them.
func TestStoreOptionsAreGraphenes(t *testing.T) {
	cleanRegistry(t)
	keep := graphenedisk.RetentionPolicy{MaxSegments: 1<<31 - 1}
	if got, want := storeOptions(false), (graphenedisk.Options{Retention: keep}); !reflect.DeepEqual(got, want) {
		t.Errorf("writable options = %+v, want %+v", got, want)
	}
	readOnly := graphenedisk.Options{ReadOnly: true, Constraints: graphenedisk.ConstraintDrop, Retention: keep}
	if got := storeOptions(true); !reflect.DeepEqual(got, readOnly) {
		t.Errorf("read-only options = %+v, want %+v", got, readOnly)
	}

	dir := util.GraphPath(storeWithNode(t))
	count := func(open func(string) (*graphenedb.Graph, error)) uint64 {
		t.Helper()
		g, err := open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		n, err := g.NodeCount()
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	ours := count(func(d string) (*graphenedb.Graph, error) { return openStore(d, false, nil) })
	oursRO := count(func(d string) (*graphenedb.Graph, error) { return openStore(d, true, nil) })
	theirs := count(graphenedb.Open)
	theirsRO := count(graphenedb.OpenReadOnly)
	if ours != theirs || oursRO != theirsRO || ours != oursRO || ours == 0 {
		t.Fatalf("node counts: ours %d/%d, graphene's %d/%d", ours, oursRO, theirs, theirsRO)
	}
}
