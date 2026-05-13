package enrichment

import (
	"encoding/base64"
	"path/filepath"
	"testing"

	graphenedb "github.com/aoiflux/graphene"
	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"

	"indicer/lib/cnst"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/klauspost/compress/zstd"
)

func TestEnrichAllCreatesFileNodesAtAllLevels(t *testing.T) {
	ensureCompressionCodecs(t)

	root := t.TempDir()
	dbPath := filepath.Join(root, "badger")
	db, err := badger.Open(badger.DefaultOptions(dbPath).WithLogger(nil))
	if err != nil {
		t.Fatalf("badger.Open: %v", err)
	}
	defer db.Close()

	repo, err := OpenGrapheneRepository(root)
	if err != nil {
		t.Fatalf("OpenGrapheneRepository: %v", err)
	}
	defer repo.Close()

	evidenceRaw := []byte("evidence-hash-001")
	partitionRaw := []byte("partition-hash-001")
	indexedRaw := []byte("indexed-hash-001")

	partitionB64 := base64.StdEncoding.EncodeToString(partitionRaw)
	indexedB64 := base64.StdEncoding.EncodeToString(indexedRaw)

	evidence := structs.NewEvidenceFile("seed", 0, 4096, map[string]structs.InternalOffset{
		partitionB64: {Start: 0, End: 1024},
	}, "e01")
	evidence.Completed = true
	evidence.Names = map[string]struct{}{
		"p0|||diskA.E01|||disk-level-a.txt": {},
		"p0|||diskA.E01|||disk-level-b.txt": {},
	}
	evidence.Size = 4096

	partition := structs.NewPartitionFile("seed-part", 0, 2048, map[string]structs.InternalOffset{
		indexedB64: {Start: 0, End: 512},
	})
	partition.Names = map[string]struct{}{
		"p1|||vol1|||part-level-a.log": {},
		"p1|||vol1|||part-level-b.log": {},
	}
	partition.Size = 2048
	partition.IndexedType = "ntfs"
	partition.IsDeleted = false

	indexed := structs.NewIndexedFile("seed-index", 0, 1024, "pe", false)
	indexed.Names = map[string]struct{}{
		"p2|||vol1|||idx-level-a.exe": {},
		"p2|||vol1|||idx-level-b.exe": {},
	}
	indexed.NameMeta = map[string]structs.IndexedNameMeta{
		"p2|||vol1|||idx-level-a.exe": {IsDeleted: false, IsFragmented: false},
		"p2|||vol1|||idx-level-b.exe": {IsDeleted: false, IsFragmented: true},
	}

	evidenceKey := util.AppendToBytesSlice(cnst.EviFileNamespace, evidenceRaw)
	partitionKey := util.AppendToBytesSlice(cnst.PartiFileNamespace, partitionRaw)
	indexedKey := util.AppendToBytesSlice(cnst.IdxFileNamespace, indexedRaw)

	evidencePacked, err := msgpack.Marshal(evidence)
	if err != nil {
		t.Fatalf("msgpack.Marshal evidence: %v", err)
	}
	partitionPacked, err := msgpack.Marshal(partition)
	if err != nil {
		t.Fatalf("msgpack.Marshal partition: %v", err)
	}
	indexedPacked, err := msgpack.Marshal(indexed)
	if err != nil {
		t.Fatalf("msgpack.Marshal indexed: %v", err)
	}

	if err := db.Update(func(txn *badger.Txn) error {
		if err := txn.Set(evidenceKey, evidencePacked); err != nil {
			return err
		}
		if err := txn.Set(partitionKey, partitionPacked); err != nil {
			return err
		}
		if err := txn.Set(indexedKey, indexedPacked); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed badger data: %v", err)
	}

	service := NewService(db, repo)
	if err := service.EnrichAll(); err != nil {
		t.Fatalf("EnrichAll: %v", err)
	}

	hierarchy, err := repo.ReadHierarchy()
	if err != nil {
		t.Fatalf("ReadHierarchy: %v", err)
	}
	if len(hierarchy.EvidenceFiles) != 1 {
		t.Fatalf("expected 1 evidence file, got %d", len(hierarchy.EvidenceFiles))
	}
	if len(hierarchy.EvidenceFiles[0].Partitions) != 1 {
		t.Fatalf("expected 1 partition, got %d", len(hierarchy.EvidenceFiles[0].Partitions))
	}
	if len(hierarchy.EvidenceFiles[0].Partitions[0].Files) != 1 {
		t.Fatalf("expected 1 indexed file node, got %d", len(hierarchy.EvidenceFiles[0].Partitions[0].Files))
	}
	if len(hierarchy.EvidenceFiles[0].Partitions[0].Files[0].FileNames) != 2 {
		t.Fatalf("expected 2 indexed-level file names, got %d", len(hierarchy.EvidenceFiles[0].Partitions[0].Files[0].FileNames))
	}

	if err := repo.Close(); err != nil {
		t.Fatalf("repo.Close: %v", err)
	}

	graph, err := graphenedb.Open(filepath.Join(root, storeDirName))
	if err != nil {
		t.Fatalf("graphenedb.Open: %v", err)
	}
	defer graph.Close()

	diskLevelHits, err := graph.NodesByProperty("level", []byte("disk_image"))
	if err != nil {
		t.Fatalf("NodesByProperty level=disk_image: %v", err)
	}
	if len(diskLevelHits) != 2 {
		t.Fatalf("expected 2 disk_image FILE nodes, got %d", len(diskLevelHits))
	}

	partitionLevelHits, err := graph.NodesByProperty("level", []byte("partition"))
	if err != nil {
		t.Fatalf("NodesByProperty level=partition: %v", err)
	}
	if len(partitionLevelHits) != 2 {
		t.Fatalf("expected 2 partition FILE nodes, got %d", len(partitionLevelHits))
	}

	indexedLevelHits, err := graph.NodesByProperty("level", []byte("indexed_file"))
	if err != nil {
		t.Fatalf("NodesByProperty level=indexed_file: %v", err)
	}
	if len(indexedLevelHits) != 2 {
		t.Fatalf("expected 2 indexed_file FILE nodes, got %d", len(indexedLevelHits))
	}
}

func ensureCompressionCodecs(t *testing.T) {
	t.Helper()

	if cnst.DECODER == nil {
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(0))
		if err != nil {
			t.Fatalf("zstd.NewReader: %v", err)
		}
		cnst.DECODER = decoder
	}

	if cnst.ENCODER == nil {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevel(zstd.SpeedBestCompression)))
		if err != nil {
			t.Fatalf("zstd.NewWriter: %v", err)
		}
		cnst.ENCODER = encoder
	}
}
