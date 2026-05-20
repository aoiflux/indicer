package search

import (
	"bytes"
	"context"
	"encoding/json"
	"indicer/lib/cnst"
	"indicer/lib/structs"
	"os"
	"path/filepath"
	"testing"

	"github.com/dgraph-io/badger/v4"
)

func TestSearchWithContextReturnsCanceled(t *testing.T) {
	dir := t.TempDir()
	opts := badger.DefaultOptions(dir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = SearchWithContext(ctx, "ab", db)
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSearchWithContextValidatesShortQueryFirst(t *testing.T) {
	dir := t.TempDir()
	opts := badger.DefaultOptions(dir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	defer db.Close()

	err = SearchWithContext(context.Background(), "a", db)
	if err != cnst.ErrSmallQuery {
		t.Fatalf("expected ErrSmallQuery, got %v", err)
	}
}

func TestSearchToPathWithContextWritesCustomReportPath(t *testing.T) {
	dir := t.TempDir()
	opts := badger.DefaultOptions(filepath.Join(dir, "db")).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	defer db.Close()

	reportPath := filepath.Join(dir, "custom-report.json")
	err = SearchToPathWithContext(context.Background(), "ab", reportPath, db)
	if err != nil {
		t.Fatalf("SearchToPathWithContext returned error: %v", err)
	}

	info, err := os.Stat(reportPath)
	if err != nil {
		t.Fatalf("stat report path: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("expected report file to be non-empty")
	}
}

func TestNewSearchSettingsNormalizesQueryBytes(t *testing.T) {
	settings := newSearchSettings("AbC")
	if !bytes.Equal(settings.queryBytes, []byte("abc")) {
		t.Fatalf("expected lower-cased query bytes, got %q", settings.queryBytes)
	}
}

func TestSubBytesChonkCountsASCIICaseInsensitiveMatchesWithoutOverlap(t *testing.T) {
	count := subBytesChonk([]byte("ab"), []byte("xxABabAbx"))
	if count != 3 {
		t.Fatalf("expected 3 matches, got %d", count)
	}
}

func TestArtefactHashFromSearchIDRejectsMalformedID(t *testing.T) {
	_, err := artefactHashFromSearchID("invalid-id")
	if err == nil {
		t.Fatal("expected malformed ID error, got nil")
	}
}

func TestSearchToPathWithContextWritesSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	opts := badger.DefaultOptions(filepath.Join(dir, "db")).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	defer db.Close()

	reportPath := filepath.Join(dir, "schema-report.json")
	err = SearchToPathWithContext(context.Background(), "ab", reportPath, db)
	if err != nil {
		t.Fatalf("SearchToPathWithContext returned error: %v", err)
	}

	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}

	var report structs.SearchReport
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if report.SchemaVersion != structs.SearchReportSchemaVersion {
		t.Fatalf("expected schema version %q, got %q", structs.SearchReportSchemaVersion, report.SchemaVersion)
	}
}
