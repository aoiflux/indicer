package detectors

import (
	"encoding/binary"
	"testing"
	"unicode/utf8"

	"indicer/lib/microartefact/model"
)

// minimalPE builds a 32-bit PE image with one section whose raw data starts
// at sectionOffset, which may lie past the end of the image.
func minimalPE(sectionOffset uint32) []byte {
	b := make([]byte, 1024)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], 0x40)
	copy(b[0x40:], "PE\x00\x00")
	coff := b[0x44:]
	binary.LittleEndian.PutUint16(coff[0:], 0x14c)      // i386
	binary.LittleEndian.PutUint16(coff[2:], 1)          // one section
	binary.LittleEndian.PutUint32(coff[4:], 0x5F5E1000) // 2020-09-13
	binary.LittleEndian.PutUint16(coff[16:], 0xE0)      // optional header size
	binary.LittleEndian.PutUint16(coff[18:], 0x0102)    // executable, 32-bit
	opt := b[0x44+20:]
	binary.LittleEndian.PutUint16(opt[0:], 0x10b) // PE32
	binary.LittleEndian.PutUint32(opt[92:], 16)   // data directories
	sec := b[0x44+20+0xE0:]
	copy(sec, ".text")
	binary.LittleEndian.PutUint32(sec[8:], 0x100) // virtual size
	binary.LittleEndian.PutUint32(sec[12:], 0x1000)
	binary.LittleEndian.PutUint32(sec[16:], 0x200) // raw size
	binary.LittleEndian.PutUint32(sec[20:], sectionOffset)
	binary.LittleEndian.PutUint32(sec[36:], 0x60000020)
	for i := 0x200; i < 0x400; i++ {
		b[i] = byte(i)
	}
	return b
}

// GRA-13: the PE detector parses the file's own bytes and finds its compile
// time and sections.
func TestPEMetadataDetectorParsesRawPE(t *testing.T) {
	d := PEMetadataDetector{}
	if !d.WantsRaw() {
		t.Fatal("the PE detector does not ask for the file's bytes")
	}
	arts, err := d.Detect(model.FileRecord{}, minimalPE(0x200))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, a := range arts {
		kinds[a.Kind]++
	}
	if kinds["pe_timestamp"] != 1 || kinds["pe_section"] != 1 {
		t.Fatalf("artefacts %v", kinds)
	}
}

// GRA-13: a section whose raw data lies past the end of the file is clamped.
// The slice used to run out of range and panic.
func TestPESectionPastEndDoesNotPanic(t *testing.T) {
	arts, err := PEMetadataDetector{}.Detect(model.FileRecord{}, minimalPE(0x10000))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range arts {
		if a.Kind == "pe_section" && (a.Span.Start > 1024 || a.Span.End > 1024) {
			t.Errorf("section span %+v lies past the file", a.Span)
		}
	}
}

// GRA-18: truncation keeps whole runes.
func TestTruncateKeepsRunes(t *testing.T) {
	s := "abc" + "ééé" + "€"
	for limit := 0; limit <= len(s); limit++ {
		got := truncate(s, limit)
		if len(got) > limit || !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) = %q", s, limit, got)
		}
	}
}

// GRA-18: a FILETIME before 1970 has no RFC 3339 rendering here. The
// subtraction used to wrap and print a date tens of thousands of years ahead.
func TestFiletimeBeforeEpoch(t *testing.T) {
	for _, ft := range []uint64{1, filetimeUnixEpoch - 1, filetimeUnixEpoch} {
		if got := windowsFiletimeToRFC3339(ft); got != "" {
			t.Errorf("FILETIME %d rendered as %q", ft, got)
		}
	}
	if got := windowsFiletimeToRFC3339(filetimeUnixEpoch + 10_000_000*86400); got != "1970-01-02T00:00:00Z" {
		t.Errorf("one day after the epoch rendered as %q", got)
	}
}

// GRA-17: the log-line detector allocates for the results it keeps.
func TestLogLineDetectorKeepsAtMostMax(t *testing.T) {
	var content []byte
	for i := 0; i < 1000; i++ {
		content = append(content, "2024-01-01 10:00:00 error something\n"...)
	}
	arts, err := LogLineDetector{MaxResults: 5}.Detect(model.FileRecord{}, content)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 5 || cap(arts) > 5 {
		t.Errorf("%d results with capacity %d, want 5", len(arts), cap(arts))
	}
}
