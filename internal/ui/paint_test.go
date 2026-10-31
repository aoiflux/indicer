package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"indicer/internal/console"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// terminal makes console's sinks buffers and m current, as if both
// streams were the terminal m describes.
func terminal(t *testing.T, m Mode) (out, errb *bytes.Buffer) {
	t.Helper()
	out, errb = new(bytes.Buffer), new(bytes.Buffer)
	console.Configure(out, errb)
	t.Cleanup(console.Reset)
	t.Cleanup(SetForTest(m))
	return out, errb
}

func ttyMode(p colorprofile.Profile, th Theme, g GlyphSet) Mode {
	c := Caps{TTY: true, Profile: p, Glyphs: g, Motion: true}
	return Mode{Out: c, Err: c, Theme: th, Interactive: true}
}

var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestPainterOnlyColoursConsoleSinks(t *testing.T) {
	terminal(t, ttyMode(colorprofile.TrueColor, ThemeDark, GlyphsFull))
	if got := For(&bytes.Buffer{}).Strong(Error, "x"); got != "x" {
		t.Fatalf("a buffer got %q", got)
	}
	if p := For(&bytes.Buffer{}); p.Decorated() || p.GlyphSet() != GlyphsASCII {
		t.Fatal("a buffer is decorated or not ASCII")
	}
	if got := For(console.Out()).Paint(Info, "x"); got != "\x1b[38;2;108;168;255mx\x1b[0m" {
		t.Fatalf("stdout got %q", got)
	}
	if got := For(console.Err()).Strong(Error, "x"); got != "\x1b[1;38;2;255;107;107mx\x1b[0m" {
		t.Fatalf("stderr got %q", got)
	}
}

func TestPainterProfiles(t *testing.T) {
	cases := []struct {
		p                    colorprofile.Profile
		th                   Theme
		paint, strong, plain string
	}{
		{colorprofile.NoTTY, ThemeDark, "x", "x", "x"},
		{colorprofile.Ascii, ThemeDark, "x", "\x1b[1mx\x1b[0m", "x"},
		{colorprofile.ANSI, ThemeDark, "\x1b[93mx\x1b[0m", "\x1b[1;93mx\x1b[0m", "x"},
		// The neutral palette is tuned for bold tokens: Paint is bold too.
		{colorprofile.ANSI, ThemeUnknown, "\x1b[1;93mx\x1b[0m", "\x1b[1;93mx\x1b[0m", "x"},
	}
	for _, c := range cases {
		terminal(t, ttyMode(c.p, c.th, GlyphsFull))
		p := For(console.Out())
		if got := p.Paint(Evidence, "x"); got != c.paint {
			t.Errorf("%v/%d Paint = %q, want %q", c.p, c.th, got, c.paint)
		}
		if got := p.Strong(Evidence, "x"); got != c.strong {
			t.Errorf("%v/%d Strong = %q, want %q", c.p, c.th, got, c.strong)
		}
		if got := p.Paint(Plain, "x"); got != c.plain {
			t.Errorf("%v/%d Paint(Plain) = %q, want %q", c.p, c.th, got, c.plain)
		}
	}
}

// A hash or ID is one token: no escape code inside it, so copying it from
// the terminal gives exactly the value.
func TestNoEscapesInsideTokens(t *testing.T) {
	terminal(t, ttyMode(colorprofile.TrueColor, ThemeUnknown, GlyphsFull))
	const hash = "9f2c4e81d0a7b3c55e6f1a2b3c4d5e6f"
	got := For(console.Out()).Paint(Evidence, hash)
	if !strings.Contains(got, hash) || ansi.Strip(got) != hash {
		t.Fatalf("token split by escapes: %q", got)
	}
}

