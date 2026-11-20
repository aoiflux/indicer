package microartefact

import (
	"testing"

	"indicer/internal/safe"
	"indicer/lib/microartefact/model"
	parsingpkg "indicer/lib/microartefact/parsing"
)

type panicParser struct{}

func (panicParser) Parse(model.FileRecord, []byte) (parsingpkg.Result, bool) {
	panic("malformed section table")
}

// TestExtractContainsParserPanics: a file parser that panics on evidence
// fails that file with an error instead of ending the process (PAR-1).
func TestExtractContainsParserPanics(t *testing.T) {
	t.Parallel()
	s := NewService(nil)
	s.parser = panicParser{}
	if _, err := s.Extract(FileRecord{Path: "/evil.exe"}, []byte("MZ")); !safe.IsPanic(err) {
		t.Fatalf("err %v, want a recovered panic", err)
	}
}
