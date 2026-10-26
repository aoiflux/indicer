package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"indicer/internal/console"
	"indicer/lib/microartefact"
	graphrepo "indicer/lib/microartefact/repository/graphene"
)

// TestMicroExtractPrintsTopFiles: after extracting, micro extract shows the
// files with the most artefacts with their totals, and how many more there
// are, instead of the whole graph (GRA-17).
func TestMicroExtractPrintsTopFiles(t *testing.T) {
	var out bytes.Buffer
	console.Configure(&out, io.Discard)
	t.Cleanup(console.Reset)
	file := &microartefact.IndexedFileArtefactView{}
	file.Name, file.Size, file.ArtefactCount = "a.exe", 2048, 57
	art := &microartefact.MicroArtefactNode{}
	art.Kind, art.Value = "url", "http://example.com"
	file.Artefacts = []*microartefact.MicroArtefactNode{art}
	part := &microartefact.PartitionFileNode{}
	part.Name, part.Files = "p0", []*microartefact.IndexedFileArtefactView{file}
	disk := &microartefact.EvidenceFileNode{}
	disk.Name, disk.Partitions = "case.img", []*microartefact.PartitionFileNode{part}
	tree := &microartefact.EvidenceFileHierarchy{}
	tree.EvidenceFiles = []*microartefact.EvidenceFileNode{disk}
	top := &graphrepo.TopFiles{Tree: tree, Files: 30, Artefacts: 900, Shown: 1}
	printTopFiles(top)
	got := out.String()
	for _, want := range []string{"30 file(s) hold 900 artefact(s)", "a.exe", "57 artefact(s)", "29 more file(s)", "dues micro list"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
}
