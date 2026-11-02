package tui

import (
	"bytes"
	"context"
	"fmt"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"indicer/internal/testutil/wcag"
	"indicer/internal/ui"
	"indicer/lib/store"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// textStyles are the styles that draw text: every lipgloss.Style field of
// st, and the text input's.
func textStyles(t *testing.T, st *styles) map[string]lipgloss.Style {
	t.Helper()
	out := map[string]lipgloss.Style{}
	v := reflect.ValueOf(st).Elem()
	styleType := reflect.TypeFor[lipgloss.Style]()
	for i := range v.NumField() {
		if f := v.Field(i); f.Type() == styleType {
			// The fields are unexported: read them through their address.
			out[v.Type().Field(i).Name] = *(*lipgloss.Style)(unsafe.Pointer(f.UnsafeAddr()))
		}
	}
	if len(out) < 20 {
		t.Fatalf("found %d styles; the struct has more", len(out))
	}
	for state, s := range map[string]struct{ Placeholder, Suggestion, Prompt, Text lipgloss.Style }{
		"focused": {st.textinput.Focused.Placeholder, st.textinput.Focused.Suggestion, st.textinput.Focused.Prompt, st.textinput.Focused.Text},
		"blurred": {st.textinput.Blurred.Placeholder, st.textinput.Blurred.Suggestion, st.textinput.Blurred.Prompt, st.textinput.Blurred.Text},
	} {
		out["input "+state+" placeholder"] = s.Placeholder
		out["input "+state+" suggestion"] = s.Suggestion
		out["input "+state+" prompt"] = s.Prompt
		out["input "+state+" text"] = s.Text
	}
	return out
}

func noColor(c color.Color) bool {
	_, none := c.(lipgloss.NoColor)
	return c == nil || none
}

// TestStyleContrast: every text colour reaches WCAG AA (4.5:1) on what it
// is drawn on, in both themes: a style with a background of its own on
// that background, any other on each reference terminal background of its
// theme. Colours are checked as sent in truecolor and as the xterm-256
// colour a 256-colour terminal shows instead. Borders are not text.
func TestStyleContrast(t *testing.T) {
	for _, dark := range []bool{true, false} {
		st := newStyles(dark)
		if st.dark != dark {
			t.Fatalf("newStyles(%v) built dark=%v", dark, st.dark)
		}
		terminal := wcag.LightBackgrounds
		if dark {
			terminal = wcag.DarkBackgrounds
		}
		for name, s := range textStyles(t, st) {
			fg := s.GetForeground()
			if noColor(fg) {
				continue
			}
			for _, p := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI256} {
				f := p.Convert(fg)
				behind := map[string]color.Color{}
				if bg := s.GetBackground(); !noColor(bg) {
					behind["its own background"] = p.Convert(bg)
				} else {
					for _, h := range terminal {
						behind[h] = wcag.RGB(h)
					}
				}
				for what, b := range behind {
					if got := wcag.Contrast(f, b); got < 4.5 {
						t.Errorf("dark=%v %s at %v: %v on %s (%v) is %.2f:1, want >= 4.5", dark, name, p, f, what, b, got)
					}
				}
			}
		}
	}
}

// fgSGR is the SGR lipgloss writes for a role's colour in a theme.
func fgSGR(r ui.Role, t ui.Theme) string {
	c := r.RGB(t)
	return fmt.Sprintf("38;2;%d;%d;%d", c.R, c.G, c.B)
}

// TestBackgroundAnswerSwitchesTheme: with --theme auto the TUI is dark until
// the terminal reports its background; a light one switches the menu, and
// any screen showing, to the light styles. Inputs and content drawn
// before the answer are redrawn.
func TestBackgroundAnswerSwitchesTheme(t *testing.T) {
	stats := Actions{Stats: func(context.Context) (*store.DBStats, error) {
		return &store.DBStats{TotalFiles: 1, CompletedFiles: 1, TotalLogicalSize: 2048, UniqueChunks: 1, TotalChunkRefs: 1}, nil
	}}
	white := tea.BackgroundColorMsg{Color: color.White}

	d := newDriver(t, stats)
	selected := fgSGR(ui.Forensics, ui.ThemeDark)
	if !d.shows(selected) || d.shows(fgSGR(ui.Forensics, ui.ThemeLight)) {
		t.Fatalf("menu before the answer is not dark:\n%q", d.view())
	}
	d.send(white)
	if d.m.env.st.dark || !d.shows(fgSGR(ui.Forensics, ui.ThemeLight)) || d.shows(selected) {
		t.Fatalf("menu after a light answer is not light:\n%q", d.view())
	}
	d.send(tea.BackgroundColorMsg{Color: color.Black})
	if !d.m.env.st.dark || !d.shows(selected) {
		t.Fatalf("menu after a dark answer is not dark:\n%q", d.view())
	}

	search := newDriver(t, Actions{})
	search.open(StateSearch)
	search.send(white)
	if !search.shows(fgSGR(ui.Muted, ui.ThemeLight)) || search.shows(fgSGR(ui.Muted, ui.ThemeDark)) {
		t.Fatalf("search screen after a light answer: the placeholder is not light:\n%q", search.view())
	}

	st := newDriver(t, stats)
	st.open(StateStats)
	st.until("the statistics load", func() bool { return st.m.screen.(*statsScreen).stats != nil })
	// Only the statistics' section headings are in the brand colour.
	if !st.shows("EVIDENCE") || !st.shows(fgSGR(ui.Forensics, ui.ThemeDark)) {
		t.Fatalf("statistics are not dark:\n%q", st.view())
	}
	st.send(white)
	if !st.shows(fgSGR(ui.Forensics, ui.ThemeLight)) || st.shows(fgSGR(ui.Forensics, ui.ThemeDark)) {
		t.Fatalf("statistics after a light answer are not light:\n%q", st.view())
	}
}

