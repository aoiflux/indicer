package ui

// Glyphs are the marks one glyph set can show. Every mark is one column
// wide, and every state shown with a mark is also named in words.
type Glyphs struct {
	OK, Fail, Warn, Note, Interrupted, Pending, Skipped string
	// Brand opens the run header.
	Brand string
	// Sep separates fields; Arrow points from a source to a target; Rule
	// draws horizontal lines.
	Sep, Arrow, Rule string
	// Spinner frames for work of unknown size.
	Spinner []string
	// BarFull, BarTip and BarEmpty draw a progress bar.
	BarFull, BarTip, BarEmpty string
	// TreeMid, TreeEnd (3 columns) and TreeBar (4 columns) draw trees.
	TreeMid, TreeEnd, TreeBar string
	// Ellipsis marks cut text; Times precedes a count ("×3"); Dash
	// introduces an attribution.
	Ellipsis, Times, Dash string
}

var glyphSets = [...]Glyphs{
	GlyphsASCII: {
		OK: "+", Fail: "x", Warn: "!", Note: "*", Interrupted: "#", Pending: ".", Skipped: "-",
		Brand: "*", Sep: "-", Arrow: "->", Rule: "-",
		Spinner: []string{"|", "/", "-", `\`},
		BarFull: "#", BarTip: ">", BarEmpty: "-",
		TreeMid: "|- ", TreeEnd: "`- ", TreeBar: "|   ",
		Ellipsis: "...", Times: "x", Dash: "--",
	},
	// The console host has no font fallback and raster fonts follow CP437:
	// ASCII marks, and the CP437 middle dot and block characters.
	GlyphsConsole: {
		OK: "+", Fail: "x", Warn: "!", Note: "*", Interrupted: "#", Pending: ".", Skipped: "-",
		Brand: "*", Sep: "·", Arrow: "->", Rule: "─",
		Spinner: []string{"|", "/", "-", `\`},
		BarFull: "█", BarTip: "█", BarEmpty: "░",
		TreeMid: "├─ ", TreeEnd: "└─ ", TreeBar: "│   ",
		Ellipsis: "...", Times: "x", Dash: "--",
	},
	GlyphsFull: {
		OK: "✓", Fail: "✗", Warn: "▲", Note: "●", Interrupted: "■", Pending: "·", Skipped: "–",
		Brand: "◆", Sep: "·", Arrow: "→", Rule: "─",
		Spinner: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		BarFull: "━", BarTip: "╸", BarEmpty: "─",
		TreeMid: "├─ ", TreeEnd: "└─ ", TreeBar: "│   ",
		Ellipsis: "…", Times: "×", Dash: "—",
	},
}

// Glyphs returns the marks of set s.
func (s GlyphSet) Glyphs() Glyphs {
	if int(s) < len(glyphSets) {
		return glyphSets[s]
	}
	return glyphSets[GlyphsASCII]
}
