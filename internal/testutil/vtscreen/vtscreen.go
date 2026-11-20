// Package vtscreen is a minimal terminal for tests. It applies the escape
// sequences DUES may write (SGR, cursor up, erase in line and in display,
// cursor show and hide, and, when allowed, OSC 9;4 taskbar progress ended
// by ST) and fails the test on any other, and on text that would wrap at
// the right margin. Tests then assert what a user would see.
package vtscreen

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// Screen is the terminal. It is safe for concurrent writes.
type Screen struct {
	tb      testing.TB
	width   int
	taskbar bool
	wrap    bool

	mu     sync.Mutex
	lines  [][]rune
	row    int
	col    int
	hidden bool
	states []string
	raw    strings.Builder
	esc    []byte // an escape sequence split across writes
}

// Option changes what a Screen accepts.
type Option func(*Screen)

// AllowTaskbar accepts OSC 9;4 progress ended by ST and records it.
func AllowTaskbar() Option { return func(s *Screen) { s.taskbar = true } }

// AllowWrap wraps text at the right margin as a terminal does, instead of
// failing: for a whole run, whose report lines on stdout may be long. A
// live display that wrapped would then leave its rows behind on screen.
func AllowWrap() Option { return func(s *Screen) { s.wrap = true } }

// New returns a screen width columns wide.
func New(tb testing.TB, width int, opts ...Option) *Screen {
	s := &Screen{tb: tb, width: width, lines: [][]rune{nil}}
	for _, o := range opts {
		o(s)
	}
	return s
}

// oscMax bounds an unterminated OSC: anything longer is not DUES's.
const oscMax = 64

var taskbarOSC = regexp.MustCompile(`^9;4;[0-4];\d{1,3}$`)

func (s *Screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw.Write(p)
	b := append(s.esc, p...)
	s.esc = nil
	for len(b) > 0 {
		switch b[0] {
		case '\x1b':
			n, ok := s.escape(b)
			if !ok {
				s.esc = append([]byte(nil), b...)
				return len(p), nil
			}
			b = b[n:]
			continue
		case '\r':
			s.col = 0
		case '\n':
			s.row++
			s.col = 0
			for len(s.lines) <= s.row {
				s.lines = append(s.lines, nil)
			}
		default:
			r, n := utf8.DecodeRune(b)
			if r < 0x20 || r == 0x7f {
				s.tb.Errorf("control byte %#x written to the terminal", b[0])
			} else {
				s.put(r)
			}
			b = b[n:]
			continue
		}
		b = b[1:]
	}
	return len(p), nil
}

func (s *Screen) put(r rune) {
	if s.col >= s.width {
		if !s.wrap {
			s.tb.Errorf("text wraps at column %d: %q", s.width, string(s.lines[s.row]))
		} else {
			s.row++
			s.col = 0
			for len(s.lines) <= s.row {
				s.lines = append(s.lines, nil)
			}
		}
	}
	line := s.lines[s.row]
	for len(line) <= s.col {
		line = append(line, ' ')
	}
	line[s.col] = r
	s.lines[s.row] = line
	s.col++
}

// escape applies the sequence at the start of b; ok is false when b ends
// inside it.
func (s *Screen) escape(b []byte) (n int, ok bool) {
	if len(b) < 2 {
		return 0, false
	}
	switch b[1] {
	case '[':
	case ']':
		return s.osc(b)
	default:
		s.tb.Errorf("escape sequence %q is not allowed", b[:2])
		return 2, true
	}
	i := 2
	for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == ';' || b[i] == '?') {
		i++
	}
	if i >= len(b) {
		return 0, false
	}
	params, final := string(b[2:i]), b[i]
	num := func(def int) int {
		if v, err := strconv.Atoi(params); err == nil {
			return v
		}
		return def
	}
	switch {
	case final == 'm':
	case final == 'A' && !strings.HasPrefix(params, "?"):
		s.row = max(s.row-num(1), 0)
	case final == 'K' && (params == "" || params == "0"):
		if s.col < len(s.lines[s.row]) {
			s.lines[s.row] = s.lines[s.row][:s.col]
		}
	case final == 'K' && params == "2":
		s.lines[s.row] = nil
	case final == 'J' && (params == "" || params == "0"):
		if s.col < len(s.lines[s.row]) {
			s.lines[s.row] = s.lines[s.row][:s.col]
		}
		s.lines = s.lines[:s.row+1]
	case final == 'l' && params == "?25":
		s.hidden = true
	case final == 'h' && params == "?25":
		s.hidden = false
	default:
		s.tb.Errorf("escape sequence %q is not allowed", b[:i+1])
	}
	return i + 1, true
}

// osc applies an operating system command, ESC ] ... ESC \. Only taskbar
// progress, and only when allowed, passes; a BEL terminator never does,
// because DUES writes ST.
func (s *Screen) osc(b []byte) (int, bool) {
	for i := 2; i < len(b); i++ {
		switch {
		case b[i] == '\a':
			s.tb.Errorf("OSC %q ends with BEL", b[:i+1])
			return i + 1, true
		case b[i] == '\x1b' && i+1 < len(b) && b[i+1] == '\\':
			payload := string(b[2:i])
			if s.taskbar && taskbarOSC.MatchString(payload) {
				s.states = append(s.states, strings.TrimPrefix(payload, "9;4;"))
			} else {
				s.tb.Errorf("OSC %q is not allowed", payload)
			}
			return i + 2, true
		case b[i] == '\x1b' && i+1 == len(b):
			return 0, false
		case i >= oscMax:
			s.tb.Errorf("OSC %q is too long", b[:i])
			return i, true
		}
	}
	return 0, false
}

// Finish fails the test when an escape sequence was left unfinished.
func (s *Screen) Finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.esc) > 0 {
		s.tb.Errorf("unfinished escape sequence %q", s.esc)
	}
}

// Text is the screen's lines, right-trimmed, without trailing empty ones.
func (s *Screen) Text() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.lines))
	for i, l := range s.lines {
		out[i] = strings.TrimRight(string(l), " ")
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (s *Screen) String() string { return strings.Join(s.Text(), "\n") }

// Hidden reports whether the cursor is hidden.
func (s *Screen) Hidden() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hidden
}

// Taskbar is the taskbar states written, in order: "1;45", "0;0".
func (s *Screen) Taskbar() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.states...)
}

// Raw is every byte written.
func (s *Screen) Raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw.String()
}
