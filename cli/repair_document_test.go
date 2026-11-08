package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"indicer/internal/console"
	"indicer/internal/testutil"
)

// repairSections are the document's keys that only some runs have.
var repairSections = []string{"upgrade", "quarantined", "revrel", "reindex", "verify", "error"}

// runRepair runs repairData with stdout and stderr captured. stdout must be
// one JSON document, holding the recovery report and exactly the sections
// named in want.
func runRepair(t *testing.T, opts DBOptions, ro RepairOptions, in string, interactive bool, want ...string) (map[string]json.RawMessage, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	console.Configure(&out, &errb)
	err := repairData(t.Context(), opts, ro, strings.NewReader(in), interactive)
	console.Reset()
	var doc map[string]json.RawMessage
	if jerr := json.Unmarshal(out.Bytes(), &doc); jerr != nil {
		t.Fatalf("repair %+v: stdout is not one JSON document (%v):\n%s\nstderr:\n%s", ro, jerr, out.String(), errb.String())
	}
	for _, k := range []string{"totalProblematic", "fixed", "evidence"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("repair %+v: the recovery report has no %q:\n%s", ro, k, out.String())
		}
	}
	for _, k := range repairSections {
		if _, ok := doc[k]; ok != slices.Contains(want, k) {
			t.Errorf("repair %+v: section %q present %v, want %v:\n%s", ro, k, ok, !ok, out.String())
		}
	}
	return doc, errb.String(), err
}

// section decodes one section of a repair document into v.
func section(t *testing.T, doc map[string]json.RawMessage, key string, v any) {
	t.Helper()
	if err := json.Unmarshal(doc[key], v); err != nil {
		t.Fatalf("section %q: %v: %s", key, err, doc[key])
	}
}

// TestRepairDocument: whatever flags repair runs with, its stdout is one JSON
// document: the recovery report with today's fields, and a section for each
// step a flag ran, none for the others. The steps' own lines go to stderr.
func TestRepairDocument(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	p := testutil.WriteRandomFile(t, dir, "ev.bin", 41, 3*matrixChunkKB*1024+7)
	if err := StoreData(t.Context(), opts, p, false, false, false, false); err != nil {
		t.Fatal(err)
	}

	for _, ro := range []RepairOptions{{}, {Fix: true}} {
		if _, _, err := runRepair(t, opts, ro, "", false); err != nil {
			t.Fatalf("repair %+v: %v", ro, err)
		}
	}

	all := RepairOptions{Fix: true, Upgrade: true, AddRevrel: true, ReindexFailed: true, Verify: true}
	doc, stderr, err := runRepair(t, opts, all, "", false, "upgrade", "quarantined", "revrel", "reindex", "verify")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"No legacy block index to upgrade.\n",
		"No index built over raw container bytes.\n",
		"Reverse relations: 1 evidence lacked them; ",
		"Nothing to reindex.\n",
		"Verified 1 evidence; 0 damaged.\n",
	} {
		if !strings.Contains(stderr, line) {
			t.Errorf("stderr has no %q:\n%s", line, stderr)
		}
	}

	var upgrade struct {
		Files               int    `json:"files"`
		RecordedAsContainer bool   `json:"recordedAsContainer"`
		BlockFiles          string `json:"blockFiles"`
		BlockFileCount      int    `json:"blockFileCount"`
	}
	section(t, doc, "upgrade", &upgrade)
	if upgrade.Files != 0 || upgrade.RecordedAsContainer || upgrade.BlockFiles != "none" || upgrade.BlockFileCount != 0 {
		t.Errorf("upgrade with no block index: %+v", upgrade)
	}
	if q := string(doc["quarantined"]); q != "[]" {
		t.Errorf("quarantined %s, want []", q)
	}
	var revrel struct {
		Evidence int   `json:"evidence"`
		Added    int64 `json:"added"`
	}
	section(t, doc, "revrel", &revrel)
	if revrel.Evidence != 1 || revrel.Added == 0 {
		t.Errorf("revrel %+v, want 1 evidence and some added", revrel)
	}
	var reindex struct {
		Evidence           []json.RawMessage `json:"evidence"`
		Incomplete, Failed int
	}
	section(t, doc, "reindex", &reindex)
	if reindex.Evidence == nil || len(reindex.Evidence) != 0 || reindex.Incomplete != 0 || reindex.Failed != 0 {
		t.Errorf("reindex with nothing failed: %s", doc["reindex"])
	}
	verify := verifySection(t, doc)
	if verify.Verified != 1 || verify.Damaged != 0 || len(verify.Results) != 1 || verify.Results[0].Problem != "" {
		t.Errorf("verify: %s", doc["verify"])
	}
}

type verifyDoc struct {
	Verified int `json:"verified"`
	Damaged  int `json:"damaged"`
	Results  []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Problem string `json:"problem"`
	} `json:"results"`
}

func verifySection(t *testing.T, doc map[string]json.RawMessage) verifyDoc {
	t.Helper()
	var v verifyDoc
	section(t, doc, "verify", &v)
	return v
}
