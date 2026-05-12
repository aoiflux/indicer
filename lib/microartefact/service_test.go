package microartefact

import (
	"indicer/lib/enrichment"
	"path/filepath"
	"testing"

	specpkg "indicer/lib/microartefact/spec"

	"github.com/aoiflux/graphene"
	graphstore "github.com/aoiflux/graphene/store"
	"github.com/vmihailenco/msgpack/v5"
)

type captureRepository struct {
	file      FileRecord
	artefacts []Artefact
	relations []Relation
	called    bool
}

func (r *captureRepository) Store(file FileRecord, artefacts []Artefact, relations []Relation) error {
	r.file = file
	r.artefacts = artefacts
	r.relations = relations
	r.called = true
	return nil
}

func (r *captureRepository) Close() error {
	return nil
}

func TestServiceProcessDetectsSimpleArtefacts(t *testing.T) {
	repo := &captureRepository{}
	service := NewService(repo)

	content := []byte("api_key=secret-value\nhttps://example.org/download\x00visible printable string")
	file := FileRecord{Hash: "hash-1", Name: "sample.txt", Path: "sample.txt", Size: int64(len(content))}

	if err := service.Process(file, content); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	if !repo.called {
		t.Fatal("repository was not called")
	}

	counts := map[string]int{}
	for _, artefact := range repo.artefacts {
		counts[artefact.Kind]++
	}
	if counts["key_value"] == 0 {
		t.Fatalf("expected key_value artefact, got %#v", repo.artefacts)
	}
	if counts["url"] == 0 {
		t.Fatalf("expected url artefact, got %#v", repo.artefacts)
	}
	if repo.file.Hash != file.Hash {
		t.Fatalf("stored file hash mismatch: got %s want %s", repo.file.Hash, file.Hash)
	}
}

func TestServiceSkipsUnsupportedFileType(t *testing.T) {
	repo := &captureRepository{}
	service := NewService(repo)

	content := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00}
	file := FileRecord{Hash: "hash-jpg", Name: "image.jpg", Path: "image.jpg", Size: int64(len(content))}

	if err := service.Process(file, content); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}
	if repo.called {
		t.Fatalf("expected unsupported file type to be skipped without repository writes")
	}
}

func TestServiceWithSpecCanDisableDetectionMethod(t *testing.T) {
	repo := &captureRepository{}
	sheet := specpkg.Default()
	sheet.RemoveDetectionRule("text.url")

	service := NewServiceWithSpec(repo, sheet)
	content := []byte("api_key=secret-value\nhttps://example.org/download")
	file := FileRecord{Hash: "hash-3", Name: "sample.txt", Path: "sample.txt", Size: int64(len(content))}

	if err := service.Process(file, content); err != nil {
		t.Fatalf("Process returned error: %v", err)
	}

	hasURL := false
	hasKV := false
	for _, artefact := range repo.artefacts {
		if artefact.Kind == "url" {
			hasURL = true
		}
		if artefact.Kind == "key_value" {
			hasKV = true
		}
	}
	if hasURL {
		t.Fatalf("expected url artefact to be disabled by spec, got %#v", repo.artefacts)
	}
	if !hasKV {
		t.Fatalf("expected key_value artefact to remain enabled, got %#v", repo.artefacts)
	}
}

