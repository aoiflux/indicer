package search

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/cnst"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func openEmptyDB(t *testing.T) *badger.DB {
	t.Helper()
	db, err := badger.Open(badger.DefaultOptions(filepath.Join(t.TempDir(), "db")).WithLogger(nil))
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSearchReturnsCanceled(t *testing.T) {
	db := openEmptyDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, db, []string{"ab"}, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSearchValidatesShortQueryFirst(t *testing.T) {
	db := openEmptyDB(t)
	out := filepath.Join(t.TempDir(), "report.json")
	if _, err := SearchReport(t.Context(), db, Request{Query: "a", Out: out}); !errors.Is(err, cnst.ErrSmallQuery) {
		t.Fatalf("expected ErrSmallQuery, got %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused query wrote a report: %v", err)
	}
}

func readReport(t *testing.T, path string) structs.SearchReport {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report structs.SearchReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	return report
}

func TestSearchReportWritesSchemaVersion(t *testing.T) {
	db := openEmptyDB(t)
	out := filepath.Join(t.TempDir(), "custom-report.json")
	if _, err := SearchReport(t.Context(), db, Request{Query: "ab", Out: out}); err != nil {
		t.Fatal(err)
	}
	if r := readReport(t, out); r.SchemaVersion != structs.SearchReportSchemaVersion {
		t.Fatalf("schema version %q, want %q", r.SchemaVersion, structs.SearchReportSchemaVersion)
	}
}

// TestSearchReportRefusesExistingFile (SRC-11): a report never replaces a
// file unless asked to, and nothing is searched when it would.
func TestSearchReportRefusesExistingFile(t *testing.T) {
	db := openEmptyDB(t)
	out := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(out, []byte("earlier work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SearchReport(t.Context(), db, Request{Query: "ab", Out: out}); !errors.Is(err, ErrReportExists) {
		t.Fatalf("search over an existing report: %v, want ErrReportExists", err)
	}
	if body, _ := os.ReadFile(out); string(body) != "earlier work" {
		t.Fatalf("the existing file was changed: %q", body)
	}
	if _, err := SearchReport(t.Context(), db, Request{Query: "ab", Out: out, Force: true}); err != nil {
		t.Fatal(err)
	}
	if r := readReport(t, out); r.SchemaVersion != structs.SearchReportSchemaVersion {
		t.Fatalf("--force did not replace the file: %+v", r)
	}
	dir := t.TempDir()
	if _, err := SearchReport(t.Context(), db, Request{Query: "ab", Out: dir, Force: true}); !errors.Is(err, ErrReportExists) {
		t.Fatalf("a directory as the report path: %v, want ErrReportExists", err)
	}
}

// TestSearchReportNamesObjects (SRC-8): the report names each object from
// its record, so a path holding the name separator is reported whole, and
// the summary counts each level on its own (SRC-9).
func TestSearchReportNamesObjects(t *testing.T) {
	db := openSearchDB(t)
	eid := []byte("search-evidence6")
	content := plantedEvidence()
	storeRaw(t, db, eid, content, structs.IngestStateCompleted)
	const odd = "/dir/a|||b.txt"
	putLegacyIndex(t, db, eid, int64(len(content)), map[string][2]int64{odd: {100, 200}, "/d": {0, 4096}})

	out := filepath.Join(t.TempDir(), "report.json")
	o, err := SearchReport(t.Context(), db, Request{Query: "needle", Out: out})
	if err != nil {
		t.Fatal(err)
	}
	r := readReport(t, out)
	if r.Summary.Evidence != (structs.LevelSummary{Objects: 1, Occurrences: 7}) ||
		r.Summary.Partitions != (structs.LevelSummary{Objects: 1, Occurrences: 7}) ||
		r.Summary.IndexedFiles != (structs.LevelSummary{Objects: 2, Occurrences: 3}) {
		t.Fatalf("summary %+v", r.Summary)
	}
	if r.Coverage.EvidenceSearched != 1 || r.Coverage.PartitionsSearched != 1 || r.Coverage.FilesSearched != 2 || o.Mode != ModeScan {
		t.Fatalf("coverage %+v, mode %s", r.Coverage, o.Mode)
	}
	var found bool
	for _, occ := range r.Occurrences {
		if occ.Level != string(LevelIndexed) || occ.Count != 1 {
			continue
		}
		found = true
		ev := occ.EvidenceFile
		if occ.Path != odd || len(occ.FileNames) != 1 || occ.FileNames[0] != odd ||
			ev == nil || ev.EvidenceFileHash != base64.StdEncoding.EncodeToString(eid) || ev.EvidenceFileNames[0] != "ev.img" ||
			ev.Partition == nil || ev.Partition.PartitionPartNames[0] != "ev.img_p0" ||
			ev.Partition.Indexed == nil || ev.Partition.Indexed.IndexedFileNames[0] != odd {
			body, _ := json.Marshal(occ)
			t.Fatalf("occurrence %s", body)
		}
	}
	if !found {
		t.Fatalf("no occurrence for %s: %+v", odd, r.Occurrences)
	}
}

// TestFullTextModeReportsCoverage (SRC-3): full-text mode searches only the
// indexed files the full-text index offers and says so; a database without
// an index falls back to searching everything, and no index is built.
func TestFullTextModeReportsCoverage(t *testing.T) {
	for _, withFTS := range []bool{true, false} {
		db := openSearchDB(t)
		var path string
		for _, m := range fsfixture.All(t) {
			if m.Name == "fat/fat32_fsagen" {
				path = fsfixture.Open(t, m)
			}
		}
		if _, err := ingest.Run(t.Context(), db, ingest.Request{Path: path, Decode: true, Quiet: true, FTS: withFTS}); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "report.json")
		o, err := SearchReport(t.Context(), db, Request{Query: "deterministic", FullText: true, Out: out})
		if err != nil {
			t.Fatal(err)
		}
		r := readReport(t, out)
		if !withFTS {
			if o.Mode != ModeScan || len(o.Notes) == 0 || !strings.Contains(o.Notes[0], "no full-text index") || r.Coverage.EvidenceSearched != 1 {
				t.Fatalf("without an index: mode %s, notes %q, coverage %+v", o.Mode, o.Notes, r.Coverage)
			}
			if _, err := os.Stat(util.FTSPath(filepath.Dir(db.Opts().Dir))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a full-text search built an index: %v", err)
			}
			continue
		}
		if o.Mode != ModeFullText || r.Mode != ModeFullText || len(o.Notes) == 0 || !strings.Contains(o.Notes[0], "full-text mode") {
			t.Fatalf("with an index: mode %s, notes %q", o.Mode, o.Notes)
		}
		if r.Coverage.EvidenceSearched != 0 || r.Coverage.PartitionsSearched != 0 || r.Coverage.FilesSearched == 0 || r.Coverage.NotCandidates == 0 {
			t.Fatalf("full-text coverage %+v", r.Coverage)
		}
		if r.Summary.Evidence.Objects != 0 || r.Summary.IndexedFiles.Objects == 0 {
			t.Fatalf("full-text summary %+v", r.Summary)
		}
	}
}
