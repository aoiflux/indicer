package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"indicer/internal/safe"
	"indicer/internal/termsafe"

	"charm.land/lipgloss/v2"
)

// clean makes a string safe to draw: file names, paths, artefact values and
// error messages come from the evidence, and an escape sequence in one must
// not reach the terminal (TUI-5).
func clean(s string) string { return termsafe.Line(s) }

// truncate cuts s to at most n runes, marking the cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// errorText is how the TUI shows an error.
func errorText(err error) string {
	var pe *safe.PanicError
	switch {
	case errors.As(err, &pe):
		return fmt.Sprintf("internal error (panic %s); the log has the details", pe.ID)
	case errors.Is(err, errCanceled):
		return "cancelled"
	case errors.Is(err, errStopping), errors.Is(err, errClosed):
		return "stopped: the TUI is closing"
	}
	return clean(err.Error())
}

// statusKind is how a status line is drawn.
type statusKind int

const (
	statusInfo statusKind = iota
	statusOK
	statusWarn
	statusError
)

type status struct {
	kind statusKind
	text string
}

func (s status) render(st *styles) string {
	if s.text == "" {
		return ""
	}
	switch s.kind {
	case statusOK:
		return st.success.Render(s.text)
	case statusWarn:
		return st.warning.Render(s.text)
	case statusError:
		return st.error.Render(s.text)
	}
	return st.info.Render(s.text)
}

func infoStatus(text string) status { return status{statusInfo, text} }
func errStatus(err error) status    { return status{statusError, "Error: " + errorText(err)} }

// outcomeLines renders an outcome's lines, each made safe and cut to width.
func outcomeLines(o Outcome, width int) string {
	if len(o.Lines) == 0 {
		return ""
	}
	lines := make([]string, len(o.Lines))
	for i, l := range o.Lines {
		lines[i] = truncate(clean(l), width)
	}
	return strings.Join(lines, "\n")
}

// frame draws content in the screen border, sized to the terminal.
func (e *env) frame(content string) string {
	f := e.st.border
	if e.width > 0 {
		f = f.Width(max(40, e.width-2))
	}
	if e.height > 0 {
		f = f.MaxHeight(max(12, e.height))
	}
	return f.Render(content)
}

// row draws a row of a list, or a label, with the focus or without.
func (e *env) row(s string, selected bool) string {
	if selected {
		return e.st.selected.Render(s)
	}
	return e.st.normal.Render(s)
}

// window returns the rows [from, to) of n to draw in rows lines so that
// selected is always among them (TUI-5).
func window(n, selected, rows int) (from, to int) {
	if rows < 1 {
		rows = 1
	}
	if n <= rows {
		return 0, n
	}
	from = selected - rows/2
	from = max(0, min(from, n-rows))
	return from, from + rows
}

// joinLines joins the non-empty parts with blank lines between them.
func joinLines(parts ...string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, strings.Join(kept, "\n\n"))
}

// since says how long ago t was, to the second.
func since(t time.Time) string {
	return time.Since(t).Round(time.Second).String()
}

// checkbox draws a toggle.
func checkbox(on bool, label string) string {
	if on {
		return "☑ " + label
	}
	return "☐ " + label
}

// button draws a button, selected or not.
func button(st *styles, label string, selected bool) string {
	if selected {
		return st.buttonSelected.Render(label)
	}
	return st.button.Render(label)
}

// disabledButton draws a button that cannot be pressed.
func disabledButton(st *styles, label string) string {
	return st.buttonDisabled.Render(label)
}
