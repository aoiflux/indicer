package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil/testdb"
)

// TestNearCommand (SRC-12): near on evidence stored without reverse
// relations fails with the command that adds them; repair --fix --revrel
// adds them, and near then runs; an existing report is a usage error.
func TestNearCommand(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	_, image := fixtureNamed(t, "fat/fat12")
	dbPath := filepath.Join(t.TempDir(), "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	if err := StoreData(t.Context(), opts, image, false, false, false, false); err != nil {
		t.Fatal(err)
	}
	_, rec := onlyEvidence(t, dbPath)
	out := filepath.Join(t.TempDir(), "near.json")
	err := NearInData(t.Context(), opts, rec.FileHash, NearOptions{Out: out})
	if err == nil || !strings.Contains(err.Error(), "repair --fix --revrel") || lifecycle.ExitCode(err) != 1 {
		t.Fatalf("near without reverse relations: %v (exit %d)", err, lifecycle.ExitCode(err))
	}

	var usage *lifecycle.UsageError
	if err := RepairData(t.Context(), opts, RepairOptions{AddRevrel: true}); !errors.As(err, &usage) {
		t.Fatalf("repair --revrel without --fix: %v, want a usage error", err)
	}
	if err := repairData(t.Context(), opts, RepairOptions{Fix: true, AddRevrel: true}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := NearInData(t.Context(), opts, rec.FileHash, NearOptions{Out: out}); err != nil {
		t.Fatal(err)
	}
	if err := NearInData(t.Context(), opts, rec.FileHash, NearOptions{Out: out}); !errors.As(err, &usage) {
		t.Fatalf("near over an existing report: %v, want a usage error", err)
	}
	if err := NearOutData(t.Context(), opts, image, NearOptions{Out: out, Force: true}); err != nil {
		t.Fatal(err)
	}
}
