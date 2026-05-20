package fileparser

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"indicer/lib/microartefact/model"

	saferelf "github.com/saferwall/elf"
)

func parseELFContent(content []byte) ([]byte, *model.ELFMetadata, bool) {
	if len(content) == 0 {
		return nil, nil, false
	}

	tmpFile, err := os.CreateTemp("", "indicer-elf-*.bin")
	if err != nil {
		return nil, nil, false
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	defer tmpFile.Close()

	if _, err := tmpFile.Write(content); err != nil {
		return nil, nil, false
	}

	parsed, err := saferelf.New(tmpPath)
	if err != nil {
		return nil, nil, false
	}
	defer parsed.CloseFile()
	if err := parsed.Parse(); err != nil {
		return nil, nil, false
	}

	meta := extractELFMetadata(parsed)
	return combineParsedPayload(extractPrintableText(content), buildELFMetadataText(parsed)), meta, true
}

func extractELFMetadata(parsed *saferelf.Parser) *model.ELFMetadata {
	if parsed == nil || parsed.F == nil {
		return nil
	}

	f := parsed.F
	metadata := &model.ELFMetadata{}

	const maxSections = 64
	if f.IsELF64() {
		metadata.Class = 64
		metadata.SectionCount = len(f.Sections64)
		metadata.RawSymbolCount = len(f.Symbols64)

		metadata.Sections = make([]model.ELFSectionMetadata, 0, minInt(len(f.Sections64), maxSections))
		for i, section := range f.Sections64 {
			if i >= maxSections {
				metadata.SectionsTruncated = len(f.Sections64) - maxSections
				break
			}
			if section == nil {
				continue
			}
			name := strings.TrimSpace(section.SectionName)
			if name == "" {
				name = "unnamed"
			}
			metadata.Sections = append(metadata.Sections, model.ELFSectionMetadata{
				Name: name,
				Type: fmt.Sprintf("%v", section.Type),
				Size: section.Size,
			})
		}
	} else {
		metadata.Class = 32
		metadata.SectionCount = len(f.Sections32)
		metadata.RawSymbolCount = len(f.Symbols32)

		metadata.Sections = make([]model.ELFSectionMetadata, 0, minInt(len(f.Sections32), maxSections))
		for i, section := range f.Sections32 {
			if i >= maxSections {
				metadata.SectionsTruncated = len(f.Sections32) - maxSections
				break
			}
			if section == nil {
				continue
			}
			name := strings.TrimSpace(section.SectionName)
			if name == "" {
				name = "unnamed"
			}
			metadata.Sections = append(metadata.Sections, model.ELFSectionMetadata{
				Name: name,
				Type: fmt.Sprintf("%v", section.Type),
				Size: uint64(section.Size),
			})
		}
	}

	metadata.NamedSymbolCount = len(f.NamedSymbols)
	const maxSymbols = 256
	metadata.NamedSymbols = make([]model.ELFSymbolMetadata, 0, minInt(len(f.NamedSymbols), maxSymbols))
	for i, symbol := range f.NamedSymbols {
		if i >= maxSymbols {
			metadata.NamedSymbolsTruncated = len(f.NamedSymbols) - maxSymbols
			break
		}
		name := strings.TrimSpace(symbol.Name)
		if name == "" {
			continue
		}
		metadata.NamedSymbols = append(metadata.NamedSymbols, model.ELFSymbolMetadata{
			Name:    name,
			Value:   symbol.Value,
			Size:    symbol.Size,
			Version: strings.TrimSpace(symbol.Version),
			Library: strings.TrimSpace(symbol.Library),
		})
	}

	return metadata
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func buildELFMetadataText(parsed *saferelf.Parser) string {
	if parsed == nil || parsed.F == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString("__elf_metadata__\n")

	f := parsed.F
	if f.IsELF64() {
		b.WriteString("elf.class=64\n")
		fmt.Fprintf(&b, "elf.section_count=%d\n", len(f.Sections64))
		fmt.Fprintf(&b, "elf.raw_symbol_count=%d\n", len(f.Symbols64))

		const maxSections = 64
		for i, section := range f.Sections64 {
			if i >= maxSections {
				fmt.Fprintf(&b, "elf.sections_truncated=%d\n", len(f.Sections64)-maxSections)
				break
			}
			if section == nil {
				continue
			}
			name := strings.TrimSpace(section.SectionName)
			if name == "" {
				name = "unnamed"
			}
			fmt.Fprintf(&b, "elf.section.%d=%s|type=%v|size=%d\n", i, name, section.Type, section.Size)
		}
	} else {
		b.WriteString("elf.class=32\n")
		fmt.Fprintf(&b, "elf.section_count=%d\n", len(f.Sections32))
		fmt.Fprintf(&b, "elf.raw_symbol_count=%d\n", len(f.Symbols32))

		const maxSections = 64
		for i, section := range f.Sections32 {
			if i >= maxSections {
				fmt.Fprintf(&b, "elf.sections_truncated=%d\n", len(f.Sections32)-maxSections)
				break
			}
			if section == nil {
				continue
			}
			name := strings.TrimSpace(section.SectionName)
			if name == "" {
				name = "unnamed"
			}
			fmt.Fprintf(&b, "elf.section.%d=%s|type=%v|size=%d\n", i, name, section.Type, section.Size)
		}
	}

	fmt.Fprintf(&b, "elf.named_symbol_count=%d\n", len(f.NamedSymbols))
	const maxSymbols = 256
	for i, symbol := range f.NamedSymbols {
		if i >= maxSymbols {
			fmt.Fprintf(&b, "elf.named_symbols_truncated=%d\n", len(f.NamedSymbols)-maxSymbols)
			break
		}
		name := strings.TrimSpace(symbol.Name)
		if name == "" {
			continue
		}
		fmt.Fprintf(
			&b,
			"elf.symbol=%s|value=0x%x|size=%d|version=%s|library=%s\n",
			name,
			symbol.Value,
			symbol.Size,
			strings.TrimSpace(symbol.Version),
			strings.TrimSpace(symbol.Library),
		)
	}

	return b.String()
}

func combineParsedPayload(printable []byte, metadata string) []byte {
	metadata = strings.TrimSpace(metadata)
	if metadata == "" {
		return printable
	}

	metaBytes := []byte(metadata)
	if len(printable) == 0 {
		combined := make([]byte, len(metaBytes)+1)
		copy(combined, metaBytes)
		combined[len(metaBytes)] = '\n'
		return combined
	}

	combined := make([]byte, 0, len(printable)+len(metaBytes)+2)
	combined = append(combined, printable...)
	if !bytes.HasSuffix(combined, []byte("\n")) {
		combined = append(combined, '\n')
	}
	combined = append(combined, metaBytes...)
	combined = append(combined, '\n')
	return combined
}
