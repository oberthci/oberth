package installer

import (
	"fmt"
	"io"
	"strings"
)

const (
	// leaderCol is the column where the dot leader ends and the status begins.
	leaderCol = 38

	// credBoxMinInner is the minimum inner width of the credential box.
	credBoxMinInner = 40

	// Minimum column widths for componentTable and tableWriter. Auto-sizing
	// expands columns to fit content but never shrinks below these floors.
	// At minimum the three columns total 80 display columns (including
	// border characters):
	//   1(│) + 31 + 1(│) + 31 + 1(│) + 14 + 1(│) = 80
	tableMinComponent = 31
	tableMinDetail    = 31
	tableMinStatus    = 14
)

// credentialRow is one label–value pair inside a credential box.
type credentialRow struct {
	Label string
	Value string
}

// configItem is one named check result for the config grid.
type configItem struct {
	Name   string
	Status string // "✓" or "✗"
}

// phase prints a dot-leader status line flush to the left margin:
//
//	Secret store ·························· ✓ configured
//
// Dots fill from the label to leaderCol. Minimum 3 dots. When status is
// empty the line is written without a trailing newline so the caller can
// append later.
func phase(w io.Writer, label, status string, color bool) {
	dots := leaderCol - len(label) - 1 // -1 for the space before dots
	if dots < 3 {
		dots = 3
	}
	leader := strings.Repeat("·", dots)

	if color {
		_, _ = fmt.Fprintf(w, "\033[1m%s\033[0m \033[2m%s\033[0m ", label, leader)
	} else {
		_, _ = fmt.Fprintf(w, "%s %s ", label, leader)
	}

	if status == "" {
		return
	}

	if color {
		status = colorizeStatus(status)
	}
	_, _ = fmt.Fprintf(w, "%s\n", status)
}

// credentialBox prints a titled Unicode box around credential rows:
//
//	┌── Title ───────────────────────────────────────────────┐
//	│  Root token   hvs.xxxxxxxxxxxxxxxxxxxxxxxxxxxxx        │
//	│  Unseal key   xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx │
//	└────── Footer ──────────────────────────────────────────┘
//
// Inner width auto-sizes to the longest row plus padding, clamped to a
// minimum of 40. Labels are right-aligned within the label column. When
// footer is empty the bottom border is plain. When color is true borders
// are bold yellow.
func credentialBox(w io.Writer, title, footer string, rows []credentialRow, color bool) {
	labelWidth := 0
	for _, r := range rows {
		if len(r.Label) > labelWidth {
			labelWidth = len(r.Label)
		}
	}

	maxRowWidth := 0
	for _, r := range rows {
		rw := 2 // left padding
		if labelWidth > 0 {
			rw += labelWidth + 3 // right-aligned label + 3-space gap
		}
		rw += len(r.Value) + 1 // value + right padding
		if rw > maxRowWidth {
			maxRowWidth = rw
		}
	}

	innerWidth := maxRowWidth
	if innerWidth < credBoxMinInner {
		innerWidth = credBoxMinInner
	}

	bY, bR := "", ""
	if color {
		bY = "\033[1;33m" // bold yellow
		bR = "\033[0m"
	}

	// Top border: ┌── Title ─────────────────┐
	topFill := innerWidth - 3 - len(title) - 1 // "── " + title + " "
	if topFill < 1 {
		topFill = 1
	}
	_, _ = fmt.Fprintf(w, "%s┌── %s %s┐%s\n", bY, title, strings.Repeat("─", topFill), bR)

	// Blank padding row inside the box.
	blankRow := fmt.Sprintf("%s│%s│%s\n", bY, strings.Repeat(" ", innerWidth), bR)

	// Content rows with internal vertical padding: one blank row after the
	// top border, one between each content row, and one before the bottom
	// border.
	for i, r := range rows {
		if i == 0 {
			_, _ = fmt.Fprint(w, blankRow)
		} else {
			_, _ = fmt.Fprint(w, blankRow)
		}
		var line string
		if labelWidth > 0 && r.Label != "" {
			line = fmt.Sprintf("  %*s   %s", labelWidth, r.Label, r.Value)
		} else {
			line = fmt.Sprintf("  %s", r.Value)
		}
		pad := innerWidth - len(line)
		if pad < 0 {
			pad = 0
		}
		_, _ = fmt.Fprintf(w, "%s│%s%s│%s\n", bY, line, strings.Repeat(" ", pad), bR)
	}
	if len(rows) > 0 {
		_, _ = fmt.Fprint(w, blankRow)
	}

	// Bottom border: └────── Footer ───────────┘
	if footer == "" {
		_, _ = fmt.Fprintf(w, "%s└%s┘%s\n", bY, strings.Repeat("─", innerWidth), bR)
	} else {
		bottomFill := innerWidth - 7 - len(footer) - 1 // "────── " + footer + " "
		if bottomFill < 1 {
			bottomFill = 1
		}
		_, _ = fmt.Fprintf(w, "%s└────── %s %s┘%s\n", bY, footer, strings.Repeat("─", bottomFill), bR)
	}
}

