// Package osdirs finds the directories DUES uses when no flag names one
// (temporary files, the home directory, the user cache) and the account
// name. It asks the operating system and never reads an environment
// variable, so DUES behaves the same whatever the environment holds.
package osdirs

import (
	"fmt"
	"path/filepath"
)

// TempOr returns dir, the value of --temp-dir, when it is set, and Temp
// otherwise.
func TempOr(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	return Temp()
}

// LogDir is the default log directory, <Cache>/dues/logs.
func LogDir() (string, error) {
	cache, err := Cache()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "dues", "logs"), nil
}

// absolute checks a directory the operating system reported. An empty or
// relative path would resolve against the working directory, so DUES would
// write somewhere nobody chose.
func absolute(what, dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("the %s %q is not an absolute path", what, dir)
	}
	return filepath.Clean(dir), nil
}
