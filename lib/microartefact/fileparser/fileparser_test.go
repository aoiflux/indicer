package fileparser

import (
	"testing"

	"indicer/lib/microartefact/model"
)

func TestParseUsesIndexedFileTypeOverHeuristics(t *testing.T) {
	content := []byte("alpha=bravo\ncharlie=delta\n")
	file := model.FileRecord{
		Name:     "sample.bin",
		Path:     "sample.bin",
		FileType: "txt",
	}

	parsed, ok := Parse(file, content)
	if !ok {
		t.Fatal("expected parse to succeed with indexed file type")
	}
	if parsed.Kind != KindText {
		t.Fatalf("expected kind %q, got %q", KindText, parsed.Kind)
	}
}

func TestParseFailsWhenIndexedTypeUnsupported(t *testing.T) {
	content := []byte("plain text content")
	file := model.FileRecord{
		Name:     "sample.txt",
		Path:     "sample.txt",
		FileType: "jpeg",
	}

	if _, ok := Parse(file, content); ok {
		t.Fatal("expected parse to fail for unsupported indexed file type")
	}
}

func TestParseFallsBackWhenFileTypeUnknown(t *testing.T) {
	content := []byte("alpha=bravo\ncharlie=delta\n")
	file := model.FileRecord{
		Name:     "sample.txt",
		Path:     "sample.txt",
		FileType: "unknown",
	}

	parsed, ok := Parse(file, content)
	if !ok {
		t.Fatal("expected parse to succeed via fallback when file type is unknown")
	}
	if parsed.Kind != KindText {
		t.Fatalf("expected kind %q, got %q", KindText, parsed.Kind)
	}
}
