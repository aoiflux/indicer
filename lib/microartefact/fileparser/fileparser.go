package fileparser

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"indicer/lib/microartefact/model"
)

type Kind string

const (
	KindText     Kind = "text"
	KindPDF      Kind = "pdf"
	KindPE       Kind = "pe"
	KindEVTX     Kind = "evtx"
	KindELF      Kind = "elf"
	KindRegistry Kind = "registry"
)

// ParsedFile is what a file's content became for the detectors.
type ParsedFile struct {
	Kind Kind
	// Content is what text detectors scan: the text, or for a binary format
	// its printable strings and the metadata its parser found.
	Content []byte
	// Raw is the file's own bytes, for detectors that parse the format
	// themselves (PE metadata, EVTX, registry hives). They used to be given
	// Content, which for a PE is text, so they never fired (GRA-13).
	Raw     []byte
	ELFMeta *model.ELFMetadata
	// Err is why the format's parser failed. Content is then the printable
	// strings, so text detectors still run, and the failure is counted rather
	// than reported as a file with no artefacts (GRA-16).
	Err error
}

// Parse prepares a file's content for the detectors. It reports false for a
// file of a kind no detector applies to.
func Parse(file model.FileRecord, content []byte) (ParsedFile, bool) {
	kind, ok := detectSupportedKind(file, content)
	if !ok {
		return ParsedFile{}, false
	}
	out := ParsedFile{Kind: kind, Raw: content}

	switch kind {
	case KindText:
		parsed, ok := parseTextContent(content)
		if !ok {
			return ParsedFile{}, false
		}
		out.Content = parsed
	case KindEVTX, KindRegistry:
		out.Content = content
	case KindPDF:
		out.Content, out.Err = parsePDFContent(content)
	case KindPE:
		out.Content, out.Err = parsePEContent(content)
	case KindELF:
		out.Content, out.ELFMeta, out.Err = parseELFContent(content)
	default:
		return ParsedFile{}, false
	}
	if out.Err != nil {
		out.Content = extractPrintableText(content)
	}
	return out, true
}

// detectSupportedKind decides how a file is parsed: by the magic of a format
// the detectors parse, then by the type its record carries (a type naming a
// format no detector reads is not parsed), then, for a record without one,
// by its extension and by whether it reads as text. The record's type used to
// decide first, so a registry hive (typed "dat" from its extension, or not at
// all) was never recognised.
func detectSupportedKind(file model.FileRecord, content []byte) (Kind, bool) {
	switch {
	case hasPrefix(content, []byte("%PDF-")):
		return KindPDF, true
	case isRegistryHive(content):
		return KindRegistry, true
	case isEVTX(content):
		return KindEVTX, true
	case isELF(content):
		return KindELF, true
	case isPE(content):
		return KindPE, true
	}

	if fileType := strings.ToLower(strings.TrimSpace(file.FileType)); fileType != "" && fileType != "unknown" {
		return kindFromFileType(fileType)
	}

	// Records that do not carry a normalized file type.
	ext := strings.ToLower(filepath.Ext(file.Name))
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(file.Path))
	}
	switch ext {
	case ".txt", ".log", ".md", ".json", ".xml", ".csv", ".yaml", ".yml", ".ini", ".conf", ".toml":
		return KindText, true
	case ".pdf":
		return KindPDF, true
	case ".exe", ".dll":
		return KindPE, true
	case ".evtx":
		return KindEVTX, true
	case ".elf":
		return KindELF, true
	}

	if looksLikeText(content) {
		return KindText, true
	}
	return "", false
}

func parseTextContent(content []byte) ([]byte, bool) {
	if len(content) == 0 {
		return nil, false
	}
	return content, true
}

func kindFromFileType(fileType string) (Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(fileType)) {
	case "txt", "log", "md", "json", "xml", "csv", "yaml", "yml", "ini", "conf", "toml", "text":
		return KindText, true
	case "pdf":
		return KindPDF, true
	case "exe", "dll", "pe":
		return KindPE, true
	case "evtx":
		return KindEVTX, true
	case "elf":
		return KindELF, true
	default:
		return "", false
	}
}

func hasPrefix(b []byte, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

func isEVTX(content []byte) bool {
	return hasPrefix(content, []byte("ElfFile\x00"))
}

// isRegistryHive recognises a Windows registry hive by its base block
// signature. Hives carry no extension (SYSTEM, NTUSER.DAT), so without this
// the registry detector never ran.
func isRegistryHive(content []byte) bool {
	return hasPrefix(content, []byte("regf"))
}

func isELF(content []byte) bool {
	return len(content) >= 4 && content[0] == 0x7f && content[1] == 'E' && content[2] == 'L' && content[3] == 'F'
}

func isPE(content []byte) bool {
	if len(content) < 0x40 {
		return false
	}
	if content[0] != 'M' || content[1] != 'Z' {
		return false
	}
	off := int(content[0x3c]) | int(content[0x3d])<<8 | int(content[0x3e])<<16 | int(content[0x3f])<<24
	if off < 0 || off+4 > len(content) {
		return false
	}
	return content[off] == 'P' && content[off+1] == 'E' && content[off+2] == 0 && content[off+3] == 0
}

func looksLikeText(content []byte) bool {
	if len(content) == 0 {
		return false
	}
	sample := content
	if len(sample) > 4096 {
		sample = sample[:4096]
	}

	if !utf8.Valid(sample) {
		return false
	}

	var printable int
	for _, b := range sample {
		if b == 0 {
			return false
		}
		if b == '\n' || b == '\r' || b == '\t' || (b >= 32 && b <= 126) {
			printable++
		}
	}
	return float64(printable)/float64(len(sample)) >= 0.85
}

func extractPrintableText(content []byte) []byte {
	const minLen = 6
	buf := make([]byte, 0, len(content)/8)
	curr := make([]byte, 0, 256)
	flush := func() {
		if len(curr) >= minLen {
			buf = append(buf, curr...)
			buf = append(buf, '\n')
		}
		curr = curr[:0]
	}

	for _, b := range content {
		if b == '\t' || b == ' ' || (b >= 33 && b <= 126) {
			curr = append(curr, b)
			continue
		}
		flush()
	}
	flush()
	return buf
}
