package main

import (
	"slices"
	"strings"
	"testing"

	"indicer/internal/ui"
)

// TestHelpTopic checks which command lines are help requests, and for which
// command, with flags and their values anywhere before the request. "dues
// -d x help" and "dues -d x" used to fall through to kingpin's own usage
// text, and "dues -d x store --help" asked for the command "-d x store".
func TestHelpTopic(t *testing.T) {
	t.Parallel()
	model := newCLIDefinition().app.Model()
	help := func(topic ...string) []string { return append([]string{}, topic...) }
	cases := []struct {
		args  []string
		topic []string // nil: not a help request DUES answers
	}{
		{nil, help()},
		{[]string{"help"}, help()},
		{[]string{"--help"}, help()},
		{[]string{"-h"}, help()},
		{[]string{"help", "store"}, help("store")},
		{[]string{"help", "near", "in"}, help("near", "in")},
		{[]string{"store", "--help"}, help("store")},
		{[]string{"store", "FILE", "-h"}, help("store", "FILE")},
		{[]string{"help", "store", "--help"}, help("store")},

		{[]string{"-d", "x", "help"}, help()},
		{[]string{"-d", "x"}, help()},
		{[]string{"-dx", "help"}, help()},
		{[]string{"--dbpath", "x", "help", "store"}, help("store")},
		{[]string{"--dbpath=x", "store", "--help"}, help("store")},
		{[]string{"-d", "x", "store", "--help"}, help("store")},
		{[]string{"store", "-w", "4", "--help"}, help("store")},
		{[]string{"--log-dir", "L", "--debug", "near", "in", "--help"}, help("near", "in")},
		{[]string{"--no-debug", "help"}, help()},
		{[]string{"--debug=true", "help"}, help()},
		{[]string{"-Q", "help"}, help()},

		// Not help: kingpin runs the command, or reports what is wrong.
		{[]string{"store", "FILE"}, nil},
		{[]string{"list"}, nil},
		{[]string{"-d", "x", "list"}, nil},
		{[]string{"store", "--", "--help"}, nil},
		{[]string{"--version"}, nil},
		{[]string{"-v"}, nil},
		{[]string{"-d", "x", "--version"}, nil},
		{[]string{"--completion-bash"}, nil},
		{[]string{"--help-man"}, nil},
		{[]string{"--bogus", "help"}, nil},
		{[]string{"-%", "help"}, nil},
		{[]string{"--debug=yes", "help"}, nil},
		{[]string{"-d"}, nil},
		{[]string{"help", "store", "-d"}, nil},
	}
	for _, c := range cases {
		topic, ok := helpTopic(c.args, model)
		if want := c.topic != nil; ok != want || ok && !slices.Equal(topic, c.topic) {
			t.Errorf("helpTopic(%q) = %q, %v; want %q, %v", c.args, topic, ok, c.topic, want)
		}
	}
}

// A help request after a global flag prints DUES's own help on stdout, as
// it does without the flag, and nothing on stderr.
func TestHelpAfterGlobalFlags(t *testing.T) {
	db := t.TempDir()
	root := ui.Identity(buildInfo()) + "\nReliable evidence storage"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-d", db, "help"}, root},
		{[]string{"-d", db}, root},
		{[]string{"-d", db, "store", "--help"}, "Command: store\n"},
		{[]string{"--log-dir", db, "help", "near", "in"}, "Command: near in\n"},
	}
	for _, c := range cases {
		var code int
		stdout, stderr := captureOutput(t, func() { code = realMain(c.args) })
		if code != 0 || !strings.Contains(stdout, c.want) || stderr != "" {
			t.Errorf("dues %s: exit %d, stdout %.120q, stderr %.200q; want exit 0 and %q on stdout",
				strings.Join(c.args, " "), code, stdout, stderr, c.want)
		}
	}
}
