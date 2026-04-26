package fileparser

import (
	"strings"
	"testing"

	saferelf "github.com/saferwall/elf"
	saferpe "github.com/saferwall/pe"
)

func TestBuildPEMetadataTextIncludesSectionsImportsAndAnomalies(t *testing.T) {
	parsed := &saferpe.File{
		Sections: []saferpe.Section{
			{
				Header: saferpe.ImageSectionHeader{
					Name:            [8]uint8{'.', 't', 'e', 'x', 't'},
					VirtualAddress:  0x1000,
					VirtualSize:     512,
					SizeOfRawData:   1024,
					Characteristics: 0x60000020,
				},
			},
		},
		Imports: []saferpe.Import{
			{
				Name: "KERNEL32.dll",
				Functions: []saferpe.ImportFunction{
					{Name: "CreateFileW"},
				},
			},
		},
		Anomalies: []string{"suspicious-header"},
	}

	metadata := buildPEMetadataText(parsed)
	if !strings.Contains(metadata, "__pe_metadata__") {
		t.Fatalf("expected pe metadata marker, got: %s", metadata)
	}
	if !strings.Contains(metadata, "pe.section_count=1") {
		t.Fatalf("expected section count in metadata, got: %s", metadata)
	}
	if !strings.Contains(metadata, "pe.import=KERNEL32.dll!CreateFileW") {
		t.Fatalf("expected import line in metadata, got: %s", metadata)
	}
	if !strings.Contains(metadata, "pe.anomaly=suspicious-header") {
		t.Fatalf("expected anomaly line in metadata, got: %s", metadata)
	}
}

func TestBuildELFMetadataTextIncludesSectionsAndSymbols(t *testing.T) {
	parsed := &saferelf.Parser{
		F: &saferelf.File{
			ELFBin32: saferelf.ELFBin32{
				Sections32: []*saferelf.ELF32Section{
					{
						SectionName: ".text",
						Size:        4096,
					},
				},
				Symbols32: make([]saferelf.ELF32SymbolTableEntry, 2),
			},
			ELFSymbols: saferelf.ELFSymbols{
				NamedSymbols: []saferelf.Symbol{
					{Name: "main", Value: 0x401000, Size: 128},
				},
			},
		},
	}

	metadata := buildELFMetadataText(parsed)
	if !strings.Contains(metadata, "__elf_metadata__") {
		t.Fatalf("expected elf metadata marker, got: %s", metadata)
	}
	if !strings.Contains(metadata, "elf.section_count=1") {
		t.Fatalf("expected section count in metadata, got: %s", metadata)
	}
	if !strings.Contains(metadata, "elf.symbol=main") {
		t.Fatalf("expected symbol line in metadata, got: %s", metadata)
	}
}

func TestCombineParsedPayloadAppendsMetadata(t *testing.T) {
	combined := combineParsedPayload([]byte("token-one\n"), "__pe_metadata__\npe.section_count=1")
	if !strings.Contains(string(combined), "token-one") {
		t.Fatalf("expected printable content preserved, got: %s", string(combined))
	}
	if !strings.Contains(string(combined), "__pe_metadata__") {
		t.Fatalf("expected metadata appended, got: %s", string(combined))
	}
	if !strings.HasSuffix(string(combined), "\n") {
		t.Fatalf("expected trailing newline in combined payload, got: %q", string(combined))
	}
}
