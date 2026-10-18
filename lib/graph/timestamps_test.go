package graph_test

import (
	"testing"
	"time"

	"indicer/lib/enrichment"
	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/propindex"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"
	"indicer/lib/structs"

	graphstore "github.com/aoiflux/graphene/store"
)

func nanos(t time.Time) *int64 { n := t.UnixNano(); return &n }

var (
	fileModified = time.Date(2026, 8, 13, 9, 4, 11, 0, time.UTC)
	fileAccessed = time.Date(2026, 8, 13, 10, 30, 0, 0, time.UTC)
)

func timestampedRecord() enrichment.FileRecord {
	return enrichment.FileRecord{
		Level:            "indexed_file",
		FileName:         "svchost.exe",
		Path:             "/Windows/System32/svchost.exe",
		Size:             55320,
		FileType:         "pe",
		EvidenceFileID:   "ZXZpZGVuY2UtdA==",
		EvidenceFileName: "timed.E01",
		PartitionID:      "cGFydGl0aW9uLXQ=",
		PartitionName:    "p1",
		IndexedFileID:    "indexed-timed-1",
		IndexedFileHash:  "aGFzaC10aW1lZA==",
		// exFAT's shape: modification and access only. Change and birth stay
		// nil because that filesystem does not record them.
		Times: &structs.Timestamps{
			Modified: nanos(fileModified),
			Accessed: nanos(fileAccessed),
		},
	}
}

// TestTimestamps_ReachTheGraph closes the loop the readiness audit opened:
// MACB was parsed by every filesystem driver and discarded before storage. This
// asserts the times now arrive as node properties, and — just as importantly —
// that the ones the filesystem did not record are absent rather than zero.
func TestTimestamps_ReachTheGraph(t *testing.T) {
	root := t.TempDir()

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	repo := enrichment.NewGrapheneRepositoryOn(handle)
	if err := repo.UpsertFile(timestampedRecord()); err != nil {
		t.Fatal(err)
	}

	hits, err := handle.Graph().NodesByProperty("indexed_file_id", []byte("indexed-timed-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 indexed-file node, got %d", len(hits))
	}
	node, err := handle.Graph().GetNode(hits[0])
	if err != nil {
		t.Fatal(err)
	}

	fields, err := props.Unpack(node.Properties)
	if err != nil {
		t.Fatal(err)
	}
	if got := fields.Int64("mtime"); got != fileModified.UnixNano() {
		t.Errorf("mtime = %d, want %d", got, fileModified.UnixNano())
	}
	if got := fields.Int64("atime"); got != fileAccessed.UnixNano() {
		t.Errorf("atime = %d, want %d", got, fileAccessed.UnixNano())
	}
	if fields.Has("ctime") {
		t.Error("ctime is present; a filesystem that records no change time must " +
			"leave it absent rather than assert the epoch")
	}
	if fields.Has("btime") {
		t.Error("btime is present but was never recorded")
	}

	// props.Map.Time reads the stored nanos back as a time.
	if got := fields.Time("mtime"); !got.Equal(fileModified) {
		t.Errorf("Time(\"mtime\") = %v, want %v", got, fileModified)
	}
}

// A timeline window is the query the timestamps exist to serve. The values are
// written with the order-preserving encoder, so a range filter bounds them
// correctly rather than comparing decimal strings.
func TestTimestamps_SupportARangeQuery(t *testing.T) {
	root := t.TempDir()

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if err := enrichment.NewGrapheneRepositoryOn(handle).UpsertFile(timestampedRecord()); err != nil {
		t.Fatal(err)
	}

	// Scoped to indexed-file nodes: a file is modelled at two levels — the
	// indexed-file node and a per-name FILE node — and both legitimately carry
	// the timestamp, exactly as they both carry size and entropy.
	indexedFile := handle.Schema().MatchTypes(schema.KindIndexedFile)

	within := func(from, to time.Time) int {
		t.Helper()
		ids, qerr := handle.Graph().QueryNodeIDs(graphstore.NodeQuery{
			Types: indexedFile,
			Filters: []graphstore.PropertyFilter{{
				Key:        "mtime",
				Op:         graphstore.PropertyOpBetweenInclusive,
				Value:      propindex.Int64(from.UnixNano()),
				ValueUpper: propindex.Int64(to.UnixNano()),
			}},
		})
		if qerr != nil {
			t.Fatal(qerr)
		}
		return len(ids)
	}

	if got := within(fileModified.Add(-time.Hour), fileModified.Add(time.Hour)); got != 1 {
		t.Errorf("a window containing the file matched %d nodes, want 1", got)
	}
	if got := within(fileModified.Add(time.Hour), fileModified.Add(2*time.Hour)); got != 0 {
		t.Errorf("a window after the file matched %d nodes, want 0", got)
	}
	if got := within(fileModified.Add(-2*time.Hour), fileModified.Add(-time.Hour)); got != 0 {
		t.Errorf("a window before the file matched %d nodes, want 0", got)
	}

	// Results alone cannot tell an index lookup from a scan that happened to
	// agree with it, which is what ExplainNodeQuery exists for. mtime is a
	// declared ordered key, so the planner must drive from the ordered index.
	plan, err := handle.Graph().ExplainNodeQuery(graphstore.NodeQuery{
		Filters: []graphstore.PropertyFilter{{
			Key:        "mtime",
			Op:         graphstore.PropertyOpBetweenInclusive,
			Value:      propindex.Int64(fileModified.Add(-time.Hour).UnixNano()),
			ValueUpper: propindex.Int64(fileModified.Add(time.Hour).UnixNano()),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Driver != graphstore.DriverOrdered {
		t.Errorf("timeline query planned as %v, want %v — the ordered key is not "+
			"declared, so every window scans the whole store.\nplan: %s",
			plan.Driver, graphstore.DriverOrdered, plan)
	}
}

// A record with no timestamps must write no time properties at all, so the
// absence stays legible instead of becoming a wall of epoch values.
func TestTimestamps_AbsentTimesWriteNothing(t *testing.T) {
	root := t.TempDir()

	handle, err := duesgraph.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	record := timestampedRecord()
	record.IndexedFileID = "indexed-untimed"
	record.Times = nil

	if err := enrichment.NewGrapheneRepositoryOn(handle).UpsertFile(record); err != nil {
		t.Fatal(err)
	}

	hits, err := handle.Graph().NodesByProperty("indexed_file_id", []byte("indexed-untimed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 node, got %d", len(hits))
	}
	node, err := handle.Graph().GetNode(hits[0])
	if err != nil {
		t.Fatal(err)
	}

	fields, err := props.Unpack(node.Properties)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"mtime", "atime", "ctime", "btime"} {
		if fields.Has(key) {
			t.Errorf("untimed record wrote %q", key)
		}
	}
}