// tableRow is one row in the component status table.
type tableRow struct {
	Name      string // Component name (left column)
	Detail    string // Detail / version (middle column)
	Status    string // Status text (right column), e.g. "ready", "deployed", "warn", "failed"
	Sensitive bool   // When true, prefix name with a lock icon and highlight in yellow
}

// componentTable prints a 3-column Unicode box-drawing table:
//
//	┌──────────────────────────┬──────────────────────────┬────────────┐
//	│ Component                │ Detail                   │ Status     │
//	├──────────────────────────┼──────────────────────────┼────────────┤
//	│ Kubernetes auth          │ secret store             │ ready      │
//	│ Oberth                   │ v0.12.43                 │ deployed   │
//	└──────────────────────────┴──────────────────────────┴────────────┘
//
// Column widths auto-size to fit content, with minimum floors so the
// table never shrinks below 80 display columns. Borders are single-weight
// box-drawing characters. When color is true, borders are dim, the header
// is bold, sensitive rows are highlighted in yellow, and status symbols
// are colorized.
func componentTable(w io.Writer, rows []tableRow, color bool) {
	// Start from minimum widths (header labels + padding already fit).
	cW := tableMinComponent
	dW := tableMinDetail
	sW := tableMinStatus

	// Expand to fit content.
	for _, row := range rows {
		if w := len(row.Name) + 2; w > cW {
			cW = w
		}
		if w := len(row.Detail) + 2; w > dW {
			dW = w
		}
		if w := plainStatusLen(row.Status) + 2; w > sW {
			sW = w
		}
	}

	dim, bold, yellow, reset := "", "", "", ""
	if color {
		dim = "\033[2m"
		bold = "\033[1m"
		yellow = "\033[33m"
		reset = "\033[0m"
	}

	// Border helpers.
	hC := strings.Repeat("─", cW) // ─ repeated
	hD := strings.Repeat("─", dW)
	hS := strings.Repeat("─", sW)

	// Top border: ┌────┬────┬────┐
	_, _ = fmt.Fprintf(w, "%s┌%s┬%s┬%s┐%s\n", dim, hC, hD, hS, reset)

	// Header row — %-*s width is cW-1 (1 leading space + cW-1 content = cW).
	_, _ = fmt.Fprintf(w, "%s│%s%s %-*s%s%s│%s%s %-*s%s%s│%s%s %-*s%s%s│%s\n",
		dim, reset, bold, cW-1, "Component", reset, dim, reset, bold, dW-1, "Detail", reset, dim, reset, bold, sW-1, "Status", reset, dim, reset)

	// Header separator: ├────┼────┼────┤
	_, _ = fmt.Fprintf(w, "%s├%s┼%s┼%s┤%s\n", dim, hC, hD, hS, reset)

	// Data rows.
	for _, row := range rows {
		name := row.Name
		nameDisplay := name

		namePad := cW - 2 - len(name)
		if namePad < 0 {
			namePad = 0
		}

		detailPad := dW - 2 - len(row.Detail)
		if detailPad < 0 {
			detailPad = 0
		}

		statusText := formatStatusText(row.Status, color)
		statusPlainLen := plainStatusLen(row.Status)
		statusPad := sW - 2 - statusPlainLen
		if statusPad < 0 {
			statusPad = 0
		}

		if color && row.Sensitive {
			_, _ = fmt.Fprintf(w, "%s│%s %s%s%s%s %s│%s %s%s%s%s %s│%s %s%s %s│%s\n",
				dim, reset,
				yellow, nameDisplay, reset, strings.Repeat(" ", namePad),
				dim, reset,
				yellow, row.Detail, reset, strings.Repeat(" ", detailPad),
				dim, reset,
				statusText, strings.Repeat(" ", statusPad),
				dim, reset)
		} else {
			_, _ = fmt.Fprintf(w, "%s│%s %s%s %s│%s %s%s %s│%s %s%s %s│%s\n",
				dim, reset,
				nameDisplay, strings.Repeat(" ", namePad),
				dim, reset,
				row.Detail, strings.Repeat(" ", detailPad),
				dim, reset,
				statusText, strings.Repeat(" ", statusPad),
				dim, reset)
		}
	}

	// Bottom border: └────┴────┴────┘
	_, _ = fmt.Fprintf(w, "%s└%s┴%s┴%s┘%s\n", dim, hC, hD, hS, reset)
}

