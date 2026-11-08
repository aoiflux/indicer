package fileparser

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"testing"

	"indicer/lib/microartefact/model"
)

// pdfWithStream builds a one-page PDF whose content stream is data,
// Flate-compressed.
func pdfWithStream(t *testing.T, data []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{}
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	obj("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>")
	offsets = append(offsets, b.Len())
	fmt.Fprintf(&b, "4 0 obj\n<< /Length %d /Filter /FlateDecode >>\nstream\n", z.Len())
	b.Write(z.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}

// GRA-14: a PDF whose stream inflates past the limits is refused before
// pdfcpu, which inflates every stream without a limit, reads it.
func TestPDFInflationIsBounded(t *testing.T) {
	t.Parallel()
	bomb := pdfWithStream(t, make([]byte, 40<<20))
	if len(bomb) > 1<<20 {
		t.Fatalf("the test PDF is %d bytes", len(bomb))
	}
	if _, err := parsePDFContent(bomb); !errors.Is(err, ErrPDFTooLarge) {
		t.Fatalf("a PDF inflating to 40 MiB: %v, want ErrPDFTooLarge", err)
	}

	// An ordinary PDF passes the guard and its text is extracted.
	text := []byte("BT /F1 12 Tf 72 712 Td (visit https://example.org/page) Tj ET")
	ok := pdfWithStream(t, text)
	if err := checkPDFInflation(ok); err != nil {
		t.Fatalf("an ordinary PDF: %v", err)
	}
	got, err := parsePDFContent(ok)
	if err != nil {
		t.Fatalf("an ordinary PDF: %v", err)
	}
	if !bytes.Contains(got, []byte("https://example.org/page")) {
		t.Errorf("page text %q", got)
	}
}

// A registry hive is recognised by its signature, whatever its record's type
// says; a hive is typed "dat" from its extension, or not at all.
func TestParseRecognisesRegistryHives(t *testing.T) {
	t.Parallel()
	hive := append([]byte("regf"), make([]byte, 4092)...)
	for _, ft := range []string{"", "unknown", "dat"} {
		parsed, ok := Parse(model.FileRecord{Name: "NTUSER.DAT", FileType: ft}, hive)
		if !ok || parsed.Kind != KindRegistry {
			t.Errorf("type %q: kind %q, ok %v", ft, parsed.Kind, ok)
		}
		if !bytes.Equal(parsed.Raw, hive) {
			t.Errorf("type %q: the raw bytes are not kept", ft)
		}
	}
}

// GRA-16: a PE whose parser fails still yields its printable strings for the
// text detectors, and the failure is reported.
func TestParseFailureKeepsStrings(t *testing.T) {
	t.Parallel()
	content := append([]byte("MZ"), make([]byte, 62)...)
	content[0x3c] = 0x40
	content = append(content, []byte("PE\x00\x00 garbage https://example.org/payload trailing text")...)
	parsed, ok := Parse(model.FileRecord{Name: "x.exe", FileType: "pe"}, content)
	if !ok || parsed.Kind != KindPE {
		t.Fatalf("kind %q, ok %v", parsed.Kind, ok)
	}
	if parsed.Err == nil {
		t.Fatal("the PE parser's failure is not reported")
	}
	if !bytes.Contains(parsed.Content, []byte("https://example.org/payload")) {
		t.Errorf("the strings were not kept: %q", parsed.Content)
	}
}
