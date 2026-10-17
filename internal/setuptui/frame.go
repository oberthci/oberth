package setuptui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Every screen of the wizard is drawn by renderFrame, so they all look and
// behave the same (#047): a title bar "Oberth setup — <step> (N of M)", a
// heading, a short explanation, one input, an error/status line, a fixed
// button row and a key line. Content taller than the terminal scrolls; long
// commands wrap instead of running off the edge (#014).

// frameSpec is what a screen hands to renderFrame.
type frameSpec struct {
	title     string
	heading   string
	explain   string
	body      []string // the input or content, one entry per line (may be styled)
	focusLine int      // body line to keep visible; -1 for none
	errMsg    string
	status    string
	buttons   []string
	button    int // focused button; -1 when focus is on the input
	disabled  map[int]bool
	keys      string
}

// wrapText wraps plain text to width, breaking long words (paths, commands)
// instead of letting them run past the edge.
func wrapText(s string, width int) []string {
	if width < 10 {
		width = 10
	}
	if s == "" {
		return nil
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if para == "" {
			out = append(out, "")
			continue
		}
		out = append(out, strings.Split(ansi.Wrap(para, width, " "), "\n")...)
	}
	return out
}

// wrapWords wraps plain text at spaces only. ansi.Wrap also breaks after a
// hyphen, which splits a command's flags ("--" | "yes") and a shell default
// (":-" | "$HOME") across lines; a word wider than the line is cut as a last
// resort.
func wrapWords(s string, width int) []string {
	width = max(10, width)
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		for ansi.StringWidth(word) > width {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			r := []rune(word)
			lines = append(lines, string(r[:width]))
			word = string(r[width:])
		}
		switch {
		case line == "":
			line = word
		case ansi.StringWidth(line)+1+ansi.StringWidth(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// wrapCommand wraps a command so every piece stays copyable: continuation
// lines are indented and nothing is truncated.
func wrapCommand(cmd string, width int, style lipgloss.Style) []string {
	lines := wrapText(cmd, max(10, width-2))
	for i := range lines {
		prefix := ""
		if i > 0 {
			prefix = "  "
		}
		lines[i] = style.Render(prefix + lines[i])
	}
	return lines
}

func indentLines(lines []string, n int) []string {
	pad := strings.Repeat(" ", n)
	out := make([]string, len(lines))
	for i, l := range lines {
		if l == "" {
			out[i] = ""
			continue
		}
		out[i] = pad + l
	}
	return out
}

// renderButtons draws "<Go Back>   <Continue>" with the focused one inverted.
func renderButtons(labels []string, focused int, disabled map[int]bool) string {
	parts := make([]string, len(labels))
	for i, label := range labels {
		text := "<" + label + ">"
		switch {
		case disabled[i]:
			parts[i] = sMuted.Render(text)
		case i == focused:
			parts[i] = sButton.Render(text)
		default:
			parts[i] = sText.Render(text)
		}
	}
	return strings.Join(parts, "   ")
}

// renderFrame lays a screen out at width×height. scroll is the screen's
// scroll offset; it is adjusted so focusLine stays visible and clamped to
// the content.
func renderFrame(width, height int, spec frameSpec, scroll *int, footer string) string {
	inner := max(20, width-4)

	var content []string
	content = append(content, indentLines(wrapText(spec.heading, inner), 2)...)
	for i := range content {
		content[i] = sQuestion.Render(content[i])
	}
	if spec.explain != "" {
		content = append(content, "")
		content = append(content, indentLines(wrapText(spec.explain, inner), 2)...)
	}
	content = append(content, "")
	bodyStart := len(content)
	content = append(content, indentLines(spec.body, 2)...)

	// Status and errors stay pinned above the buttons, so a long screen
	// never scrolls the reason for a refusal out of sight.
	var notes []string
	for _, l := range wrapText(spec.status, inner) {
		if l != "" {
			notes = append(notes, "  "+sInfo.Render(l))
		}
	}
	for _, l := range wrapText(spec.errMsg, inner) {
		if l != "" {
			notes = append(notes, "  "+sFail.Render(l))
		}
	}
	if len(notes) > 4 {
		notes = append(notes[:3], "  "+sFail.Render("…"))
	}

	// title(1) + blank(1) + content + notes + blank(1) + buttons(1) + keys(1)
	avail := max(3, height-5-len(notes))
	maxScroll := max(0, len(content)-avail)
	if spec.focusLine >= 0 {
		line := bodyStart + spec.focusLine
		if line < *scroll {
			*scroll = line
		}
		if line >= *scroll+avail {
			*scroll = line - avail + 1
		}
	}
	*scroll = min(max(0, *scroll), maxScroll)
	visible := content[*scroll:min(len(content), *scroll+avail)]

	var b strings.Builder
	title := " Oberth setup — " + spec.title
	b.WriteString(sTitleBar.Width(width).Render(ansi.Truncate(title, width, "…")) + "\n\n")
	for _, l := range visible {
		b.WriteString(ansi.Truncate(l, width, "") + "\n")
	}
	for i := len(visible); i < avail; i++ {
		b.WriteByte('\n')
	}
	for _, l := range notes {
		b.WriteString(ansi.Truncate(l, width, "") + "\n")
	}

	more := ""
	switch {
	case *scroll > 0 && *scroll < maxScroll:
		more = "↑↓ more · "
	case *scroll > 0:
		more = "↑ more · "
	case *scroll < maxScroll:
		more = "↓ more · "
	}
	buttons := renderButtons(spec.buttons, spec.button, spec.disabled)
	b.WriteString("\n" + lipgloss.PlaceHorizontal(width, lipgloss.Center, buttons) + "\n")
	keys := footer
	if keys == "" {
		keys = more + spec.keys
	}
	b.WriteString(sMuted.Render(ansi.Truncate(" "+keys, width, "…")))
	return b.String()
}

// ---------- focus: the input, then the buttons ----------

// focusRing moves focus across a screen's inputs (targets) and its buttons
// with the same keys on every screen: tab / shift+tab cycle, ←/→ move along
// the button row, ↑ from the buttons returns to the input.
type focusRing struct {
	targets int
	buttons int
	at      int
}

// button returns the focused button index, or -1 when an input has focus.
func (f *focusRing) button() int {
	if f.at < f.targets {
		return -1
	}
	return f.at - f.targets
}

func (f *focusRing) focusButton(i int) { f.at = f.targets + i }

func (f *focusRing) total() int { return f.targets + f.buttons }

// handle applies the shared navigation keys and reports whether it used key.
func (f *focusRing) handle(key string) bool {
	if f.total() == 0 {
		return false
	}
	switch key {
	case "tab":
		f.at = (f.at + 1) % f.total()
		return true
	case "shift+tab":
		f.at = (f.at + f.total() - 1) % f.total()
		return true
	}
	if f.button() < 0 {
		return false
	}
	switch key {
	case "left":
		if f.button() > 0 {
			f.at--
		}
		return true
	case "right":
		if f.button() < f.buttons-1 {
			f.at++
		}
		return true
	case "up":
		if f.targets > 0 {
			f.at = f.targets - 1
			return true
		}
	}
	return false
}

// ---------- a one-line text input with a real cursor (#006) ----------

type lineInput struct {
	runes []rune
	pos   int
}

func newLineInput(value string) *lineInput {
	r := []rune(value)
	return &lineInput{runes: r, pos: len(r)}
}

func (in *lineInput) value() string { return string(in.runes) }

func (in *lineInput) insert(s string) {
	s = strings.NewReplacer("\r", "", "\n", "", "\t", " ").Replace(s)
	r := []rune(s)
	in.runes = append(in.runes[:in.pos], append(r, in.runes[in.pos:]...)...)
	in.pos += len(r)
}

// handleKey edits the text and reports whether the key was an editing key
// and whether the text changed.
func (in *lineInput) handleKey(msg tea.KeyPressMsg) (handled, changed bool) {
	switch msg.String() {
	case "left", "ctrl+b":
		in.pos = max(0, in.pos-1)
		return true, false
	case "right", "ctrl+f":
		in.pos = min(len(in.runes), in.pos+1)
		return true, false
	case "home", "ctrl+a":
		in.pos = 0
		return true, false
	case "end", "ctrl+e":
		in.pos = len(in.runes)
		return true, false
	case "alt+left", "ctrl+left":
		for in.pos > 0 && in.runes[in.pos-1] == ' ' {
			in.pos--
		}
		for in.pos > 0 && in.runes[in.pos-1] != ' ' {
			in.pos--
		}
		return true, false
	case "alt+right", "ctrl+right":
		for in.pos < len(in.runes) && in.runes[in.pos] == ' ' {
			in.pos++
		}
		for in.pos < len(in.runes) && in.runes[in.pos] != ' ' {
			in.pos++
		}
		return true, false
	case "backspace", "ctrl+h":
		if in.pos > 0 {
			in.runes = append(in.runes[:in.pos-1], in.runes[in.pos:]...)
			in.pos--
			return true, true
		}
		return true, false
	case "delete", "ctrl+d":
		if in.pos < len(in.runes) {
			in.runes = append(in.runes[:in.pos], in.runes[in.pos+1:]...)
			return true, true
		}
		return true, false
	case "ctrl+u":
		changed := in.pos > 0
		in.runes = in.runes[in.pos:]
		in.pos = 0
		return true, changed
	case "ctrl+k":
		changed := in.pos < len(in.runes)
		in.runes = in.runes[:in.pos]
		return true, changed
	case "ctrl+w":
		start := in.pos
		for start > 0 && in.runes[start-1] == ' ' {
			start--
		}
		for start > 0 && in.runes[start-1] != ' ' {
			start--
		}
		changed := start < in.pos
		in.runes = append(in.runes[:start], in.runes[in.pos:]...)
		in.pos = start
		return true, changed
	case "space":
		in.insert(" ")
		return true, true
	}
	if msg.Text != "" && msg.Mod&(tea.ModCtrl|tea.ModAlt) == 0 {
		in.insert(msg.Text)
		return true, true
	}
	return false, false
}

// view renders the field width cells wide, scrolled so the cursor shows.
func (in *lineInput) view(width int, focused bool, placeholder string) string {
	width = max(8, width)
	box := lipgloss.NewStyle().Background(cLine).Foreground(cFg)
	// A placeholder is an example, never a value (#057): it reads "e.g. …",
	// stays dim, and shows behind the cursor while the field is focused too,
	// so moving focus away does not make it look as if it had been typed.
	if len(in.runes) == 0 && placeholder != "" {
		hint := truncateRunes("e.g. "+placeholder, max(0, width-1))
		if focused {
			return sCursor.Render(" ") + box.Foreground(cComment).Render(padTo(hint, width-1))
		}
		return box.Foreground(cComment).Render(padTo(hint, width))
	}
	start := 0
	if in.pos >= width {
		start = in.pos - width + 1
	}
	end := min(len(in.runes), start+width)
	var b strings.Builder
	for i := start; i < end; i++ {
		ch := string(in.runes[i])
		if focused && i == in.pos {
			b.WriteString(sCursor.Render(ch))
		} else {
			b.WriteString(box.Render(ch))
		}
	}
	shown := end - start
	if focused && in.pos == end && shown < width {
		b.WriteString(sCursor.Render(" "))
		shown++
	}
	if shown < width {
		b.WriteString(box.Render(strings.Repeat(" ", width-shown)))
	}
	return b.String()
}