// formatStatusText renders a status string with its leading symbol colorized.
func formatStatusText(status string, color bool) string {
	if !color {
		return status
	}
	if strings.HasPrefix(status, "✓") { // ✓
		return "\033[32m✓\033[0m" + status[len("✓"):]
	}
	if strings.HasPrefix(status, "⚠") { // ⚠
		return "\033[33m⚠\033[0m" + status[len("⚠"):]
	}
	if strings.HasPrefix(status, "✗") { // ✗
		return "\033[31m✗\033[0m" + status[len("✗"):]
	}
	return status
}

// plainStatusLen returns the display-column count of a status string,
// accounting for multi-byte Unicode symbols that are each 1 display column.
func plainStatusLen(status string) int {
	n := 0
	for _, r := range status {
		n++
		_ = r
	}
	return n
}

func colorizeStatus(s string) string {
	s = strings.ReplaceAll(s, "✓", "\033[32m✓\033[0m")
	s = strings.ReplaceAll(s, "✗", "\033[31m✗\033[0m")
	return s
}

// isColor reports whether color output is enabled.
func isColor(deps Deps) bool {
	return deps.IsTerminal != nil && deps.IsTerminal()
}

// ---------------------------------------------------------------------------
// Borderless prompt helpers — used by the onboarding section. Prompts appear
// below the table's bottom border; after the user answers, the prompt lines
// are erased and the result is appended to the table via AppendRow.
// ---------------------------------------------------------------------------

// erasePromptLines erases n lines above the cursor. Used to clean up prompt
// lines before appending the result to the table. No-op when color is off.
func erasePromptLines(w io.Writer, color bool, n int) {
	if !color {
		return
	}
	for range n {
		_, _ = fmt.Fprint(w, "\033[1A\r\033[2K")
	}
}

// startPrompt prints "  label  prompt" without a newline — the caller reads
// input on the same line. Bold label when color is on.
func startPrompt(w io.Writer, color bool, label, prompt string) {
	bold, reset := "", ""
	if color {
		bold = "\033[1m"
		reset = "\033[0m"
	}
	_, _ = fmt.Fprintf(w, "  %s%s%s  %s", bold, label, reset, prompt)
}

// completePrompt overwrites the prompt line with the result using the same
// cursor-up-and-clear technique as tableWriter.CompleteRow. Call this after
// startPrompt + user input (which moved the cursor to the next line).
func completePrompt(w io.Writer, color bool, label, detail, status string) {
	if color {
		_, _ = fmt.Fprint(w, "\033[1A\r\033[2K")
	}
	writeResult(w, color, label, detail, status)
}

