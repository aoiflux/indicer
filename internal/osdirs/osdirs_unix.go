//go:build !windows

package osdirs

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
)

// Temp is /tmp. TMPDIR is not read: pass --temp-dir to use another
// directory.
func Temp() (string, error) { return "/tmp", nil }

// Home is the home directory in the account database (/etc/passwd, or NSS
// in a cgo build). HOME is not read.
func Home() (string, error) {
	u, err := account()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("no home directory for uid %s in the account database", u.Uid)
	}
	return absolute("home directory of uid "+u.Uid, u.HomeDir)
}

// Cache is ~/Library/Caches on macOS and ~/.cache elsewhere.
// XDG_CACHE_HOME is not read.
func Cache() (string, error) {
	home, err := Home()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Caches"), nil
	}
	return filepath.Join(home, ".cache"), nil
}

// UserName is the account name in the account database. USER and LOGNAME
// are not read.
func UserName() (string, error) {
	u, err := account()
	if err != nil {
		return "", fmt.Errorf("find the account name: %w", err)
	}
	if u.Username == "" {
		return "", fmt.Errorf("no account name for uid %s in the account database", u.Uid)
	}
	return u.Username, nil
}

// lookupID is user.LookupId; tests replace it.
var lookupID = user.LookupId

// account looks the process's uid up. user.Current is not used: when the
// lookup fails it falls back to $USER and $HOME.
func account() (*user.User, error) {
	uid := strconv.Itoa(os.Getuid())
	u, err := lookupID(uid)
	if err != nil {
		return nil, fmt.Errorf("uid %s is not in the account database: %w", uid, err)
	}
	return u, nil
}
