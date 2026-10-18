package enrichment

import (
	"errors"
	"testing"
)

// GRA-10: a write through a repository that is not open is an error. It used
// to succeed and write nothing.
func TestClosedRepositoryRefusesWrites(t *testing.T) {
	repo, err := OpenGrapheneRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{
		"evidence":  repo.UpsertEvidence(EvidenceRecord{HashBase64: "e"}),
		"partition": repo.UpsertPartition(PartitionRecord{EvidenceFileID: "e", PartitionID: "p"}),
		"file":      repo.UpsertFile(FileRecord{EvidenceFileID: "e", Level: "evidence_file"}),
		"files":     repo.UpsertFiles([]FileRecord{{EvidenceFileID: "e", Level: "evidence_file"}}),
	} {
		if !errors.Is(err, ErrRepositoryClosed) {
			t.Errorf("%s written through a closed repository: %v", name, err)
		}
	}
	if _, err := repo.ReadHierarchy(); !errors.Is(err, ErrRepositoryClosed) {
		t.Errorf("read through a closed repository: %v", err)
	}
}
