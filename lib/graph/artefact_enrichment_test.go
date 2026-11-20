package graph_test

import (
	"testing"

	"indicer/lib/enrichment"
	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/propindex"
	"indicer/lib/graph/props"
	"indicer/lib/graph/schema"

	graphstore "github.com/aoiflux/graphene/store"
)

const (
	enrEvidenceID  = "ZW5yaWNoLWV2aWRlbmNl"
	enrPartitionID = "ZW5yaWNoLXBhcnRpdGlvbg=="
	enrIndexedID   = "enrich-indexed-1"
)

// nodeProps fetches the single node carrying key=value and returns its
// decoded property blob.
func nodeProps(t *testing.T, handle *duesgraph.Handle, key, value string) props.Map {
	t.Helper()
	hits, err := handle.Graph().NodesByProperty(key, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("%s=%s matched %d nodes, want 1", key, value, len(hits))
	}
	node, err := handle.Graph().GetNode(hits[0])
	if err != nil {
		t.Fatal(err)
	}
	fields, err := props.Unpack(node.Properties)
	if err != nil {
		t.Fatal(err)
	}
	return fields
}

func enrichedStore(t *testing.T) *duesgraph.Handle {
	t.Helper()

	handle, err := duesgraph.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { handle.Release() })

	repo := enrichment.NewGrapheneRepositoryOn(handle)

	if err := repo.UpsertEvidence(enrichment.EvidenceRecord{
		HashBase64:     enrEvidenceID,
		Name:           "disk1.E01",
		Path:           "/cases/2026-114/disk1.E01",
		Size:           8 << 30,
		EvidenceType:   "ewf",
		IngestState:    "completed",
		Completed:      true,
		PartitionCount: 2,
	}); err != nil {
		t.Fatal(err)
	}

	if err := repo.UpsertPartition(enrichment.PartitionRecord{
		EvidenceFileID:   enrEvidenceID,
		EvidenceFileName: "disk1.E01",
		PartitionID:      enrPartitionID,
		PartitionName:    "p1",
		Offset:           1048576,
		Size:             512 << 20,
		FilesystemType:   "ntfs",
		Hash:             "cGFydC1oYXNo",
		IndexedFileCount: 4211,
	}); err != nil {
		t.Fatal(err)
	}

	if err := repo.UpsertFile(enrichment.FileRecord{
		Level:            "indexed_file",
		FileName:         "svchost.exe",
		Path:             "/Windows/System32/svchost.exe",
		Size:             55320,
		FileType:         "pe",
		MimeType:         "application/vnd.microsoft.portable-executable",
		Tags:             []string{"executable"},
		Entropy:          7.81,
		HasEntropy:       true,
		IsFragmented:     true,
		Start:            4096,
		Simhash:          0xDEADBEEFCAFEF00D,
		HasSimhash:       true,
		EvidenceFileID:   enrEvidenceID,
		EvidenceFileName: "disk1.E01",
		PartitionID:      enrPartitionID,
		PartitionName:    "p1",
		IndexedFileID:    enrIndexedID,
		IndexedFileHash:  "c3ZjaG9zdA==",
	}); err != nil {
		t.Fatal(err)
	}

	return handle
}

// Evidence nodes used to carry only id, name, path, size and hash — nothing
// about how the evidence was acquired or whether ingest finished.
func TestArtefactEnrichment_EvidenceCarriesAcquisitionAndIngestState(t *testing.T) {
	t.Parallel()
	handle := enrichedStore(t)
	fields := nodeProps(t, handle, "evidence_file_id", enrEvidenceID)

	if got := fields.Str("evidence_type"); got != "ewf" {
		t.Errorf("evidence_type = %q, want %q", got, "ewf")
	}
	if got := fields.Str("ingest_state"); got != "completed" {
		t.Errorf("ingest_state = %q, want %q", got, "completed")
	}
	if !fields.Bool("completed") {
		t.Error("completed = false, want true")
	}
	if fields.Bool("failed") {
		t.Error("failed = true, want false")
	}
	if got := fields.Int("partition_count"); got != 2 {
		t.Errorf("partition_count = %d, want 2", got)
	}
}

