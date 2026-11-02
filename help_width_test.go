package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"indicer/internal/ui"
)

// TestHelpFitsTheTerminal checks that no line of any help page is wider
// than helpWidth, so help reads unbroken in an 80-column terminal.
func TestHelpFitsTheTerminal(t *testing.T) {
	pages := map[string]func(){
		"help": printRootHelp,
		"version": func() {
			defer ui.SetForTest(ui.Mode{Out: ui.Caps{TTY: true}, Plain: true})()
			printVersionInfo()
		},
	}
	for _, c := range newCLIDefinition().app.Model().Commands {
		paths := [][]string{{c.Name}}
		for _, sub := range c.Commands {
			paths = append(paths, []string{c.Name, sub.Name})
		}
		for _, path := range paths {
			pages["help "+strings.Join(path, " ")] = func() { printHelpForPath(path) }
		}
	}
	for name, page := range pages {
		out := captureStdout(t, page)
		for i, line := range strings.Split(out, "\n") {
			if w := ansi.StringWidth(line); w > helpWidth {
				t.Errorf("dues %s, line %d is %d columns wide:\n%s", name, i+1, w, line)
			}
		}
		// Prose is wrapped where it is written: wrapHelpLine would leave a
		// word alone on a line in the middle of a paragraph. Only flag and
		// command rows and synopses may need it.
		out = captureStdout(t, func() {
			defer func(w io.Writer) { helpOut = w }(helpOut)
			helpOut = stdoutNow{}
			page()
		})
		for i, line := range strings.Split(out, "\n") {
			plain := ansi.Strip(line)
			indent := len(plain) - len(strings.TrimLeft(plain, " "))
			if ansi.StringWidth(line) > helpWidth && hangColumn(plain) == indent {
				t.Errorf("dues %s, line %d: prose wider than %d columns; wrap it in help.go:\n%s", name, i+1, helpWidth, line)
			}
		}
	}
}

// stdoutNow writes to os.Stdout as it is when written, which captureOutput
// replaces.
type stdoutNow struct{}

func (stdoutNow) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

func TestWrapHelpLine(t *testing.T) {
	desc := strings.Repeat("word ", 20) + "end"
	cases := []struct {
		name, line string
		hang       int
	}{
		{"flag row", "  --config FILE    " + desc, len("  --config FILE    ")},
		{"usage", "Usage: dues store FILE|FOLDER [--no-index|-N] [--store-workers|-w N] " + desc, len("Usage: ")},
		{"prose", "  " + desc, 2},
		{"coloured flag row", "  \x1b[36m--plain\x1b[m           " + desc, len("  --plain           ")},
	}
	for _, c := range cases {
		lines := wrapHelpLine(c.line, helpWidth)
		if len(lines) < 2 {
			t.Errorf("%s: not wrapped: %q", c.name, lines)
			continue
		}
		for i, l := range lines {
			if ansi.StringWidth(l) > helpWidth {
				t.Errorf("%s: line %d is %d columns wide", c.name, i+1, ansi.StringWidth(l))
			}
			if i > 0 && !strings.HasPrefix(l, strings.Repeat(" ", c.hang)) || i > 0 && l[c.hang] == ' ' {
				t.Errorf("%s: line %d is not indented to column %d: %q", c.name, i+1, c.hang, l)
			}
		}
		// The words and their order survive; only spaces change.
		if got, want := strings.Fields(ansi.Strip(strings.Join(lines, "\n"))), strings.Fields(ansi.Strip(c.line)); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: wrapping changed the text:\n got %q\nwant %q", c.name, got, want)
		}
		if strings.Contains(strings.Join(lines, "\n"), "--store-\n") {
			t.Errorf("%s: a flag name was broken at its hyphen", c.name)
		}
	}
	// A synopsis breaks between options, never inside one.
	usage := "Usage: dues store FILE|FOLDER [--no-index|-N] [--require-verified] [--fts|-F] [--enrich|-E] [--hash-algo|-g [sha3|blake3]] [--hash-strategy|-S [sync|async|hocho]] [global options]"
	for i, l := range wrapHelpLine(usage, helpWidth) {
		if strings.Count(l, "[") != strings.Count(l, "]") {
			t.Errorf("synopsis line %d breaks inside an option: %q", i+1, l)
		}
	}
	if got := wrapHelpLine("  short line", helpWidth); len(got) != 1 || got[0] != "  short line" {
		t.Errorf("a line that fits changed: %q", got)
	}
}
