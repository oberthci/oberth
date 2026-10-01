// Package setuptui implements the interactive TUI wizard for `oberth setup`.
// It builds an [installer.Config] from a multi-page Bubble Tea flow and hands
// it to [installer.Execute] — the same code path the flag-driven install uses.
package setuptui

import (
	"strings"

	"charm.land/bubbles/v2/progress"
	"charm.land/lipgloss/v2"
)

// Dracula canonical palette — draculatheme.com/contribute.
// Terminal cells are not subpixel-rendered text; canonical Dracula is designed
// for exactly this medium. Semantic assignments match the oberth.ci pipeline
// diagram (orange=preflight, purple=policy/brand, cyan=sync/info, green=live).
var (
	cBg      = lipgloss.Color("#282a36") // Background
	cLine    = lipgloss.Color("#44475a") // Current Line
	cFg      = lipgloss.Color("#f8f8f2") // Foreground
	cComment = lipgloss.Color("#6272a4") // Comment
	cCyan    = lipgloss.Color("#8be9fd") // Cyan
	cGreen   = lipgloss.Color("#50fa7b") // Green
	cOrange  = lipgloss.Color("#ffb86c") // Orange
	cPink    = lipgloss.Color("#ff79c6") // Pink
	cPurple  = lipgloss.Color("#bd93f9") // Purple
	cRed     = lipgloss.Color("#ff5555") // Red
	cYellow  = lipgloss.Color("#f1fa8c") // Yellow
)

// Semantic style tokens shared across all pages.
var (
	sQuestion  = lipgloss.NewStyle().Foreground(cPurple).Bold(true)
	sTopBar    = lipgloss.NewStyle().Foreground(cComment)
	sKey       = lipgloss.NewStyle().Foreground(cPink)
	sGo        = lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	sHold      = lipgloss.NewStyle().Foreground(cOrange)
	sFail      = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	sInfo      = lipgloss.NewStyle().Foreground(cCyan)
	sHighlight = lipgloss.NewStyle().Foreground(cYellow)
	sMuted     = lipgloss.NewStyle().Foreground(cComment)
	sText      = lipgloss.NewStyle().Foreground(cFg)
	sButton    = lipgloss.NewStyle().Background(cPurple).Foreground(cBg).Padding(0, 2)

	// sButtonDim is the review apply button when any HOLD exists.
	sButtonDim = lipgloss.NewStyle().Background(cLine).Foreground(cComment).Padding(0, 2)

	sListBox = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(cComment)
	sGutter = lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(cRed).
		PaddingLeft(1)

	// The token ceremony's distinctive double-line border.
	sCeremonyBox = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(cRed).
			Padding(1, 2)

	// Help modal border.
	sHelpModal = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(cPurple).
			Padding(1, 2)
)

// brandMark is the triangle logomark used in the top bar.
const brandMark = "▲" // ▲

// sectionLabel renders a page section heading. The focused section is the
// one keyboard input lands in, so it is the only one drawn in brand Purple —
// no second cursor glyph competing with the selection marker inside the
// section (the forge page used to show ❯ for both, which read as two
// unrelated cursors).
func sectionLabel(label string, focused bool) string {
	if focused {
		return lipgloss.NewStyle().Foreground(cPurple).Bold(true).Render(label)
	}
	return sMuted.Render(label)
}

// inputBox renders a text field value on the Current Line background. A
// focused field carries a visible text cursor so it is obvious where typing
// lands; an empty unfocused field shows its placeholder muted.
func inputBox(value, placeholder string, focused bool) string {
	box := lipgloss.NewStyle().Background(cLine).Foreground(cFg).Padding(0, 1)
	if value == "" && !focused && placeholder != "" {
		return box.Foreground(cComment).Render(placeholder)
	}
	if focused {
		return box.Render(value + "▏")
	}
	return box.Render(value)
}

// padTo right-pads a possibly ANSI-styled string to a visible width, so
// columns line up regardless of the escape sequences inside the cells.
func padTo(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// truncateRunes shortens plain (unstyled) text to at most n runes, marking
// the cut with an ellipsis. Used on review summaries before styling.
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// newBand creates the apt-style progress band: solid Purple fill with full
// block characters, no gradient, no percentage text, no easing (design 4.4).
func newBand() progress.Model {
	return progress.New(
		progress.WithColors(cPurple),
		progress.WithFillCharacters(progress.DefaultFullCharFullBlock, progress.DefaultEmptyCharBlock),
		progress.WithoutPercentage(),
	)
}
