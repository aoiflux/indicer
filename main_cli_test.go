package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"indicer/internal/ui"
)

func TestPrintHelpForPath_NearInRoutesToNearInHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"near", "in"})
	})
	if !strings.Contains(out, "Command: near in") {
		t.Fatalf("expected near in help header, got: %s", out)
	}
}

func TestPrintHelpForPath_MicroExtractRoutesToMicroExtractHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"micro", "extract"})
	})
	if !strings.Contains(out, "Command: micro extract") {
		t.Fatalf("expected micro extract help header, got: %s", out)
	}
}

func TestPrintHelpForPath_MicroListRoutesToMicroListHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"micro", "list"})
	})
	if !strings.Contains(out, "Command: micro list") {
		t.Fatalf("expected micro list help header, got: %s", out)
	}
}

func TestPrintHelpForPath_UnknownFallsBackToRootHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"does-not-exist"})
	})
	if !strings.Contains(out, "\n"+ui.Identity(buildInfo())+"\nReliable evidence storage") {
		t.Fatalf("expected root help fallback, got: %s", out)
	}
}

// Piped, the root help opens with the identity line, with no art and no
// blank line before it.
func TestPipedRootHelpStartsWithIdentity(t *testing.T) {
	out := captureStdout(t, printRootHelp)
	if !strings.HasPrefix(out, ui.Identity(buildInfo())+"\nReliable evidence storage") {
		t.Fatalf("piped root help starts:\n%s", out[:min(len(out), 200)])
	}
}

// Piped, `dues version` is the identity line alone. On a terminal under
// --plain it heads the overview instead.
func TestVersionPage(t *testing.T) {
	if out := captureStdout(t, printVersionInfo); out != ui.Identity(buildInfo())+"\n" {
		t.Errorf("piped dues version:\n%s", out)
	}
	defer ui.SetForTest(ui.Mode{Out: ui.Caps{TTY: true}, Plain: true})()
	out := captureStdout(t, printVersionInfo)
	if !strings.HasPrefix(out, ui.Identity(buildInfo())+"\n") || !strings.Contains(out, "Overview") {
		t.Errorf("dues version on a terminal under --plain:\n%s", out)
	}
}

// TestEveryCommandHasHelp checks that `dues help` knows every command the
// parser defines. The help text is written by hand, apart from the parser,
// and `dues help repair` and `dues help reindex` printed "Unknown command".
func TestEveryCommandHasHelp(t *testing.T) {
	for _, c := range newCLIDefinition().app.Model().Commands {
		if c.Hidden {
			continue
		}
		paths := [][]string{{c.Name}}
		for _, sub := range c.Commands {
			if !sub.Hidden {
				paths = append(paths, []string{c.Name, sub.Name})
			}
		}
		for _, path := range paths {
			name := strings.Join(path, " ")
			out := captureStdout(t, func() { printHelpForPath(path) })
			if !strings.Contains(out, "Command: "+name+"\n") {
				t.Errorf("dues help %s does not print its own help", name)
			}
		}
	}
}

// The extras are chosen by command name: each name they use must still be
// a command that shows progress, so a renamed command is caught here.
func TestEggCommandsAreCommands(t *testing.T) {
	def := newCLIDefinition()
	known := map[string]bool{}
	for _, c := range def.app.Model().Commands {
		known[c.Name] = true
		for _, sub := range c.Commands {
			known[c.Name+" "+sub.Name] = true
		}
	}
	for _, name := range ui.EggCommands() {
		if !known[name] || !showsProgress(name, def) {
			t.Errorf("the extras name %q, which is not a command that shows progress", name)
		}
	}
	if eggsAllowed(def.commands.repair.FullCommand(), def) {
		t.Error("an extra may follow repair, whose output scripts read")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	stdout, _ := captureOutput(t, fn)
	return stdout
}

// captureOutput runs fn with os.Stdout and os.Stderr on pipes and returns
// what it wrote to each.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldStdout, oldStderr := os.Stdout, os.Stderr
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe create failed: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe create failed: %v", err)
	}
	os.Stdout, os.Stderr = outW, errW

	// Read while fn writes: output larger than the pipe's buffer would
	// otherwise block fn for ever.
	type result struct {
		b   []byte
		err error
	}
	drain := func(r *os.File) chan result {
		read := make(chan result, 1)
		go func() {
			b, err := io.ReadAll(r)
			read <- result{b, err}
		}()
		return read
	}
	outRead, errRead := drain(outR), drain(errR)

	fn()

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldStdout, oldStderr

	out, errOut := <-outRead, <-errRead
	if out.err != nil || errOut.err != nil {
		t.Fatalf("read output failed: %v, %v", out.err, errOut.err)
	}
	_ = outR.Close()
	_ = errR.Close()
	return string(out.b), string(errOut.b)
}