// Partition nodes were the thinnest in the graph: three properties, with their
// location, size, filesystem and hash all parsed at ingest and discarded.
func TestArtefactEnrichment_PartitionCarriesGeometryAndFilesystem(t *testing.T) {
	t.Parallel()
	handle := enrichedStore(t)
	fields := nodeProps(t, handle, "partition_id", enrPartitionID)

	if got := fields.Int64("start"); got != 1048576 {
		t.Errorf("start = %d, want 1048576", got)
	}
	if got := fields.Int64("size"); got != 512<<20 {
		t.Errorf("size = %d, want %d", got, 512<<20)
	}
	if got := fields.Str("filesystem_type"); got != "ntfs" {
		t.Errorf("filesystem_type = %q, want %q", got, "ntfs")
	}
	if got := fields.Str("hash"); got != "cGFydC1oYXNo" {
		t.Errorf("hash = %q, want %q", got, "cGFydC1oYXNo")
	}
	if got := fields.Int("indexed_file_count"); got != 4211 {
		t.Errorf("indexed_file_count = %d, want 4211", got)
	}
}

// The indexed-file node is what a traversal lands on, but its byte offset and
// its deleted/fragmented flags lived only on the separate per-name FILE node.
func TestArtefactEnrichment_IndexedFileCarriesOffsetAndFlags(t *testing.T) {
	t.Parallel()
	handle := enrichedStore(t)
	fields := nodeProps(t, handle, "indexed_file_id", enrIndexedID)

	if got := fields.Int64("start"); got != 4096 {
		t.Errorf("start = %d, want 4096", got)
	}
	if !fields.Bool("is_fragmented") {
		t.Error("is_fragmented = false, want true")
	}
	if fields.Bool("is_deleted") {
		t.Error("is_deleted = true, want false")
	}
	if got := fields.Float64("entropy"); got != 7.81 {
		t.Errorf("entropy = %v, want 7.81", got)
	}
}

// SimHash was computed at ingest and cached in KV, but only lib/near could
// read it, so similarity was invisible to the graph.
func TestArtefactEnrichment_SimhashReachesTheGraph(t *testing.T) {
	t.Parallel()
	handle := enrichedStore(t)
	fields := nodeProps(t, handle, "indexed_file_id", enrIndexedID)

	if !fields.Bool("has_simhash") {
		t.Error("has_simhash = false, want true")
	}
	if got := uint64(fields.Int64("simhash")); got != 0xDEADBEEFCAFEF00D {
		t.Errorf("simhash = %#x, want %#x", got, uint64(0xDEADBEEFCAFEF00D))
	}
}

// A file with no fingerprint must say so rather than claim a zero one — the
// fingerprint is written asynchronously at ingest, so a miss is legitimate.
func TestArtefactEnrichment_AbsentSimhashIsReportedNotZeroed(t *testing.T) {
	t.Parallel()
	handle, err := duesgraph.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Release()

	if err := enrichment.NewGrapheneRepositoryOn(handle).UpsertFile(enrichment.FileRecord{
		Level:            "indexed_file",
		FileName:         "notes.txt",
		Path:             "/notes.txt",
		Size:             1041,
		EvidenceFileID:   enrEvidenceID,
		EvidenceFileName: "disk1.E01",
		PartitionID:      enrPartitionID,
		PartitionName:    "p1",
		IndexedFileID:    "indexed-nosimhash",
	}); err != nil {
		t.Fatal(err)
	}

	fields := nodeProps(t, handle, "indexed_file_id", "indexed-nosimhash")
	if fields.Bool("has_simhash") {
		t.Error("has_simhash = true on a file with no fingerprint")
	}
	if fields.Has("simhash") {
		t.Error("simhash was written for a file that has none; absent and zero " +
			"are different answers")
	}
}

// Size and entropy are now index-registered order-preserving, so the queries
// an examiner actually wants — big files, high-entropy carve candidates —
// resolve through an index rather than a scan.
func TestArtefactEnrichment_SizeAndEntropyAreRangeQueryable(t *testing.T) {
	t.Parallel()
	handle := enrichedStore(t)
	indexed := handle.Schema().MatchTypes(schema.KindIndexedFile)

	ids, err := handle.Graph().QueryNodeIDs(graphstore.NodeQuery{
		Types: indexed,
		Filters: []graphstore.PropertyFilter{{
			Key:        "entropy",
			Op:         graphstore.PropertyOpGreaterThanOrEqual,
			Value:      propindex.Float64(7.5),
			ValueUpper: nil,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Errorf("high-entropy query matched %d nodes, want 1", len(ids))
	}

	plan, err := handle.Graph().ExplainNodeQuery(graphstore.NodeQuery{
		Filters: []graphstore.PropertyFilter{{
			Key:        "size",
			Op:         graphstore.PropertyOpBetweenInclusive,
			Value:      propindex.Int64(1 << 10),
			ValueUpper: propindex.Int64(1 << 30),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Driver != graphstore.DriverOrdered {
		t.Errorf("size range planned as %v, want %v\nplan: %s",
			plan.Driver, graphstore.DriverOrdered, plan)
	}
}
