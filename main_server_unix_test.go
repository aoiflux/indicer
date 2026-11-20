//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
)

// TestServerTokenFileAccess: a token file another account can change is
// refused, as the configuration file is, so no one else can add a token.
// One others may only read, as a mounted secret is, is taken.
func TestServerTokenFileAccess(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(p, []byte("from-file-0123456789abcdef0123456789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mode os.FileMode
		ok   bool
	}{{0o600, true}, {0o444, true}, {0o620, false}, {0o666, false}} {
		if err := os.Chmod(p, c.mode); err != nil {
			t.Fatal(err)
		}
		args := []string{"server", "--token-file", p}
		cliDef := newCLIDefinition()
		if _, err := cliDef.app.Parse(args); err != nil {
			t.Fatal(err)
		}
		set, err := flagsSetByUser(cliDef.app, args)
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolveServerTokens(cliDef.flags, sources{set: set})
		if c.ok != (err == nil) || (!c.ok && (lifecycle.ExitCode(err) != lifecycle.ExitUsage || !strings.Contains(err.Error(), "chmod go-w"))) {
			t.Errorf("mode %04o: %v", c.mode, err)
		}
	}
}
