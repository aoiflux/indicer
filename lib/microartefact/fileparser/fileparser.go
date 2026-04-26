package fileparser

import (
	"path/filepath"
	"strings"
	"unicode/utf8"

	"indicer/lib/microartefact/model"
)

type Kind string

const (
	KindText Kind = "text"
	KindPDF  Kind = "pdf"
	KindPE   Kind = "pe"
	KindEVTX Kind = "evtx"
	KindELF  Kind = "elf"
)

type ParsedFile struct {
	Kind    Kind
	Content []byte
}

func Parse(file model.FileRecord, content []byte) (ParsedFile, bool) {
	kind, ok := detectSupportedKind(file, content)
	if !ok {
		return ParsedFile{}, false
	}

	switch kind {
	case KindText:
		parsed, ok := parseTextContent(content)
		if !ok {
			return ParsedFile{}, false
		}
		return ParsedFile{Kind: kind, Content: parsed}, true
	case KindEVTX:
		return ParsedFile{Kind: kind, Content: content}, true
	case KindPDF:
		parsed, ok := parsePDFContent(content)
		if !ok {
			return ParsedFile{}, false
		}
		return ParsedFile{Kind: kind, Content: parsed}, true
	case KindPE:
		parsed, ok := parsePEContent(content)
		if !ok {
			return ParsedFile{}, false
		}
		return ParsedFile{Kind: kind, Content: parsed}, true
	case KindELF:
		parsed, ok := parseELFContent(content)
		if !ok {
			return ParsedFile{}, false
		}
		return ParsedFile{Kind: kind, Content: parsed}, true
	default:
		return ParsedFile{}, false
	}
}

func detectSupportedKind(file model.FileRecord, content []byte) (Kind, bool) {
	fileType := strings.ToLower(strings.TrimSpace(file.FileType))
	if fileType != "" && fileType != "unknown" {
		kind, ok := kindFromFileType(fileType)
		return kind, ok
	}

	// Legacy fallback for old records that do not yet carry normalized filetype.
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

	if hasPrefix(content, []byte("%PDF-")) {
		return KindPDF, true
	}
	if isEVTX(content) {
		return KindEVTX, true
	}
	if isELF(content) {
		return KindELF, true
	}
	if isPE(content) {
		return KindPE, true
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
