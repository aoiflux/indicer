package microartefact

import (
	"testing"

	"indicer/lib/microartefact/detection"
	"indicer/lib/microartefact/parsing"
)

// FuzzExtract runs the whole extraction (format parsers, detectors,
// relations) over content of every kind it handles. The format parsers are
// third-party code DUES contains (PAR-1), so a panic in them is a failed
// file, not a finding: this target runs without DUES_SAFE_STRICT. What must
// not happen is a crash past the containment, a hang, or unbounded output.
func FuzzExtract(f *testing.F) {
	f.Setenv("DUES_SAFE_STRICT", "")
	for _, seed := range [][]byte{
		[]byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xff\xffPE\x00\x00"),
		[]byte("\x7fELF\x02\x01\x01\x00"),
		[]byte("%PDF-1.4\n1 0 obj << /Length 5 /Filter /FlateDecode >> stream\nx\x9c\x03\x00endstream endobj"),
		[]byte("ElfFile\x00"),
		[]byte("regf\x00\x00\x00\x00"),
		[]byte("visit http://example.com/a?b=c from 10.0.0.1 user=alice"),
	} {
		f.Add(seed)
	}
	svc := NewService(nil)
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, typ := range []string{"exe", "elf", "pdf", "evtx", "dat", "txt"} {
			file := FileRecord{Name: "f." + typ, Path: "/f." + typ, FileType: typ, Size: int64(len(data))}
			arts, rels, _ := svc.ExtractWithRelations(file, data)
			if len(arts) > 1_000_000 || len(rels) > 1_000_000 {
				t.Fatalf("%s: %d artefacts and %d relations from %d bytes", typ, len(arts), len(rels), len(data))
			}
		}
	})
}

// FuzzTextDetectors runs DUES's own text detectors over arbitrary text, with
// DUES_SAFE_STRICT as the fuzz smoke sets it: a panic in them is a finding.
func FuzzTextDetectors(f *testing.F) {
	f.Add([]byte("GET http://example.com/x?a=1 HTTP/1.1\r\nHost: 192.168.0.1:8080\r\n"))
	f.Add([]byte(`schtasks /create /tn x /tr c:\evil.exe` + "\nkey=value; secret = \"s\"\n"))
	f.Add([]byte("2024-01-02 03:04:05 EventID=4624 Logon\n"))
	e := detection.NewEngine(nil, detection.DefaultMethods())
	f.Fuzz(func(t *testing.T, data []byte) {
		arts, err := e.Detect(FileRecord{Name: "f.txt", Path: "/f.txt", FileType: "txt"}, parsing.Result{Kind: parsing.KindText, Content: data})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range arts {
			if a.Span.Start < 0 || a.Span.End < a.Span.Start || a.Span.End > int64(len(data)) {
				t.Fatalf("artefact %q at [%d, %d) of %d bytes", a.Value, a.Span.Start, a.Span.End, len(data))
			}
		}
	})
}
