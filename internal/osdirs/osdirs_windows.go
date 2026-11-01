//go:build windows

package osdirs

import (
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
)

// Every lookup starts from user.Current, which on Windows reads the process
// token and the profile list, not the environment.
// The known-folder API is not used: it expands the registry's
// %USERPROFILE% with the process environment, so a junk USERPROFILE moved
// or broke the local application data folder.

// Temp is AppData\Local\Temp in the profile, where Windows puts a user's
// TMP by default. GetTempPath is not used: it reads TMP and TEMP.
func Temp() (string, error) {
	local, err := Cache()
	if err != nil {
		return "", err
	}
	return filepath.Join(local, "Temp"), nil
}

// Home is the profile folder of the process token's account. USERPROFILE,
// HOMEDRIVE and HOMEPATH are not read.
func Home() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("find the profile folder: %w", err)
	}
	return absolute("profile folder", u.HomeDir)
}

// Cache is AppData\Local in the profile. LOCALAPPDATA is not read.
// Windows does not support redirecting this folder elsewhere.
func Cache() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "AppData", "Local"), nil
}

// UserName is the account name of the process token, without its domain.
// USERNAME is not read.
func UserName() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("find the account name: %w", err)
	}
	name := u.Username[strings.LastIndexByte(u.Username, '\\')+1:]
	if name == "" {
		return "", fmt.Errorf("the process token has no account name (%q)", u.Username)
	}
	return name, nil
}
