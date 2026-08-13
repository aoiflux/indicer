//go:build fixturegen

// Regenerates the checked-in legacy-v1 store fixture.
//
//	go test -tags fixturegen -run TestGenerateLegacyV1Fixture ./lib/graph/
//
// This only produces a *legacy* fixture while the module is pinned to graphene
// v0.1.3, so it guards on the custom-type base and refuses to run once the
// module has moved to v0.4.0. Without that guard, running it after the upgrade
// would silently overwrite the v1 fixture with a v2 store and destroy the only
// regression test that catches the vocabulary shift.
//
// The fixture is written through the real repository code paths rather than by
// poking graphene directly, so it carries the exact property blobs, index keys
// and strconv-encoded index values that production stores carry.
package graph_test

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/enrichment"
	"indicer/lib/microartefact"
	"indicer/lib/microartefact/model"

	graphstore "github.com/aoiflux/graphene/store"
)

func TestGenerateLegacyV1Fixture(t *testing.T) {
	// The whole point of the fixture is that it was written by a graphene whose
	// custom band starts at 128. v0.4.0 moved that to 32768.
	if got := graphstore.CustomNodeType(0); uint16(got) != 128 {
		t.Fatalf("refusing to regenerate: CustomNodeType(0) = %d, want 128.\n"+
			"This module is no longer pinned to graphene v0.1.3, so this would write a\n"+
			"v2 fixture over the v1 one and destroy the regression test that catches the\n"+
			"vocabulary shift. Check out the v0.1.3 commit to regenerate.", got)
	}

	dest, err := filepath.Abs(filepath.Join("testdata", "legacy-v1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dest); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	writeLegacyFixture(t, dest)
	t.Logf("wrote legacy-v1 fixture to %s", dest)
}

// writeLegacyFixture builds the fixture dataset. It is deliberately small but
// covers every shape the migration has to survive: all four custom node types,
// contains edges, EdgeType(100) relation edges, entropy and size properties,
// and strconv-encoded ordered-index candidates (start/end/deterministic_flag).
func writeLegacyFixture(t *testing.T, root string) {
	t.Helper()

	enrichRepo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}

	const (
		evidenceID   = "ZXZpZGVuY2UtaGFzaC1kaXNrMQ=="
		evidenceName = "disk1.E01"
	)

	if err := enrichRepo.UpsertEvidence(enrichment.EvidenceRecord{
		HashBase64: evidenceID,
		Name:       evidenceName,
		Path:       "/cases/2026-114/disk1.E01",
		Size:       8 * 1024 * 1024 * 1024,
	}); err != nil {
		t.Fatal(err)
	}

	// An evidence-level file node, so the fixture exercises Level="evidence_file".
	if err := enrichRepo.UpsertFile(enrichment.FileRecord{
		Level:            "evidence_file",
		FileName:         evidenceName,
		Path:             "/cases/2026-114/disk1.E01",
		Size:             8 * 1024 * 1024 * 1024,
		EvidenceFileID:   evidenceID,
		EvidenceFileName: evidenceName,
		Tags:             []string{"acquired", "verified"},
	}); err != nil {
		t.Fatal(err)
	}

	partitions := []struct {
		id   string
		name string
	}{
		{"cGFydGl0aW9uLTE=", "p1"},
		{"cGFydGl0aW9uLTI=", "p2"},
	}
	for _, p := range partitions {
		if err := enrichRepo.UpsertPartition(enrichment.PartitionRecord{
			EvidenceFileID:   evidenceID,
			EvidenceFileName: evidenceName,
			PartitionID:      p.id,
			PartitionName:    p.name,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A partition with no indexed files at all, to pin the empty-partition shape.
	if err := enrichRepo.UpsertFile(enrichment.FileRecord{
		Level:            "partition",
		FileName:         "p2",
		Size:             512 * 1024 * 1024,
		EvidenceFileID:   evidenceID,
		EvidenceFileName: evidenceName,
		PartitionID:      partitions[1].id,
		PartitionName:    partitions[1].name,
	}); err != nil {
		t.Fatal(err)
	}

	type indexedSpec struct {
		path       string
		name       string
		hash       string
		size       int64
		fileType   string
		mimeType   string
		entropy    float64
		hasEntropy bool
		deleted    bool
		fragmented bool
	}
	indexed := []indexedSpec{
		{
			path: "/Users/jane/AppData/Local/Google/Chrome/History", name: "History",
			hash: "aGlzdG9yeS1oYXNo", size: 262144, fileType: "sqlite",
			mimeType: "application/vnd.sqlite3", entropy: 5.42, hasEntropy: true,
		},
		{
			path: "/Windows/System32/svchost.exe", name: "svchost.exe",
			hash: "c3ZjaG9zdC1oYXNo", size: 55320, fileType: "pe",
			mimeType: "application/vnd.microsoft.portable-executable",
			entropy:  7.81, hasEntropy: true, fragmented: true,
		},
		{
			path: "/Users/jane/Documents/deleted-notes.txt", name: "deleted-notes.txt",
			hash: "ZGVsZXRlZC1ub3Rlcw==", size: 1041, fileType: "txt",
			mimeType: "text/plain", deleted: true,
		},
	}

	for _, spec := range indexed {
		indexedFileID := model.BuildIndexedFileID(partitions[0].id, spec.path, spec.hash)
		if err := enrichRepo.UpsertFile(enrichment.FileRecord{
			Level:            "indexed_file",
			FileName:         spec.name,
			Path:             spec.path,
			Size:             spec.size,
			IsDeleted:        spec.deleted,
			IsFragmented:     spec.fragmented,
			FileType:         spec.fileType,
			MimeType:         spec.mimeType,
			Entropy:          spec.entropy,
			HasEntropy:       spec.hasEntropy,
			Tags:             []string{"indexed"},
			EvidenceFileID:   evidenceID,
			EvidenceFileName: evidenceName,
			PartitionID:      partitions[0].id,
			PartitionName:    partitions[0].name,
			IndexedFileID:    indexedFileID,
			IndexedFileHash:  spec.hash,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := enrichRepo.Close(); err != nil {
		t.Fatal(err)
	}

	// Micro-artefacts go through the other repository, which on v0.1.3 means a
	// second handle on the same directory. That is the bug this whole plan
	// fixes; here we simply close one before opening the other.
	microRepo, err := microartefact.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}

	historyID := model.BuildIndexedFileID(partitions[0].id, indexed[0].path, indexed[0].hash)
	artefacts := []model.Artefact{
		{
			Kind: "url", Detector: "browser", Value: "http://malicious.example/payload.exe",
			Summary: "download URL", Confidence: 0.92,
			Span:       model.Span{Start: 1024, End: 1060},
			Attributes: map[string]string{"scheme": "http", "host": "malicious.example"},
		},
		{
			Kind: "domain", Detector: "ioc", Value: "malicious.example",
			Summary: "host extracted from URL", Confidence: 0.88,
			Span:       model.Span{Start: 1031, End: 1048},
			Attributes: map[string]string{"tld": "example"},
		},
		{
			Kind: "ipv4", Detector: "ioc", Value: "203.0.113.42",
			Summary: "resolved address", Confidence: 0.55,
			Span:       model.Span{Start: 2048, End: 2060},
			Attributes: map[string]string{},
		},
	}
	relations := []model.Relation{
		{
			FromKind: "url", FromValue: "http://malicious.example/payload.exe",
			ToKind: "domain", ToValue: "malicious.example",
			RelationType: "resolves_to", Method: "same-identifier",
			Deterministic: true, Confidence: 1.0,
			Evidence: []model.EvidenceField{
				{FieldName: "host", Parser: "urlparse", RawValue: "malicious.example"},
			},
		},
		{
			FromKind: "domain", FromValue: "malicious.example",
			ToKind: "ipv4", ToValue: "203.0.113.42",
			RelationType: "connected_to", Method: "transaction-propagation",
			Deterministic: false, Confidence: 0.61,
			Evidence: []model.EvidenceField{
				{FieldName: "answer", Parser: "dnslog", RawValue: "203.0.113.42"},
			},
		},
	}

	if err := microRepo.Store(microartefact.FileRecord{
		Hash:             indexed[0].hash,
		Name:             indexed[0].name,
		Path:             indexed[0].path,
		Size:             indexed[0].size,
		FileType:         indexed[0].fileType,
		EvidenceFileID:   evidenceID,
		EvidenceFileName: evidenceName,
		PartitionID:      partitions[0].id,
		PartitionName:    partitions[0].name,
		IndexedFileID:    historyID,
	}, artefacts, relations); err != nil {
		t.Fatal(err)
	}

	if err := microRepo.Close(); err != nil {
		t.Fatal(err)
	}
}
