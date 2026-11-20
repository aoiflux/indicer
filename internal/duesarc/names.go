package duesarc

import (
	"errors"
	"path/filepath"
	"strings"
)

// LocalName turns an archive entry name (slash-separated) into a local
// path, or explains why it cannot be one. "" with no error is the archive
// root itself.
func LocalName(name string) (string, error) {
	switch {
	case strings.ContainsRune(name, '\\'):
		return "", errors.New("a backslash in a name is ambiguous")
	case strings.ContainsRune(name, 0):
		return "", errors.New("a NUL byte in a name")
	case strings.HasPrefix(name, "/"):
		return "", errors.New("an absolute path")
	}
	var keep []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			return "", errors.New("a reference to a parent directory")
		}
		if err := checkComponent(c); err != nil {
			return "", err
		}
		keep = append(keep, c)
	}
	if len(keep) == 0 {
		return "", nil
	}
	local, err := filepath.Localize(strings.Join(keep, "/"))
	if err != nil {
		return "", errors.New("not a valid name on this system")
	}
	return local, nil
}