func TestGlyphSets(t *testing.T) {
	for set, g := range glyphSets {
		marks := append([]string{g.OK, g.Fail, g.Warn, g.Note, g.Interrupted, g.Pending, g.Skipped,
			g.Brand, g.Sep, g.Rule, g.BarFull, g.BarTip, g.BarEmpty}, g.Spinner...)
		for _, m := range marks {
			if w := ansi.StringWidth(m); w != 1 {
				t.Errorf("set %d: %q is %d columns wide", set, m, w)
			}
			if GlyphSet(set) == GlyphsASCII && !isASCII(m) {
				t.Errorf("ASCII set has %q", m)
			}
			if GlyphSet(set) == GlyphsConsole && !isASCII(m) && !strings.Contains("·─█░", m) {
				t.Errorf("console set has %q, which is not in CP437", m)
			}
		}
		if w := ansi.StringWidth(g.Arrow); w < 1 || w > 2 {
			t.Errorf("set %d: arrow %q is %d columns wide", set, g.Arrow, w)
		}
		if ansi.StringWidth(g.TreeMid) != 3 || ansi.StringWidth(g.TreeEnd) != 3 || ansi.StringWidth(g.TreeBar) != 4 {
			t.Errorf("set %d: tree pieces %q %q %q are not 3, 3 and 4 columns", set, g.TreeMid, g.TreeEnd, g.TreeBar)
		}
		for _, m := range []string{g.TreeMid, g.TreeEnd, g.TreeBar, g.Ellipsis, g.Times} {
			if GlyphSet(set) == GlyphsASCII && !isASCII(m) {
				t.Errorf("ASCII set has %q", m)
			}
			if GlyphSet(set) == GlyphsConsole && !isASCII(strings.Trim(m, "├└│─ ")) {
				t.Errorf("console set has %q, which is not in CP437", m)
			}
		}
	}
	if GlyphSet(99).Glyphs().OK != "+" {
		t.Fatal("an unknown set is not ASCII")
	}
}

func isASCII(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func TestMessage(t *testing.T) {
	var buf bytes.Buffer
	Message(&buf, LevelWarning, "index incomplete")
	Message(&buf, LevelError, "boom\x1b[2J")
	Message(&buf, LevelNote, "a\nb")
	want := "dues: warning: index incomplete\n\ndues: error: boom\\x1b[2J\ndues: note: a\nb\n"
	if buf.String() != want {
		t.Fatalf("plain messages:\n%q\nwant\n%q", buf.String(), want)
	}

	_, errb := terminal(t, ttyMode(colorprofile.TrueColor, ThemeDark, GlyphsFull))
	Warnf("stored with %d warnings", 2)
	Errorf("disk full")
	Notef("database %s is not encrypted", "x")
	got := ansi.Strip(errb.String())
	want = "▲ warning: stored with 2 warnings\n\n✗ error: disk full\n● note: database x is not encrypted\n"
	if got != want {
		t.Fatalf("terminal messages:\n%q\nwant\n%q", got, want)
	}
	if !sgrRE.MatchString(errb.String()) {
		t.Fatal("terminal messages have no colour")
	}
	if strings.Contains(errb.String(), "\x1b[1;38;2;255;107;107mdisk") {
		t.Fatal("the message text is coloured; only the mark and label may be")
	}
}

func TestMessagePlainMode(t *testing.T) {
	m := ttyMode(colorprofile.NoTTY, ThemeUnknown, GlyphsASCII)
	m.Plain = true
	_, errb := terminal(t, m)
	Warnf("x")
	if errb.String() != "dues: warning: x\n" {
		t.Fatalf("--plain message = %q", errb.String())
	}
}

func TestRunHeader(t *testing.T) {
	h := Header{
		Version: "0.39.0", Command: "store", Subject: "evil\x1b]0;x\x07.dd", Target: `O:\cases\c42`,
		Settings: []Setting{{"mode", "performance", "performance"}, {"compress", "best", "zstd best"}},
	}
	var buf bytes.Buffer
	RunHeader(&buf, h)
	want := "dues: store: DUES 0.39.0, evil\\x1b]0;x\\x07.dd -> O:\\cases\\c42\n" +
		"dues: store: settings: mode=performance compress=best\n"
	if buf.String() != want {
		t.Fatalf("plain header:\n%q\nwant\n%q", buf.String(), want)
	}

	_, errb := terminal(t, ttyMode(colorprofile.TrueColor, ThemeDark, GlyphsFull))
	RunHeader(console.Err(), h)
	got := ansi.Strip(errb.String())
	want = "◆ DUES v0.39.0 · store · evil\\x1b]0;x\\x07.dd → O:\\cases\\c42\n  performance · zstd best\n"
	if got != want {
		t.Fatalf("terminal header:\n%q\nwant\n%q", got, want)
	}
	for _, out := range []string{buf.String(), got} {
		if strings.ContainsAny(out, "{}") {
			t.Fatalf("header has a brace (the backcompat test parses list output by braces): %q", out)
		}
	}

	// On a narrow terminal the lines break at spaces; none is wider.
	t.Setenv("COLUMNS", "30")
	errb.Reset()
	h.Target = "/tmp/TestLiveDisplayInPTY2895467/001/db"
	RunHeader(console.Err(), h)
	for _, l := range strings.Split(strings.TrimSuffix(ansi.Strip(errb.String()), "\n"), "\n") {
		if ansi.StringWidth(l) > 29 {
			t.Errorf("header line %q is wider than the terminal", l)
		}
	}
}