// writeResult prints one borderless result line. The status symbol (when
// present) is extracted and placed before the label:
//
//	✓ Upstream        ssh://git@github.com/org   connected
func writeResult(w io.Writer, color bool, label, detail, status string) {
	symbol, statusWord := splitStatusSymbol(status)
	if color && symbol != "" {
		symbol = colorizeSymbol(symbol)
	}
	var prefix string
	if symbol != "" {
		prefix = "  " + symbol + " "
	} else {
		prefix = "    "
	}
	if detail != "" {
		_, _ = fmt.Fprintf(w, "%s%-16s  %-34s  %s\n", prefix, label, detail, statusWord)
	} else {
		_, _ = fmt.Fprintf(w, "%s%-16s  %s\n", prefix, label, statusWord)
	}
}

// splitStatusSymbol splits a status string like "✓ connected" into its
// symbol ("✓") and text ("connected"). Returns empty symbol for status
// strings without a recognized prefix.
func splitStatusSymbol(status string) (string, string) {
	for _, s := range []string{"✓", "⚠", "✗", "…"} {
		if strings.HasPrefix(status, s+" ") {
			return s, status[len(s)+1:]
		}
	}
	return "", status
}

// colorizeSymbol returns the ANSI-colorized version of a single status symbol.
func colorizeSymbol(symbol string) string {
	switch symbol {
	case "✓":
		return "\033[32m✓\033[0m"
	case "⚠":
		return "\033[33m⚠\033[0m"
	case "✗":
		return "\033[31m✗\033[0m"
	default:
		return symbol
	}
}

// ---------------------------------------------------------------------------
// tableWriter — streaming version of componentTable
// ---------------------------------------------------------------------------

// tableWriter writes a streaming 3-column component table. The header is
// printed once, rows appear one at a time as each step completes, and the
// footer closes the table. Interactive prompts can be embedded inline using
// StartPromptRow / CompleteRow.
//
// Column widths default to the minimum floors and can be expanded before
// WriteHeader via RegisterContent. Once the header is written, widths are
// locked — content wider than the registered width will overflow (same as
// WriteSpanRow's intentional ragged-right behavior).
type tableWriter struct {
	w      io.Writer
	color  bool
	closed bool
	dim    string
	bold   string
	yellow string
	reset  string
	cW     int // component column width
	dW     int // detail column width
	sW     int // status column width
}

// newTableWriter creates a streaming table writer. Column widths start at
// the minimum floors. Call RegisterContent to expand columns for known
// content before WriteHeader, then WriteRow / WriteSensitiveRow for each
// completed step, and WriteFooter when all rows are done.
func newTableWriter(w io.Writer, color bool) *tableWriter {
	tw := &tableWriter{
		w:     w,
		color: color,
		cW:    tableMinComponent,
		dW:    tableMinDetail,
		sW:    tableMinStatus,
	}
	if color {
		tw.dim = "\033[2m"
		tw.bold = "\033[1m"
		tw.yellow = "\033[33m"
		tw.reset = "\033[0m"
	}
	return tw
}

// RegisterContent expands column widths to fit the given content. Call this
// for every row whose content is known before WriteHeader so the header
// and all subsequent rows share consistent column widths.
func (tw *tableWriter) RegisterContent(component, detail, status string) {
	if w := len(component) + 2; w > tw.cW {
		tw.cW = w
	}
	if w := len(detail) + 2; w > tw.dW {
		tw.dW = w
	}
	if w := plainStatusLen(status) + 2; w > tw.sW {
		tw.sW = w
	}
}

// WriteHeader prints the top border, header row, and separator line. Header
// cells pad to the full column width (one leading space + width-1 content)
// so the │ separators align with the ┬ joints of the borders.
func (tw *tableWriter) WriteHeader() {
	hC := strings.Repeat("─", tw.cW)
	hD := strings.Repeat("─", tw.dW)
	hS := strings.Repeat("─", tw.sW)

	_, _ = fmt.Fprintf(tw.w, "%s┌%s┬%s┬%s┐%s\n", tw.dim, hC, hD, hS, tw.reset)
	_, _ = fmt.Fprintf(tw.w, "%s│%s%s %-*s%s%s│%s%s %-*s%s%s│%s%s %-*s%s%s│%s\n",
		tw.dim, tw.reset, tw.bold, tw.cW-1, "Component", tw.reset, tw.dim,
		tw.reset, tw.bold, tw.dW-1, "Detail", tw.reset, tw.dim,
		tw.reset, tw.bold, tw.sW-1, "Status", tw.reset, tw.dim, tw.reset)
	_, _ = fmt.Fprintf(tw.w, "%s├%s┼%s┼%s┤%s\n", tw.dim, hC, hD, hS, tw.reset)
}

