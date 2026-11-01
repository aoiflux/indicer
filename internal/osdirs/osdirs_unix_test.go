//go:build !windows

package osdirs

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

// TestAccountProblems covers a uid with no account entry (LDAP or sssd
// users in a pure-Go build, containers run as an arbitrary uid) and
// entries with an unusable home directory.
func TestAccountProblems(t *testing.T) {
	for _, c := range []struct {
		name     string
		u        *user.User
		err      error
		wantHome string
	}{
		{name: "no entry", err: errors.New("user: unknown userid 4242"), wantHome: "not in the account database"},
		{name: "empty home", u: &user.User{Uid: "4242", Username: "ann"}, wantHome: "no home directory for uid 4242"},
		{name: "relative home", u: &user.User{Uid: "4242", Username: "ann", HomeDir: "home/ann"}, wantHome: "not an absolute path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			old := lookupID
			t.Cleanup(func() { lookupID = old })
			lookupID = func(string) (*user.User, error) { return c.u, c.err }

			for name, f := range map[string]func() (string, error){"Home": Home, "Cache": Cache, "LogDir": LogDir} {
				if dir, err := f(); err == nil || !strings.Contains(err.Error(), c.wantHome) {
					t.Errorf("%s = %q, %v; want an error containing %q", name, dir, err, c.wantHome)
				}
			}
			if dir, err := Temp(); dir != "/tmp" || err != nil {
				t.Errorf("Temp = %q, %v; it needs no account", dir, err)
			}
			name, err := UserName()
			if c.err != nil {
				if err == nil {
					t.Errorf("UserName = %q with no account entry", name)
				}
			} else if name != "ann" || err != nil {
				t.Errorf("UserName = %q, %v; want ann", name, err)
			}
		})
	}
}