// TestThemeFlagIsKept: --theme dark or light is used as given, and an
// answer the TUI did not ask for changes nothing.
func TestThemeFlagIsKept(t *testing.T) {
	for _, c := range []struct {
		theme  ui.Theme
		answer color.Color
		light  bool
	}{
		{ui.ThemeDark, color.White, false},
		{ui.ThemeLight, color.Black, true},
	} {
		d := newThemedDriver(t, Actions{}, c.theme)
		d.send(tea.BackgroundColorMsg{Color: c.answer})
		if d.m.env.st.dark == c.light || !d.shows(fgSGR(ui.Forensics, c.theme)) {
			t.Fatalf("--theme %v: dark=%v after an answer of %v:\n%q", c.theme, d.m.env.st.dark, c.answer, d.view())
		}
	}
}

// syncBuffer is a buffer the program writes to while the test waits.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestOnlyAutoAsksTheTerminal: the program asks the terminal for its
// background (OSC 11) under --theme auto only. The question goes out as
// the program starts; the menu is drawn by then.
func TestOnlyAutoAsksTheTerminal(t *testing.T) {
	for _, c := range []struct {
		theme ui.Theme
		asks  bool
	}{
		{ui.ThemeUnknown, true},
		{ui.ThemeDark, false},
		{ui.ThemeLight, false},
	} {
		r, w := io.Pipe()
		var out syncBuffer
		done := make(chan error, 1)
		go func() {
			done <- Run(t.Context(), Config{Info: testInfo, Theme: c.theme, Options: []tea.ProgramOption{
				tea.WithInput(r), tea.WithOutput(&out), tea.WithWindowSize(100, 30),
			}})
		}()
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(out.String(), "Evidence Store") || (c.asks && !strings.Contains(out.String(), ansi.RequestBackgroundColor)) {
			if time.Now().After(deadline) {
				t.Fatalf("--theme %v: no menu, or no question, after 5s:\n%q", c.theme, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := io.WriteString(w, "q"); err != nil {
			t.Fatal(err)
		}
		if err := ended(t, done); err != nil {
			t.Fatalf("--theme %v: %v", c.theme, err)
		}
		w.Close()
		if got := strings.Contains(out.String(), ansi.RequestBackgroundColor); got != c.asks {
			t.Errorf("--theme %v: asked the terminal for its background: %v, want %v", c.theme, got, c.asks)
		}
	}
}

// emojiPresentation are the code points below U+1F000 that a terminal
// draws as emoji even without a variation selector (Emoji_Presentation=Yes
// in Unicode's emoji-data.txt). Text symbols such as ☐ and ☑ are not.
var emojiPresentation = [][2]rune{
	{0x231A, 0x231B}, {0x23E9, 0x23EC}, {0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE},
	{0x2614, 0x2615}, {0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE}, {0x26D4, 0x26D4},
	{0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5}, {0x26FA, 0x26FA}, {0x26FD, 0x26FD},
	{0x2705, 0x2705}, {0x270A, 0x270B}, {0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E},
	{0x2753, 0x2755}, {0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55},
}

// isEmoji reports whether r draws as emoji: a pictograph (U+1F000 and up,
// which also holds the flags), an emoji-presentation symbol, or a joiner or
// selector that makes a symbol emoji.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, r == 0xFE0F, r == 0x200D, r == 0x20E3:
		return true
	}
	for _, rg := range emojiPresentation {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// TestNoEmoji: the TUI draws no emoji. Their width differs between fonts
// and terminals, and they do not belong in case work; menu entries and
// titles are words.
func TestNoEmoji(t *testing.T) {
	for _, r := range []rune{'💾', '🔬', 0xFE0F, '✅', '🇬'} {
		if !isEmoji(r) {
			t.Fatalf("isEmoji(%U) = false", r)
		}
	}
	for _, r := range []rune{'☐', '☑', '…', '↑', '←', '─', '⚠'} {
		if isEmoji(r) {
			t.Fatalf("isEmoji(%U) = true for a text symbol", r)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(src), "\n") {
			for _, r := range line {
				if isEmoji(r) {
					t.Errorf("%s:%d: %U: the TUI draws no emoji", f, i+1, r)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no TUI source was checked")
	}
}
