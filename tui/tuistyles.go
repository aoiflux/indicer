package tui

import (
	"image/color"

	"indicer/internal/ui"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	"charm.land/lipgloss/v2"
)

// styles are the TUI's styles for a dark or a light background. Colours
// follow the CLI's roles (internal/ui, docs/CLI_UX.md) in the theme's
// values; styles_test.go holds every text colour to WCAG AA (4.5:1) on
// what it is drawn on.
type styles struct {
	dark bool

	header, title, border, dialog lipgloss.Style
	// selected is the row or field with the focus; normal any other row.
	selected, normal                                     lipgloss.Style
	dim, help, input                                     lipgloss.Style
	success, warning, error, info                        lipgloss.Style
	button, buttonSelected, buttonDisabled               lipgloss.Style
	statSection, statLabel, statValue, statGood, statBad lipgloss.Style

	textinput textinput.Styles
	list      list.Styles
}

// greys are the xterm-256 colours that are not roles.
type greys struct {
	panel    string // behind titles and the selected row
	text     string // rows and labels
	border   string
	onAccent string // text on a coloured button
	// disabledText on disabledPanel draws a button that cannot be pressed.
	disabledText, disabledPanel string
}

var (
	darkGreys  = greys{panel: "235", text: "250", border: "240", onAccent: "235", disabledText: "248", disabledPanel: "237"}
	lightGreys = greys{panel: "255", text: "238", border: "245", onAccent: "231", disabledText: "240", disabledPanel: "253"}
)

func newStyles(dark bool) *styles {
	theme, g := ui.ThemeDark, darkGreys
	if !dark {
		theme, g = ui.ThemeLight, lightGreys
	}
	role := func(r ui.Role) color.Color { return lipgloss.Color(r.Hex(theme)) }
	accent, active := role(ui.Forensics), role(ui.Highlight)
	panel, text, muted := lipgloss.Color(g.panel), lipgloss.Color(g.text), role(ui.Muted)
	fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	button := lipgloss.NewStyle().Foreground(lipgloss.Color(g.onAccent)).Padding(0, 2).MarginRight(1)

	st := &styles{
		dark:   dark,
		header: fg(accent).Bold(true).MarginBottom(1),
		title:  fg(role(ui.Evidence)).Bold(true).Background(panel).Padding(0, 2),
		border: lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(g.border)).Padding(0, 1),
		dialog: lipgloss.NewStyle().Border(lipgloss.DoubleBorder()).BorderForeground(accent).Padding(1, 2),

		selected: fg(accent).Background(panel).Bold(true).Padding(0, 1),
		normal:   fg(text).Padding(0, 1),
		dim:      fg(muted),
		help:     fg(muted).Italic(true),
		input:    lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1),

		success: fg(role(ui.Success)).Bold(true),
		warning: fg(role(ui.Warning)).Bold(true),
		error:   fg(role(ui.Error)).Bold(true),
		info:    fg(active),

		button:         button.Background(accent),
		buttonSelected: button.Background(active).Bold(true),
		buttonDisabled: fg(lipgloss.Color(g.disabledText)).Background(lipgloss.Color(g.disabledPanel)).Padding(0, 2).MarginRight(1),

		statSection: fg(accent).Bold(true),
		statLabel:   fg(text).Width(32),
		statValue:   fg(role(ui.Evidence)).Bold(true),
		statGood:    fg(role(ui.Success)).Bold(true),
		statBad:     fg(role(ui.Error)).Bold(true),

		list: list.DefaultStyles(dark),
	}

	// Typed text and the prompt keep the terminal's own colour; the cursor
	// is reverse video, so it shows on any background.
	st.textinput = textinput.DefaultStyles(dark)
	for _, s := range []*textinput.StyleState{&st.textinput.Focused, &st.textinput.Blurred} {
		s.Placeholder, s.Suggestion = fg(muted), fg(muted)
		s.Prompt, s.Text = lipgloss.NewStyle(), lipgloss.NewStyle()
	}
	st.textinput.Cursor.Color = lipgloss.NoColor{}
	return st
}

// restyler is a screen that keeps styles of its own (an input, content
// drawn ahead), to be redrawn when the theme changes.
type restyler interface {
	restyle()
}

// newInput is a text input in the current styles.
func (e *env) newInput(placeholder string) textinput.Model {
	in := textinput.New()
	in.SetStyles(e.st.textinput)
	in.Placeholder = placeholder
	return in
}