func TestGrapheneRepositoryStoresNodesAndEdges(t *testing.T) {
	root := t.TempDir()
	enrichmentRepo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatalf("enrichment.OpenGrapheneRepository: %v", err)
	}
	if err := enrichmentRepo.UpsertFile(enrichment.FileRecord{
		Level:           "indexed_file",
		FileName:        "evidence.bin",
		Path:            filepath.Join(root, "evidence.bin"),
		Size:            128,
		DiskImageID:     "disk-image-001",
		DiskImageName:   "evidence.E01",
		PartitionID:     "partition-001",
		PartitionName:   "vol0",
		IndexedFileID:   "hash-2",
		IndexedFileHash: "hash-2",
	}); err != nil {
		t.Fatalf("enrichment.UpsertFile: %v", err)
	}
	if err := enrichmentRepo.Close(); err != nil {
		t.Fatalf("enrichment.Close: %v", err)
	}

	repo, err := OpenGrapheneRepository(root)
	if err != nil {
		t.Fatalf("OpenGrapheneRepository: %v", err)
	}

	file := FileRecord{
		Hash:          "hash-2",
		Name:          "evidence.bin",
		Path:          filepath.Join(root, "evidence.bin"),
		Size:          128,
		ELFMeta:       &ELFMetadata{Class: 64, SectionCount: 4, NamedSymbolCount: 3},
		DiskImageID:   "disk-image-001",
		DiskImageName: "evidence.E01",
		PartitionID:   "partition-001",
		PartitionName: "vol0",
		IndexedFileID: "hash-2",
	}
	artefacts := []Artefact{
		{Kind: "url", Detector: "url-pattern", Value: "https://example.org", Summary: "https://example.org", Confidence: 0.9, Span: Span{Start: 4, End: 23}},
		{Kind: "key_value", Detector: "key-value", Value: "user=admin", Summary: "user=admin", Confidence: 0.8, Span: Span{Start: 24, End: 34}},
	}
	relations := []Relation{
		{
			FromKind:      "url",
			FromValue:     "https://example.org",
			ToKind:        "key_value",
			ToValue:       "user=admin",
			RelationType:  "referenced",
			Method:        "test.manual",
			Deterministic: true,
			Confidence:    1,
		},
	}

	if err := repo.Store(file, artefacts, relations); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	graph, err := graphene.Open(filepath.Join(root, grapheneStoreDir))
	if err != nil {
		t.Fatalf("graphene.Open: %v", err)
	}
	defer graph.Close()

	// Check for FILE node with "evidence.bin" name at indexed_file level
	fileHits, err := graph.NodesByProperty("name", []byte("evidence.bin"))
	if err != nil {
		t.Fatalf("NodesByProperty name: %v", err)
	}
	if len(fileHits) == 0 {
		t.Fatalf("expected file node with name 'evidence.bin', got %d nodes", len(fileHits))
	}

	artefactHits, err := graph.NodesByType(graphstore.NodeTypeMicroArtefact)
	if err != nil {
		t.Fatalf("NodesByType microartefact: %v", err)
	}
	if len(artefactHits) != 2 {
		t.Fatalf("expected 2 microartefact nodes, got %d", len(artefactHits))
	}

	edgeHits, err := graph.EdgesByType(graphstore.EdgeTypeContains)
	if err != nil {
		t.Fatalf("EdgesByType contains: %v", err)
	}
	if len(edgeHits) != 5 {
		t.Fatalf("expected 5 contains edges (disk->partition->indexed_file->file + 2 artefacts), got %d", len(edgeHits))
	}

	relationEdgeHits, err := graph.EdgesByType(graphstore.EdgeType(100))
	if err != nil {
		t.Fatalf("EdgesByType relation: %v", err)
	}
	if len(relationEdgeHits) != 1 {
		t.Fatalf("expected 1 relation edge, got %d", len(relationEdgeHits))
	}

	indexedHits, err := graph.NodesByProperty("indexed_file_id", []byte(file.Hash))
	if err != nil {
		t.Fatalf("NodesByProperty indexed_file_id: %v", err)
	}
	if len(indexedHits) != 1 {
		t.Fatalf("expected 1 indexed_file node hit, got %d", len(indexedHits))
	}
	indexedNode, err := graph.GetNode(indexedHits[0])
	if err != nil {
		t.Fatalf("GetNode indexed_file: %v", err)
	}
	props := map[string]any{}
	if err := msgpack.Unmarshal(indexedNode.Properties, &props); err != nil {
		t.Fatalf("msgpack.Unmarshal indexed node properties: %v", err)
	}
	if gotHash, _ := props["hash"].(string); gotHash != file.Hash {
		t.Fatalf("expected indexed_file node hash=%q, got %#v", file.Hash, props["hash"])
	}

	if kindHits, err := graph.NodesByProperty("artefact_kind", []byte("url")); err != nil {
		t.Fatalf("NodesByProperty artefact_kind: %v", err)
	} else if len(kindHits) != 1 {
		t.Fatalf("expected 1 url artefact hit, got %d", len(kindHits))
	}
}

func asInt(value any) int {
	switch num := value.(type) {
	case int:
		return num
	case int8:
		return int(num)
	case int16:
		return int(num)
	case int32:
		return int(num)
	case int64:
		return int(num)
	case uint:
		return int(num)
	case uint8:
		return int(num)
	case uint16:
		return int(num)
	case uint32:
		return int(num)
	case uint64:
		return int(num)
	case float32:
		return int(num)
	case float64:
		return int(num)
	default:
		return 0
	}
}
