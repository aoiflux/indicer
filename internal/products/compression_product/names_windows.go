package compression_product

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// checkComponent refuses a path element Windows would misread or quietly
// change: an alternate data stream, a device name, a trailing dot or space
// (dropped), and characters the file system does not allow.
func checkComponent(c string) error {
	if strings.ContainsRune(c, ':') {
		return errors.New("':' names an alternate data stream on Windows")
	}
	if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
		return errors.New("a trailing dot or space, which Windows drops")
	}
	for _, r := range c {
		if r < 0x20 || strings.ContainsRune(`<>"|?*`, r) {
			return fmt.Errorf("character %q is not allowed on Windows", r)
		}
	}
	if !filepath.IsLocal(c) || reservedDevice(c) {
		return errors.New("a reserved device name on Windows")
	}
	return nil
}

// reservedDevice reports whether c names a DOS device. Windows before 11
// opens the device for such a name whatever extension follows it (NUL.txt),
// so the extension is ignored here even where Go's filepath.IsLocal accepts
// it.
func reservedDevice(c string) bool {
	base, _, _ := strings.Cut(c, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return strings.ContainsRune("0123456789", rune(base[3]))
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		switch base[3:] {
		case "¹", "²", "³":
			return true
		}
	}
	return false
}

// foldName is the key two entry names collide on: Windows file names ignore
// case.
func foldName(local string) string { return strings.ToLower(local) }
