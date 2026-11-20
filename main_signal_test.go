package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"indicer/cli"
	"indicer/internal/lifecycle"
	"indicer/lib/dbopen"
)

// TestSignalledCommandExitsWithSignalCode checks the plumbing from the root
// context to the exit code: a command whose context a signal cancelled exits
// 130 (SIGINT) or 143 (SIGTERM), not 1, and leaves the database unlocked.
func TestSignalledCommandExitsWithSignalCode(t *testing.T) {
	root := filepath.Join(t.TempDir(), "db")
	h, err := dbopen.Open(context.Background(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()

	for _, c := range []struct {
		sig  os.Signal
		code int
	}{{os.Interrupt, 130}, {syscall.SIGTERM, 143}} {
		args := []string{"--dbpath", root, "list"}
		cliDef := newCLIDefinition()
		parsed, err := cliDef.app.Parse(args)
		if err != nil {
			t.Fatal(err)
		}
		tuning, err := applyRuntimeConfig(parsed, cliDef, cmdLineSources(t, cliDef, args))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(&lifecycle.SignalError{Signal: c.sig})
		err = executeCommand(ctx, parsed, cliDef, cli.DBOptions{Path: root, Layout: tuning.layout}, tuning, sources{}, runConfig{}, nil)
		if got := lifecycle.ExitCode(err); got != c.code {
			t.Errorf("%v: exit %d (%v), want %d", c.sig, got, err, c.code)
		}
	}
	// Nothing was left holding the database.
	h, err = dbopen.Open(context.Background(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatalf("database still locked: %v", err)
	}
	h.Close()
}
