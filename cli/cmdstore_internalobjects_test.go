package cli

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/structs"
)

// TestStoreRecordsIndexState: every stored evidence records why it was or
// was not indexed. The legacy store recorded nothing, so an evidence whose
// index never ran looked the same as one with nothing in it (ING-4).
func TestStoreRecordsIndexState(t *testing.T) {
	_, e01 := fixtureNamed(t, "ewf/fat12_multiseg")
	dir := t.TempDir()
	qcow := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(qcow, append([]byte("QFI\xfb"), make([]byte, 8188)...), 0o600); err != nil {
		t.Fatal(err)
	}
	e01Size := func() int64 {
		fi, err := os.Stat(e01)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}()

	for _, tc := range []struct {
		name     string
		path     string
		opts     StoreOptions
		state    structs.IndexState
		reason   string
		format   string
		rawBytes int64 // the size stored, when it is the file's own
	}{
		{"not requested", e01, StoreOptions{NoIndex: true, Decode: true}, structs.IndexSkipped, "not_requested", structs.SourceEWF, 0},
		{"not decoded", e01, StoreOptions{Decode: false}, structs.IndexSkipped, "container_not_decoded", structs.SourceRaw, e01Size},
		{"unsupported container", qcow, StoreOptions{Decode: true}, structs.IndexSkipped, "unsupported_container", structs.SourceOpaque, 8192},
		{"decoded and indexed", e01, StoreOptions{Decode: true}, structs.IndexComplete, "", structs.SourceEWF, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "db")
			if err := StoreDataWithOptions(t.Context(), optsFor(dbPath, matrixChunkKB, nil), tc.path, tc.opts); err != nil {
				t.Fatalf("store: %v", err)
			}
			_, rec := onlyEvidence(t, dbPath)
			if rec.Index == nil || rec.Index.State != tc.state || rec.Index.Reason != tc.reason {
				t.Fatalf("index info %+v, want %s (%s)", rec.Index, tc.state, tc.reason)
			}
			if rec.Source == nil || rec.Source.Format != tc.format {
				t.Fatalf("source %+v, want format %s", rec.Source, tc.format)
			}
			if tc.rawBytes > 0 && rec.Size != tc.rawBytes {
				t.Fatalf("stored %d bytes, want the file's own %d", rec.Size, tc.rawBytes)
			}
			if tc.state == structs.IndexSkipped && len(rec.InternalObjects) != 0 {
				t.Fatalf("an evidence that was not indexed links %d partitions", len(rec.InternalObjects))
			}
		})
	}
}
