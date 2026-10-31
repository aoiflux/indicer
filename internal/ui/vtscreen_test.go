package ui

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// screen is a minimal terminal for tests. It applies the escape sequences
// DUES may write (SGR, cursor up, erase in line and in display, cursor
// show and hide) and fails the test on any other, and on text that would
// wrap at the right margin. Tests then assert what a user would see.
type screen struct {
	t      *testing.T
	width  int
	mu     sync.Mutex
	lines  [][]rune
	row    int
	col    int
	hidden bool
	raw    strings.Builder
	esc    []byte // an escape sequence split across writes
}

func newScreen(t *testing.T, width int) *screen {
	return &screen{t: t, width: width, lines: [][]rune{nil}}
}

func (s *screen) Write(p []byte) (int, error) {
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
				s.t.Errorf("control byte %#x written to the terminal", b[0])
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

func (s *screen) put(r rune) {
	if s.col >= s.width {
		s.t.Errorf("text wraps at column %d: %q", s.width, string(s.lines[s.row]))
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
func (s *screen) escape(b []byte) (n int, ok bool) {
	if len(b) < 2 {
		return 0, false
	}
	if b[1] != '[' {
		s.t.Errorf("escape sequence %q is not allowed", b[:2])
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
		s.t.Errorf("escape sequence %q is not allowed", b[:i+1])
	}
	return i + 1, true
}

// text is the screen's lines, right-trimmed, without trailing empty ones.
func (s *screen) text() []string {
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

func (s *screen) String() string { return strings.Join(s.text(), "\n") }

func TestScreen(t *testing.T) {
	s := newScreen(t, 40)
	s.Write([]byte("one\ntwo\n\x1b[?25l\x1b[1;32mrow1\x1b[0m\x1b[K\nrow2\x1b[K\n"))
	s.Write([]byte("\r\x1b[2A\x1b[J"))
	s.Write([]byte("above\n\x1b[2Krow1'\x1b[K\n\x1b[?25h"))
	want := "one\ntwo\nabove\nrow1'"
	if got := s.String(); got != want || s.hidden {
		t.Fatalf("screen:\n%s\nwant:\n%s\nhidden %v", got, want, s.hidden)
	}
}