// WriteRow prints one completed data row.
func (tw *tableWriter) WriteRow(component, detail, status string) {
	tw.writeDataRow(component, detail, status, false)
}

// WriteSensitiveRow prints a data row highlighted in yellow (when color is on).
func (tw *tableWriter) WriteSensitiveRow(component, detail, status string) {
	tw.writeDataRow(component, detail, status, true)
}

// WriteSpanRow prints a single-cell row that spans the full table width.
// Useful for displaying content like SSH public keys that do not fit in one
// column. The left and right borders use the same │ character so the row is
// visually part of the table. Content longer than the inner width is printed
// IN FULL — the right border goes ragged rather than truncating: for a
// deploy key, one silently dropped character produces a key the forge
// rejects (or worse, a key the operator hunts for a corrupted paste of).
func (tw *tableWriter) WriteSpanRow(content string) {
	// Inner width = cW + 1(│) + dW + 1(│) + sW = total between outer │s.
	innerWidth := tw.cW + 1 + tw.dW + 1 + tw.sW
	pad := innerWidth - 2 - len(content) // leading and trailing space
	if pad < 0 {
		pad = 0
	}
	_, _ = fmt.Fprintf(tw.w, "%s│%s %s%s %s│%s\n",
		tw.dim, tw.reset,
		content, strings.Repeat(" ", pad),
		tw.dim, tw.reset)
}

// StartPromptRow prints a partial row (│ component │ promptText) without a
// newline. The caller reads user input, then calls CompleteRow to overwrite
// the partial line with the final completed row.
func (tw *tableWriter) StartPromptRow(component, promptText string) {
	namePad := tw.cW - 2 - len(component)
	if namePad < 0 {
		namePad = 0
	}
	_, _ = fmt.Fprintf(tw.w, "%s│%s %s%s %s│%s %s",
		tw.dim, tw.reset,
		component, strings.Repeat(" ", namePad),
		tw.dim, tw.reset,
		promptText)
}

// CompleteRow replaces the current prompt line with a completed data row.
// When color is enabled, ANSI escapes move the cursor up one line and clear
// it before printing the final row. When color is off (tests, pipes), the
// completed row is printed on the next line.
func (tw *tableWriter) CompleteRow(component, detail, status string, sensitive bool) {
	if tw.color {
		// The user pressed Enter after the prompt, moving the cursor down.
		// Go back up, clear the prompt line, and overwrite it.
		_, _ = fmt.Fprint(tw.w, "\033[1A\r\033[2K")
	}
	tw.writeDataRow(component, detail, status, sensitive)
}

// WriteFooter prints the bottom border. Safe to call multiple times; only the
// first call writes output.
func (tw *tableWriter) WriteFooter() {
	if tw.closed {
		return
	}
	tw.closed = true
	tw.writeBottomBorder()
}

// writeBottomBorder unconditionally writes the └───┴───┴───┘ line.
func (tw *tableWriter) writeBottomBorder() {
	hC := strings.Repeat("─", tw.cW)
	hD := strings.Repeat("─", tw.dW)
	hS := strings.Repeat("─", tw.sW)
	_, _ = fmt.Fprintf(tw.w, "%s└%s┴%s┴%s┘%s\n", tw.dim, hC, hD, hS, tw.reset)
}

// eraseLineAbove moves the cursor up one line and clears it. Used by AppendRow
// and AppendSpanRow to erase the bottom border before appending a new row and
// redrawing the border. No-op when color is off (pipes/CI).
func (tw *tableWriter) eraseLineAbove() {
	if tw.color {
		_, _ = fmt.Fprint(tw.w, "\033[1A\r\033[2K")
	}
}

