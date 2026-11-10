package dbpackage

import (
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/dbmeta"
)

// TestGraphImageMustTravelWithItsMarker: a graph whose schema marker
// records a compacted image holds in its log only what came after it, so a
// package that has the marker and not the image fails verify and import,
// and one that has both passes.
func TestGraphImageMustTravelWithItsMarker(t *testing.T) {
	t.Parallel()
	root := newRoot(t, dbmeta.NoPassword())
	marker := filepath.Join(root, "graph", "dues.schema.json")
	if err := os.WriteFile(marker, []byte(`{"schema_version": 3, "image": {"csr_version": 9, "segment_seq": 1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const image = "db/graph/graphene.csr"

	without := export(t, ExportOptions{Path: root})
	p, err := Open(without.Output)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Verify(t.Context(), VerifyOptions{})
	p.Close()
	if !isDamaged(err, image) {
		t.Fatalf("verify of a package without the image: %v, want %s named", err, image)
	}
	target := filepath.Join(t.TempDir(), "target")
	if _, err := Import(t.Context(), ImportOptions{Package: without.Output, Path: target}); !isDamaged(err, image) {
		t.Fatalf("import of a package without the image: %v, want %s named", err, image)
	}
	noStage(t, target, false)

	if err := os.WriteFile(filepath.Join(root, "graph", "graphene.csr"), []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	with := export(t, ExportOptions{Path: root})
	p, err = Open(with.Output)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Verify(t.Context(), VerifyOptions{})
	p.Close()
	if err != nil {
		t.Fatalf("verify of a package with the image: %v", err)
	}
}
