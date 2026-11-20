package ui

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Frame is one picture of an animation: its rows, shown for Hold.
type Frame struct {
	Rows []string
	Hold time.Duration
}

// Animation is a short drawing on stderr that erases itself.
type Animation struct {
	Name   string
	Frames []Frame
}

// The limits every animation keeps: tests check the catalogue, and play
// enforces the time limit. Under three changes a second of a few cells is
// far from what WCAG 2.3.1 calls flashing.
const (
	maxAnimation = 1500 * time.Millisecond
	minFrame     = 80 * time.Millisecond
	maxRows      = 4
	maxCols      = 40
)

func (a Animation) duration() time.Duration {
	var d time.Duration
	for _, f := range a.Frames {
		d += f.Hold
	}
	return d
}

func (a Animation) rows() int {
	n := 0
	for _, f := range a.Frames {
		n = max(n, len(f.Rows))
	}
	return min(n, maxRows)
}

// sleeper waits d, or until ctx ends; false means ctx ended.
type sleeper func(ctx context.Context, d time.Duration) bool

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// play draws a on w, in place: it reserves the rows, hides the cursor,
// draws each frame over the last, then erases every row and shows the
// cursor again, leaving the cursor where it started. ctx ending stops it at
// once, with the same cleanup. Only \r, \n, ESC[nA, ESC[2K, ESC[J and
// ESC[?25l/h are written.
func play(ctx context.Context, w io.Writer, a Animation, sleep sleeper) {
	rows := a.rows()
	if rows == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, maxAnimation)
	defer cancel()
	up := "\r"
	if rows > 1 {
		up = "\r\x1b[" + strconv.Itoa(rows-1) + "A"
	}
	_, _ = io.WriteString(w, "\x1b[?25l"+strings.Repeat("\n", rows-1))
	defer func() { _, _ = io.WriteString(w, up+"\x1b[J\x1b[?25h") }()
	for _, f := range a.Frames {
		var b strings.Builder
		b.WriteString(up)
		for i := range rows {
			b.WriteString("\x1b[2K")
			if i < len(f.Rows) {
				b.WriteString(ansi.Truncate(f.Rows[i], maxCols, ""))
			}
			if i < rows-1 {
				b.WriteString("\n")
			}
		}
		_, _ = io.WriteString(w, b.String())
		if !sleep(ctx, f.Hold) {
			return
		}
	}
}
