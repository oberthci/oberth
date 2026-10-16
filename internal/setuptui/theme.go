// Package setuptui implements `oberth setup`: a Debian-installer-style
// wizard that builds an [installer.Config] one validated question at a time
// and hands it to [installer.Execute] — the same code path the flag-driven
// install uses. The same questions drive the TUI and the plain prompts.
package setuptui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// Dracula canonical palette — draculatheme.com/contribute.
var (
	cBg      = lipgloss.Color("#282a36") // Background
	cLine    = lipgloss.Color("#44475a") // Current Line
	cFg      = lipgloss.Color("#f8f8f2") // Foreground
	cComment = lipgloss.Color("#6272a4") // Comment
	cCyan    = lipgloss.Color("#8be9fd") // Cyan
	cGreen   = lipgloss.Color("#50fa7b") // Green
	cOrange  = lipgloss.Color("#ffb86c") // Orange
	cPurple  = lipgloss.Color("#bd93f9") // Purple
	cRed     = lipgloss.Color("#ff5555") // Red
	cYellow  = lipgloss.Color("#f1fa8c") // Yellow
)

// Semantic style tokens shared by every screen.
var (
	sQuestion  = lipgloss.NewStyle().Foreground(cPurple).Bold(true)
	sTitleBar  = lipgloss.NewStyle().Background(cPurple).Foreground(cBg).Bold(true)
	sGo        = lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	sHold      = lipgloss.NewStyle().Foreground(cOrange)
	sFail      = lipgloss.NewStyle().Foreground(cRed).Bold(true)
	sInfo      = lipgloss.NewStyle().Foreground(cCyan)
	sHighlight = lipgloss.NewStyle().Foreground(cYellow)
	sMuted     = lipgloss.NewStyle().Foreground(cComment)
	sText      = lipgloss.NewStyle().Foreground(cFg)
	sSelected  = lipgloss.NewStyle().Background(cPurple).Foreground(cBg)
	sButton    = lipgloss.NewStyle().Background(cPurple).Foreground(cBg).Bold(true)
	sCursor    = lipgloss.NewStyle().Background(cFg).Foreground(cBg)
	sReveal    = lipgloss.NewStyle().Background(cLine).Foreground(cFg)
)

// padTo right-pads a possibly ANSI-styled string to a visible width, so
// columns line up regardless of the escape sequences inside the cells.
func padTo(s string, width int) string {
	if w := lipgloss.Width(s); w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return s
}

// truncateRunes shortens plain (unstyled) text to at most n runes, marking
// the cut with an ellipsis.
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
