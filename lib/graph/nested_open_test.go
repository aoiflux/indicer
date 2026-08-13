package graph_test

import (
	"testing"

	"indicer/lib/enrichment"
	"indicer/lib/microartefact"
	"indicer/lib/microartefact/model"
)

const (
	nestedEvidenceID   = "ZXZpZGVuY2UtbmVzdGVk"
	nestedEvidenceName = "nested.E01"
	nestedPartitionID  = "cGFydGl0aW9uLW5lc3RlZA=="
	nestedPath         = "/Windows/System32/config/SOFTWARE"
	nestedHash         = "c29mdHdhcmUtaGFzaA=="
)

func indexedFileRecord(root string) enrichment.FileRecord {
	return enrichment.FileRecord{
		Level:            "indexed_file",
		FileName:         "SOFTWARE",
		Path:             nestedPath,
		Size:             98304,
		FileType:         "registry",
		EvidenceFileID:   nestedEvidenceID,
		EvidenceFileName: nestedEvidenceName,
		PartitionID:      nestedPartitionID,
		PartitionName:    "p1",
		IndexedFileID:    model.BuildIndexedFileID(nestedPartitionID, nestedPath, nestedHash),
		IndexedFileHash:  nestedHash,
	}
}

// TestNestedOpen_WriteThroughInnerRepoIsVisibleToOuter reproduces the shape that
// made this work necessary.
//
// cli/cmdmicro.go opens the micro-artefact repository at line 70 and holds it
// for the whole command. Inside that window, ensureIndexedFileNodes at line 306
// opens an enrichment repository on the same directory and runs the backfill
// through it, then the caller immediately re-checks HasIndexedFile.
//
// On a per-repository store that check reads a different in-memory delta
// overlay from the one the backfill wrote into, so it can fail even though the
// nodes were written — and both handles are appending to one WAL, which is the
// corruption the file lock and the registry exist to prevent. With a shared
// handle the write is simply visible.
func TestNestedOpen_WriteThroughInnerRepoIsVisibleToOuter(t *testing.T) {
	root := t.TempDir()
	record := indexedFileRecord(root)

	// Outer: the handle a command holds for its whole run.
	microRepo, err := microartefact.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer microRepo.Close()

	exists, err := microRepo.HasIndexedFile(record.IndexedFileID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("indexed file present before the backfill ran")
	}

	// Inner: the backfill, opened while the outer handle is still held.
	enrichRepo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := enrichRepo.UpsertFile(record); err != nil {
		t.Fatal(err)
	}
	if err := enrichRepo.Close(); err != nil {
		t.Fatal(err)
	}

	exists, err = microRepo.HasIndexedFile(record.IndexedFileID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("the outer repository cannot see a node the inner one wrote; " +
			"the two are not sharing a store")
	}
}

// The inner repository closing must not close the store out from under the
// outer one, which is what a naive de-duplication would do.
func TestNestedOpen_InnerCloseLeavesOuterUsable(t *testing.T) {
	root := t.TempDir()

	microRepo, err := microartefact.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer microRepo.Close()

	enrichRepo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := enrichRepo.Close(); err != nil {
		t.Fatal(err)
	}

	// Any call that reaches the store proves it is still open.
	if _, err := microRepo.HasIndexedFile("whatever"); err != nil {
		t.Fatalf("outer repository is unusable after the inner one closed: %v", err)
	}
}

// The full ensureIndexedFileNodes sequence: backfill through the inner
// repository, then store artefacts through the outer one. Store() hard-fails if
// it cannot find the indexed-file node, so this fails loudly on a split store.
func TestNestedOpen_ArtefactStoreAfterBackfill(t *testing.T) {
	root := t.TempDir()
	record := indexedFileRecord(root)

	microRepo, err := microartefact.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer microRepo.Close()

	enrichRepo, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := enrichRepo.UpsertFile(record); err != nil {
		t.Fatal(err)
	}
	if err := enrichRepo.Close(); err != nil {
		t.Fatal(err)
	}

	err = microRepo.Store(microartefact.FileRecord{
		Hash:             nestedHash,
		Name:             "SOFTWARE",
		Path:             nestedPath,
		Size:             98304,
		FileType:         "registry",
		EvidenceFileID:   nestedEvidenceID,
		EvidenceFileName: nestedEvidenceName,
		PartitionID:      nestedPartitionID,
		PartitionName:    "p1",
		IndexedFileID:    record.IndexedFileID,
	}, []model.Artefact{{
		Kind: "registry_key", Detector: "registry",
		Value:      `HKLM\Software\Microsoft\Windows\CurrentVersion\Run`,
		Summary:    "autorun key",
		Confidence: 0.9,
		Span:       model.Span{Start: 4096, End: 4160},
	}}, nil)
	if err != nil {
		t.Fatalf("storing artefacts after a nested backfill: %v", err)
	}

	hasArtefacts, err := microRepo.HasMicroArtefacts(record.IndexedFileID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasArtefacts {
		t.Error("artefacts were stored but are not visible through the same repository")
	}
}
