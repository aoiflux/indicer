package ui

import (
	"fmt"
	"io"

	"indicer/internal/console"
	"indicer/internal/termsafe"
)

// Level is the kind of a side message.
type Level uint8

const (
	LevelNote Level = iota
	LevelWarning
	LevelError
)

func (l Level) label() string {
	switch l {
	case LevelWarning:
		return "warning"
	case LevelError:
		return "error"
	}
	return "note"
}

// Message writes one message line to w. On a terminal it reads
// "▲ warning: text" with the mark and label coloured and the text in the
// terminal's own colour. Elsewhere it reads "dues: warning: text", the
// form scripts and logs already match. The text is sanitised: evidence
// names reach these lines inside errors.
func Message(w io.Writer, l Level, text string) {
	p := For(w)
	text = termsafe.Block(text)
	if l == LevelError {
		// Start on a fresh line: an older progress bar may have left its
		// line unfinished.
		fmt.Fprintln(w)
	}
	g := p.Glyphs()
	role, mark := Info, g.Note
	switch l {
	case LevelWarning:
		role, mark = Warning, g.Warn
	case LevelError:
		role, mark = Error, g.Fail
	}
	if !p.Decorated() {
		// The label is coloured only under --color=always.
		fmt.Fprintf(w, "dues: %s %s\n", p.Strong(role, l.label()+":"), text)
		return
	}
	fmt.Fprintf(w, "%s %s %s\n", p.Strong(role, mark), p.Strong(role, l.label()+":"), text)
}

// Notef, Warnf and Errorf write a message to stderr.
func Notef(format string, a ...any)  { Message(console.Err(), LevelNote, fmt.Sprintf(format, a...)) }
func Warnf(format string, a ...any)  { Message(console.Err(), LevelWarning, fmt.Sprintf(format, a...)) }
func Errorf(format string, a ...any) { Message(console.Err(), LevelError, fmt.Sprintf(format, a...)) }
