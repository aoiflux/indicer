package vtscreen

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestScreen(t *testing.T) {
	t.Parallel()
	s := New(t, 40)
	s.Write([]byte("one\ntwo\n\x1b[?25l\x1b[1;32mrow1\x1b[0m\x1b[K\nrow2\x1b[K\n"))
	s.Write([]byte("\r\x1b[2A\x1b[J"))
	s.Write([]byte("above\n\x1b[2Krow1'\x1b[K\n\x1b[?25h"))
	want := "one\ntwo\nabove\nrow1'"
	if got := s.String(); got != want || s.Hidden() {
		t.Fatalf("screen:\n%s\nwant:\n%s\nhidden %v", got, want, s.Hidden())
	}
	s.Finish()
}

// Taskbar progress passes only when allowed, only ended by ST, and may be
// split across writes.
func TestTaskbar(t *testing.T) {
	t.Parallel()
	s := New(t, 40, AllowTaskbar())
	s.Write([]byte("a\x1b]9;4;1;45\x1b"))
	s.Write([]byte("\\b\x1b]9;4;0;0\x1b\\"))
	if got := s.Taskbar(); !slices.Equal(got, []string{"1;45", "0;0"}) || s.String() != "ab" {
		t.Fatalf("states %q, screen %q", got, s.String())
	}
	s.Finish()

	for name, c := range map[string]struct {
		opts  []Option
		write string
	}{
		"not allowed":    {nil, "\x1b]9;4;1;45\x1b\\"},
		"BEL":            {[]Option{AllowTaskbar()}, "\x1b]9;4;1;45\a"},
		"title":          {[]Option{AllowTaskbar()}, "\x1b]0;hostile title\x1b\\"},
		"bad state":      {[]Option{AllowTaskbar()}, "\x1b]9;4;7;45\x1b\\"},
		"unfinished OSC": {[]Option{AllowTaskbar()}, "\x1b]9;4;1;4"},
		"unfinished CSI": {nil, "\x1b[3"},
	} {
		rec := &recorder{TB: t}
		s := New(rec, 40, c.opts...)
		s.Write([]byte(c.write))
		s.Finish()
		if len(rec.errs) == 0 {
			t.Errorf("%s: %q accepted", name, c.write)
		}
	}
}

// recorder notes the errors a screen reports instead of failing the test.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func TestWrapFails(t *testing.T) {
	t.Parallel()
	rec := &recorder{TB: t}
	s := New(rec, 3)
	s.Write([]byte("abcd"))
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "wraps") {
		t.Fatalf("errors %q", rec.errs)
	}
	// Allowed, it wraps as a terminal does: a line that fills the width
	// exactly does not leave an empty one.
	s = New(t, 3, AllowWrap())
	s.Write([]byte("abcdef\r\nxyz\r\nq"))
	if got := s.String(); got != "abc\ndef\nxyz\nq" {
		t.Fatalf("wrapped screen %q", got)
	}
}