// AppendRow adds a row to a table that already has its bottom border drawn.
// When color is on, the bottom border is erased, the new data row is written,
// and the border is redrawn — the table appears to grow in place. When color
// is off (pipes/CI), the row is simply printed with no cursor tricks; a final
// WriteFooter at the end is needed.
func (tw *tableWriter) AppendRow(component, detail, status string, sensitive bool) {
	if tw.color {
		tw.eraseLineAbove()
		tw.closed = false
		tw.writeDataRow(component, detail, status, sensitive)
		tw.closed = false
		tw.writeBottomBorder()
		tw.closed = true
	} else {
		tw.writeDataRow(component, detail, status, sensitive)
	}
}

// AppendSpanRow adds a full-width span row to a table that already has its
// bottom border drawn. Same erase+redraw pattern as AppendRow.
func (tw *tableWriter) AppendSpanRow(content string) {
	if tw.color {
		tw.eraseLineAbove()
		tw.closed = false
		tw.WriteSpanRow(content)
		tw.closed = false
		tw.writeBottomBorder()
		tw.closed = true
	} else {
		tw.WriteSpanRow(content)
	}
}

// writeDataRow is the shared implementation for WriteRow and WriteSensitiveRow.
func (tw *tableWriter) writeDataRow(component, detail, status string, sensitive bool) {
	namePad := tw.cW - 2 - len(component)
	if namePad < 0 {
		namePad = 0
	}
	detailPad := tw.dW - 2 - len(detail)
	if detailPad < 0 {
		detailPad = 0
	}
	statusText := formatStatusText(status, tw.color)
	statusPlainLen := plainStatusLen(status)
	statusPad := tw.sW - 2 - statusPlainLen
	if statusPad < 0 {
		statusPad = 0
	}

	if tw.color && sensitive {
		_, _ = fmt.Fprintf(tw.w, "%s│%s %s%s%s%s %s│%s %s%s%s%s %s│%s %s%s %s│%s\n",
			tw.dim, tw.reset,
			tw.yellow, component, tw.reset, strings.Repeat(" ", namePad),
			tw.dim, tw.reset,
			tw.yellow, detail, tw.reset, strings.Repeat(" ", detailPad),
			tw.dim, tw.reset,
			statusText, strings.Repeat(" ", statusPad),
			tw.dim, tw.reset)
	} else {
		_, _ = fmt.Fprintf(tw.w, "%s│%s %s%s %s│%s %s%s %s│%s %s%s %s│%s\n",
			tw.dim, tw.reset,
			component, strings.Repeat(" ", namePad),
			tw.dim, tw.reset,
			detail, strings.Repeat(" ", detailPad),
			tw.dim, tw.reset,
			statusText, strings.Repeat(" ", statusPad),
			tw.dim, tw.reset)
	}
}

// ---------------------------------------------------------------------------
// heldCredentials — deferred credential display
// ---------------------------------------------------------------------------

// heldCredentials collects credential rows (OpenBao root token, unseal key,
// bearer token) and prints them in a single box after the install table
// closes. flush is idempotent — safe to call from a defer and again at the
// end of a normal flow.
type heldCredentials struct {
	rows []credentialRow
	// notes are plain lines printed after the box: where a credential was
	// saved and how to read it back. A credential the installer put into the
	// host's secret store produces a note and no row, because the value the
	// operator no longer has to copy is a value that should not be on screen.
	notes   []string
	printed bool
}

// add appends one label/value pair. Must be called before flush.
func (h *heldCredentials) add(label, value string) {
	h.rows = append(h.rows, credentialRow{Label: label, Value: value})
}

// addNote appends one line printed after the box. Must be called before flush.
func (h *heldCredentials) addNote(line string) {
	h.notes = append(h.notes, line)
}

// flush prints the credential box exactly once, followed by any notes.
// Subsequent calls are no-ops. Does nothing when nothing has been added.
func (h *heldCredentials) flush(w io.Writer, color bool) {
	if h.printed || (len(h.rows) == 0 && len(h.notes) == 0) {
		return
	}
	h.printed = true
	_, _ = fmt.Fprintln(w)
	if len(h.rows) > 0 {
		credentialBox(w,
			"Credentials (save now, shown once)",
			"",
			h.rows, color)
	}
	for _, note := range h.notes {
		_, _ = fmt.Fprintln(w, note)
	}
}
