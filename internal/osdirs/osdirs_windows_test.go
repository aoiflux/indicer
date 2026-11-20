//go:build windows

package osdirs

import (
	"os/user"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestMatchesKnownFolders checks, in this process's ordinary environment,
// that the folders built from the profile are the ones the known-folder
// API reports, so this machine has not moved AppData\Local.
func TestMatchesKnownFolders(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		got  func() (string, error)
		id   *windows.KNOWNFOLDERID
	}{
		{"Home", Home, windows.FOLDERID_Profile},
		{"Cache", Cache, windows.FOLDERID_LocalAppData},
	} {
		want, err := windows.KnownFolderPath(c.id, windows.KF_FLAG_DEFAULT)
		if err != nil {
			t.Fatalf("%s: known folder: %v", c.name, err)
		}
		if got, err := c.got(); err != nil || !strings.EqualFold(got, want) {
			t.Errorf("%s = %q, %v; the known folder is %q", c.name, got, err, want)
		}
	}
}

// TestHomeIsTheAccountProfile: Home, read from the token directly, is the
// folder user.Current reports for the same account.
func TestHomeIsTheAccountProfile(t *testing.T) {
	t.Parallel()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Home(); err != nil || !strings.EqualFold(got, u.HomeDir) {
		t.Errorf("Home = %q, %v; user.Current has %q", got, err, u.HomeDir)
	}
}

func TestUserNameHasNoDomain(t *testing.T) {
	t.Parallel()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	name, err := UserName()
	if err != nil || strings.Contains(name, `\`) || !strings.HasSuffix(strings.ToLower(u.Username), `\`+strings.ToLower(name)) {
		t.Errorf("UserName = %q, %v; want the name part of %q", name, err, u.Username)
	}
}
