package cli

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/lib/util"
)

// TestRestoreLogicalRangeAcrossChunkBoundaries is the RST-1 regression. A
// partition or indexed file is a byte range of its evidence, and restore
// trims each chunk to that range. The old trim sliced the first chunk from
// the range start but took the range size, not what was left of it, from
// the second, so a small range straddling a chunk boundary came back too
// long; a range reaching past the evidence read past the last chunk.
func TestRestoreLogicalRangeAcrossChunkBoundaries(t *testing.T) {
	t.Parallel()
	cs := int64(matrixChunkKB * 1024)
	evidenceSize := 3*cs + 17

	ranges := []struct {
		name        string
		start, size int64
	}{
		{"straddles_one_boundary", cs - 100, 200},
		{"ends_on_boundary", cs - 100, 100},
		{"starts_on_boundary", cs, 100},
		{"inside_first_chunk", 50, 200},
		{"spans_three_chunks", cs - 1, cs + 2},
		{"inside_last_short_chunk", 3*cs + 5, 10},
		{"to_end_of_evidence", 2*cs - 100, cs + 117},
		{"whole_evidence_from_one", 1, evidenceSize - 1},
	}

	testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "db")
		opts := optsForMode(dbPath, matrixChunkKB, m)

		src := testutil.WriteRandomFile(t, dir, "in/evidence.bin", 7, int(evidenceSize))
		if err := StoreData(t.Context(), opts, src, false, true, false, false); err != nil {
			t.Fatalf("store: %v", err)
		}
		content, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}

		hashes := make(map[string]string, len(ranges)+1)
		h, err := openTestDB(dbPath, []byte(m.Password))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		eid, err := dbio.GuessFileType(plainHashB64(t, src), h.DB)
		if err != nil {
			h.Close()
			t.Fatalf("find evidence: %v", err)
		}
		evidencePrefix := base64.StdEncoding.EncodeToString(dbio.EvidenceRawID(eid))
		addRange := func(name string, start, size int64) {
			raw := bytes.Repeat([]byte{byte(len(hashes) + 1)}, 16)
			p := structs.NewPartitionFile(evidencePrefix+cnst.DataSeperator+name, start, size, nil, "")
			if err := dbio.SetFile(util.AppendToBytesSlice(cnst.PartiFileNamespace, raw), p, h.DB); err != nil {
				t.Fatalf("seed range %s: %v", name, err)
			}
			hashes[name] = base64.StdEncoding.EncodeToString(raw)
		}
		for _, r := range ranges {
			addRange(r.name, r.start, r.size)
		}
		// A range that claims bytes past the end of its evidence.
		addRange("past_evidence_end", 3*cs, 100)
		if err := h.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		for _, r := range ranges {
			t.Run(r.name, func(t *testing.T) {
				out := filepath.Join(dir, "out", r.name)
				if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := RestoreData(t.Context(), opts, hashes[r.name], out, RestoreOptions{}); err != nil {
					t.Fatalf("restore [%d,+%d): %v", r.start, r.size, err)
				}
				got, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				want := content[r.start : r.start+r.size]
				if len(got) != len(want) {
					t.Fatalf("restore [%d,+%d): got %d bytes, want %d", r.start, r.size, len(got), len(want))
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("restore [%d,+%d): content differs", r.start, r.size)
				}
			})
		}

		t.Run("past_evidence_end", func(t *testing.T) {
			out := filepath.Join(dir, "out", "past_evidence_end")
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := RestoreData(t.Context(), opts, hashes["past_evidence_end"], out, RestoreOptions{}); err == nil {
				t.Fatal("restore of a range past the end of its evidence succeeded")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("failed restore left output behind: %v", err)
			}
		})
	})
}
