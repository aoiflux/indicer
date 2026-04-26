package util

import (
	"bytes"
	"path/filepath"
	"strings"

	"indicer/lib/cnst"
)

func DetectFileType(name string, data []byte) string {
	if t := detectByMagic(data); t != "" {
		return t
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext != "" {
		return ext
	}

	return cnst.UnknownEvidenceType
}

func DetectEvidenceType(name string, data []byte) string {
	return DetectFileType(name, data)
}

func detectByMagic(data []byte) string {
	if len(data) == 0 {
		return ""
	}

	if hasPrefix(data, []byte("%PDF-")) {
		return "pdf"
	}
	if hasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		return "png"
	}
	if hasPrefix(data, []byte{0xff, 0xd8, 0xff}) {
		return "jpeg"
	}
	if hasPrefix(data, []byte("GIF87a")) || hasPrefix(data, []byte("GIF89a")) {
		return "gif"
	}
	if hasPrefix(data, []byte{0x1f, 0x8b}) {
		return "gzip"
	}
	if hasPrefix(data, []byte("BZh")) {
		return "bzip2"
	}
	if hasPrefix(data, []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}) {
		return "xz"
	}
	if hasPrefix(data, []byte("PK\x03\x04")) || hasPrefix(data, []byte("PK\x05\x06")) || hasPrefix(data, []byte("PK\x07\x08")) {
		return "zip"
	}
	if hasPrefix(data, []byte("7z\xbc\xaf'\x1c")) {
		return "7z"
	}
	if hasPrefix(data, []byte("Rar!\x1a\x07\x00")) || hasPrefix(data, []byte("Rar!\x1a\x07\x01\x00")) {
		return "rar"
	}
	if hasPrefix(data, []byte{0x7f, 'E', 'L', 'F'}) {
		return "elf"
	}
	if hasPrefix(data, []byte("MZ")) {
		return "pe"
	}
	if hasPrefix(data, []byte("SQLite format 3\x00")) {
		return "sqlite"
	}
	if len(data) >= 12 && hasPrefix(data, []byte("RIFF")) {
		if bytes.Equal(data[8:12], []byte("WAVE")) {
			return "wav"
		}
		if bytes.Equal(data[8:12], []byte("WEBP")) {
			return "webp"
		}
	}
	if hasPrefix(data, []byte("OggS")) {
		return "ogg"
	}
	if len(data) >= 8 && bytes.Equal(data[4:8], []byte("ftyp")) {
		return "mp4"
	}

	return ""
}

func hasPrefix(data, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	return bytes.Equal(data[:len(prefix)], prefix)
}
