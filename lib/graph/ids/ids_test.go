package ids_test

import (
	"testing"

	"indicer/lib/graph/ids"
)

// TestHashText_MatchesStoredIdentifiers pins the derivation against identifiers
// that are already on disk in lib/graph/testdata/legacy-v1. If this test fails,
// the consolidation changed a persisted value and every node keyed by it in
// every existing store has been orphaned.
func TestHashText_MatchesStoredIdentifiers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		rest  []string
		want  string
	}{
		{
			// model.BuildIndexedFileID(partitionID, path, hash) for the Chrome
			// History file, as stored in the fixture's indexed_file_id property.
			name:  "indexed file id",
			value: "path-node",
			rest:  []string{"cGFydGl0aW9uLTE=", "/Users/jane/AppData/Local/Google/Chrome/History"},
			want:  "ce4668c24688e09d1d91553619ae0e5e9b689e6d4160118f6a6ccf9ae8807c76",
		},
		{
			// enrichment.FileRecord.buildFileNodeID() at evidence_file level.
			name:  "evidence-level file node id",
			value: "file-node",
			rest:  []string{"ZXZpZGVuY2UtaGFzaC1kaXNrMQ==", "/cases/2026-114/disk1.E01"},
			want:  "a34c410492ce2ee40025fdf2d52390a786eda33caec39344bbc90b5229b92c59",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ids.HashText(tc.value, tc.rest...); got != tc.want {
				t.Errorf("HashText(%q, %q)\n got %s\nwant %s", tc.value, tc.rest, got, tc.want)
			}
		})
	}
}

// The NUL separator is what stops component boundaries from being ambiguous.
func TestHashText_SeparatesComponents(t *testing.T) {
	t.Parallel()
	if ids.HashText("ab", "c") == ids.HashText("a", "bc") {
		t.Error(`HashText("ab","c") collides with HashText("a","bc"): components are not separated`)
	}
	if ids.HashText("a") == ids.HashText("a", "") {
		t.Error(`HashText("a") collides with HashText("a",""): an empty trailing component is not distinguished`)
	}
}

func TestHashText_IsDeterministic(t *testing.T) {
	t.Parallel()
	first := ids.HashText("path-node", "p1", "/tmp/x")
	for i := 0; i < 100; i++ {
		if got := ids.HashText("path-node", "p1", "/tmp/x"); got != first {
			t.Fatalf("HashText is not deterministic: %s != %s", got, first)
		}
	}
	if len(first) != 64 {
		t.Errorf("HashText returned %d hex chars, want 64", len(first))
	}
}

func TestArtefactLookupKey_DistinguishesKindFromValue(t *testing.T) {
	t.Parallel()
	if ids.ArtefactLookupKey("url", "x") == ids.ArtefactLookupKey("ur", "lx") {
		t.Error("ArtefactLookupKey does not separate kind from value")
	}
	if got, want := ids.ArtefactLookupKey("url", "http://a"), "url\x00http://a"; got != want {
		t.Errorf("ArtefactLookupKey = %q, want %q", got, want)
	}
}
