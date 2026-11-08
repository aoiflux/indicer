package microartefact

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"indicer/lib/enrichment"
	duesgraph "indicer/lib/graph"
	"indicer/lib/graph/schema"
	reppkg "indicer/lib/microartefact/repository"
	specpkg "indicer/lib/microartefact/spec"
)

// testPE builds a 32-bit PE image with one section.
func testPE() []byte {
	b := make([]byte, 1024)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	coff := b[0x44:]
	binary.LittleEndian.PutUint16(coff[0:], 0x14c)
	binary.LittleEndian.PutUint16(coff[2:], 1)
	binary.LittleEndian.PutUint32(coff[4:], 0x5F5E1000)
	binary.LittleEndian.PutUint16(coff[16:], 0xE0)
	binary.LittleEndian.PutUint16(coff[18:], 0x0102)
	opt := b[0x44+20:]
	binary.LittleEndian.PutUint16(opt[0:], 0x10b)
	binary.LittleEndian.PutUint32(opt[92:], 16)
	sec := b[0x44+20+0xE0:]
	copy(sec, ".text")
	binary.LittleEndian.PutUint32(sec[8:], 0x100)
	binary.LittleEndian.PutUint32(sec[12:], 0x1000)
	binary.LittleEndian.PutUint32(sec[16:], 0x200)
	binary.LittleEndian.PutUint32(sec[20:], 0x200)
	return b
}

// GRA-13: through the service, the PE detector gets the file's bytes, not
// the text the PE parser made of them, and finds the PE's metadata.
func TestServicePEDetectorGetsRawBytes(t *testing.T) {
	t.Parallel()
	repo := &captureRepository{}
	out, err := NewService(repo).ProcessFile(FileRecord{Name: "a.exe", Path: "/a.exe", FileType: "pe"}, testPE(), 1024, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range repo.artefacts {
		if a.Kind == "pe_timestamp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no PE metadata found (status %s, artefacts %v)", out.Status, repo.artefacts)
	}
}

// GRA-16: the outcome says what happened, so a parse failure or a file too
// large to parse is not reported as a file with no artefacts.
func TestServiceOutcomes(t *testing.T) {
	t.Parallel()
	svc := NewService(&captureRepository{})
	text := []byte("see https://example.org/a and key=value\n")
	brokenPE := append(append([]byte("MZ"), make([]byte, 62)...), "PE\x00\x00 junk https://example.org/b"...)
	brokenPE[0x3c] = 0x40
	for _, tc := range []struct {
		name    string
		file    FileRecord
		content []byte
		size    int64
		status  string
		some    bool
	}{
		{"text", FileRecord{Name: "a.txt", FileType: "txt"}, text, int64(len(text)), reppkg.StatusExtracted, true},
		{"nothing", FileRecord{Name: "b.txt", FileType: "txt"}, []byte("plain words only\n"), 17, reppkg.StatusNone, false},
		{"unsupported", FileRecord{Name: "c.png", FileType: "png"}, []byte("\x89PNG...."), 8, reppkg.StatusUnsupported, false},
		{"parse failed", FileRecord{Name: "d.exe", FileType: "pe"}, brokenPE, int64(len(brokenPE)), reppkg.StatusParseFailed, true},
		{"too large", FileRecord{Name: "e.bin", FileType: "pe"}, text, RawLimit + 1, reppkg.StatusTooLarge, true},
	} {
		out, err := svc.ProcessFile(tc.file, tc.content, tc.size, false)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if out.Status != tc.status || (out.Artefacts > 0) != tc.some {
			t.Errorf("%s: status %s with %d artefacts, want %s (artefacts %v)", tc.name, out.Status, out.Artefacts, tc.status, tc.some)
		}
	}
}

// GRA-18: removing a rule from a copy of a sheet leaves the original alone.
func TestSpecFiltersCopyOnWrite(t *testing.T) {
	t.Parallel()
	base := specpkg.Default()
	var want []string
	for _, r := range base.DetectionRules {
		want = append(want, r.ID)
	}
	copied := base // shares the rules' backing array
	copied.RemoveDetectionRule(want[0])
	if len(copied.DetectionRules) != len(want)-1 {
		t.Fatalf("the copy has %d rules, want %d", len(copied.DetectionRules), len(want)-1)
	}
	for i, id := range want {
		if base.DetectionRules[i].ID != id {
			t.Fatalf("rule %d of the original is %s after a copy removed one, want %s", i, base.DetectionRules[i].ID, id)
		}
	}
}

// GRA-17: a value found many times in a file is related to itself as a
// star, not every pair, so its relation edges stay bounded.
func TestRelationEdgesAreBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	enr, err := enrichment.OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := enr.UpsertFile(enrichment.FileRecord{Level: "indexed_file", EvidenceFileID: "ev", PartitionID: "p", IndexedFileID: "f", FileName: "f.log", Path: "/f.log"}); err != nil {
		t.Fatal(err)
	}
	if err := enr.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := OpenGrapheneRepository(root)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var content bytes.Buffer
	for i := 0; i < 100; i++ {
		content.WriteString(strings.Repeat(" ", i) + "contact 10.1.2.3 now\n")
	}
	file := FileRecord{Name: "f.log", Path: "/f.log", FileType: "log", EvidenceFileID: "ev", PartitionID: "p", IndexedFileID: "f"}
	out, err := NewService(repo).ProcessFile(file, content.Bytes(), int64(content.Len()), false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Artefacts < 100 {
		t.Fatalf("%d artefacts, want the IP found 100 times", out.Artefacts)
	}

	h, err := duesgraph.OpenWith(root, duesgraph.OpenOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	edges, err := h.Graph().EdgesByType(h.Schema().EdgeType(schema.EdgeKindRelation))
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) == 0 || len(edges) > 64 {
		t.Errorf("%d relation edges for one value found 100 times, want between 1 and 64", len(edges))
	}

	// The file's node counts its artefacts and records that it was examined.
	status, err := repo.ExtractionStatus("f")
	if err != nil || status != reppkg.StatusExtracted {
		t.Errorf("status %q, %v", status, err)
	}
}
