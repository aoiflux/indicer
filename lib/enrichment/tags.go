package enrichment

import (
	"mime"
	"path/filepath"
	"sort"
	"strings"
)

var mimeByIndexedType = map[string]string{
	"pdf":    "application/pdf",
	"png":    "image/png",
	"jpeg":   "image/jpeg",
	"gif":    "image/gif",
	"zip":    "application/zip",
	"7z":     "application/x-7z-compressed",
	"rar":    "application/vnd.rar",
	"gzip":   "application/gzip",
	"bzip2":  "application/x-bzip2",
	"xz":     "application/x-xz",
	"sqlite": "application/vnd.sqlite3",
	"txt":    "text/plain",
	"json":   "application/json",
	"xml":    "application/xml",
	"csv":    "text/csv",
	"html":   "text/html",
	"js":     "text/javascript",
	"py":     "text/x-python",
	"ps1":    "text/x-powershell",
	"sh":     "text/x-shellscript",
}

func classifyFile(filePath, indexedType string) (string, []string) {
	mimeType := inferMimeType(filePath, indexedType)
	lowerPath := strings.ToLower(filePath)
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(lowerPath), "."))

	tags := make(map[string]struct{}, 6)

	if strings.HasPrefix(mimeType, "image/") || isOneOf(ext, "png", "jpg", "jpeg", "gif", "bmp", "tif", "tiff", "webp", "svg", "heic") {
		tags["image"] = struct{}{}
	}

	if strings.HasPrefix(mimeType, "text/") || strings.HasPrefix(mimeType, "application/pdf") ||
		isOneOf(ext, "pdf", "txt", "md", "rtf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "odt", "csv", "json", "xml", "yml", "yaml", "log") {
		tags["document"] = struct{}{}
	}

	if strings.Contains(lowerPath, "credential") || strings.Contains(lowerPath, "password") || strings.Contains(lowerPath, "secret") ||
		strings.Contains(lowerPath, "token") || strings.Contains(lowerPath, "apikey") || strings.Contains(lowerPath, "api-key") ||
		strings.Contains(lowerPath, "auth") || strings.Contains(lowerPath, "login") ||
		isOneOf(ext, "kdbx", "pem", "key", "pfx", "p12", "ovpn", "ppk") {
		tags["credential"] = struct{}{}
	}

	if strings.HasPrefix(mimeType, "application/zip") || strings.HasPrefix(mimeType, "application/gzip") || strings.HasPrefix(mimeType, "application/x-") ||
		isOneOf(ext, "zip", "7z", "rar", "tar", "gz", "bz2", "xz", "tgz", "cab", "iso") {
		tags["archive"] = struct{}{}
	}

	if strings.HasPrefix(mimeType, "text/x-") || strings.Contains(mimeType, "javascript") ||
		isOneOf(ext, "ps1", "psm1", "bat", "cmd", "sh", "bash", "zsh", "ksh", "py", "js", "vbs", "rb", "pl", "php", "lua") {
		tags["script"] = struct{}{}
	}

	if strings.Contains(lowerPath, "browser") || strings.Contains(lowerPath, "chrome") || strings.Contains(lowerPath, "chromium") ||
		strings.Contains(lowerPath, "firefox") || strings.Contains(lowerPath, "edge") || strings.Contains(lowerPath, "safari") ||
		strings.Contains(lowerPath, "cookies") || strings.Contains(lowerPath, "history") || strings.Contains(lowerPath, "bookmarks") ||
		strings.Contains(lowerPath, "webcache") || strings.Contains(lowerPath, "places.sqlite") ||
		(ext == "sqlite" && (strings.Contains(lowerPath, "profile") || strings.Contains(lowerPath, "cache"))) {
		tags["browser artefact"] = struct{}{}
	}

	out := make([]string, 0, len(tags))
	for tag := range tags {
		out = append(out, tag)
	}
	sort.Strings(out)
	return mimeType, out
}

func inferMimeType(filePath, indexedType string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	if ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			if i := strings.IndexByte(t, ';'); i > 0 {
				return t[:i]
			}
			return t
		}
	}

	if t, ok := mimeByIndexedType[strings.ToLower(indexedType)]; ok {
		return t
	}

	return "application/octet-stream"
}

func isOneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}
