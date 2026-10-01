package installer

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPhaseWithStatus(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	phase(&buf, "Secret store", "✓ configured", false)
	got := buf.String()
	if !strings.HasPrefix(got, "Secret store ") {
		t.Fatalf("expected label at start, got %q", got)
	}
	if !strings.Contains(got, "·") {
		t.Fatalf("expected middle-dot leaders, got %q", got)
	}
	if !strings.HasSuffix(got, "✓ configured\n") {
		t.Fatalf("expected status with newline at end, got %q", got)
	}
	if len(got) > 80 {
		t.Fatalf("line exceeds 80 columns: %d chars: %q", len(got), got)
	}
}

func TestPhaseWithoutStatus(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	phase(&buf, "Secret store", "", false)
	got := buf.String()
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("empty status must not produce a trailing newline, got %q", got)
	}
}

func TestPhaseMinimumDots(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	phase(&buf, strings.Repeat("X", leaderCol), "done", false)
	got := buf.String()
	if !strings.Contains(got, "···") {
		t.Fatalf("very long label must still get minimum 3 dots, got %q", got)
	}
}

func TestPhaseColorCodes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	phase(&buf, "Test", "✓ ok", true)
	got := buf.String()
	if !strings.Contains(got, "\033[1m") {
		t.Fatalf("color=true must produce bold label, got %q", got)
	}
	if !strings.Contains(got, "\033[2m") {
		t.Fatalf("color=true must produce dim dots, got %q", got)
	}
	if !strings.Contains(got, "\033[32m✓\033[0m") {
		t.Fatalf("color=true must produce green checkmark, got %q", got)
	}
}

func TestPhaseNoColorCodes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	phase(&buf, "Test", "✓ ok", false)
	got := buf.String()
	if strings.Contains(got, "\033[") {
		t.Fatalf("color=false must not contain ANSI codes, got %q", got)
	}
}

func TestCredentialBoxBasic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "OpenBao credentials (save now, shown once)",
		"Save. The unseal key is needed after pod restart",
		[]credentialRow{
			{Label: "Root token", Value: "hvs.xxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
			{Label: "Unseal key", Value: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		}, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// top + blank + row1 + blank + row2 + blank + bottom = 7 lines
	// (internal vertical padding: blank after top, between rows, before bottom)
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines (padded layout), got %d: %q", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "┌── ") || !strings.HasSuffix(lines[0], "┐") {
		t.Fatalf("top border malformed: %q", lines[0])
	}
	if !strings.Contains(lines[0], "OpenBao credentials") {
		t.Fatalf("top border missing title: %q", lines[0])
	}
	bottom := lines[len(lines)-1]
	if !strings.HasPrefix(bottom, "└────── ") || !strings.HasSuffix(bottom, "┘") {
		t.Fatalf("bottom border malformed: %q", bottom)
	}
	if !strings.Contains(bottom, "Save. The unseal key") {
		t.Fatalf("bottom border missing footer: %q", bottom)
	}
	for _, line := range lines {
		// ✓ and ─ are multi-byte; check display width via simple ASCII estimate
		// Box-drawing chars are 1 column wide, so byte count overestimates.
		// Real constraint: must not exceed ~80 display columns for realistic tokens.
		if len(line) > 200 { // generous byte limit (multi-byte chars inflate)
			t.Fatalf("line unexpectedly long: %d bytes", len(line))
		}
	}
}

func TestCredentialBoxNoLabel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "Bearer token (save now, shown once)",
		"This token is never shown again",
		[]credentialRow{{Value: "oberth_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}},
		false)
	got := buf.String()
	if !strings.Contains(got, "oberth_") {
		t.Fatalf("token value missing: %q", got)
	}
	if !strings.Contains(got, "Bearer token") {
		t.Fatalf("title missing: %q", got)
	}
	if !strings.Contains(got, "never shown again") {
		t.Fatalf("footer missing: %q", got)
	}
}

func TestCredentialBoxNoFooter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "Title", "", []credentialRow{{Value: "val"}}, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	bottom := lines[len(lines)-1]
	if !strings.HasPrefix(bottom, "└") || !strings.HasSuffix(bottom, "┘") {
		t.Fatalf("plain bottom border malformed: %q", bottom)
	}
	// No text in bottom border
	bottomInner := bottom[len("└") : len(bottom)-len("┘")]
	for _, r := range bottomInner {
		if r != '─' {
			t.Fatalf("plain bottom border must be all dashes, got %q", bottom)
		}
	}
}

func TestCredentialBoxColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "Title", "Footer",
		[]credentialRow{{Value: "val"}}, true)
	got := buf.String()
	if !strings.Contains(got, "\033[1;33m") {
		t.Fatalf("color=true must produce bold yellow borders, got %q", got)
	}
}

func TestCredentialBoxNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "Title", "Footer",
		[]credentialRow{{Value: "val"}}, false)
	got := buf.String()
	if strings.Contains(got, "\033[") {
		t.Fatalf("color=false must not contain ANSI codes, got %q", got)
	}
}

func TestCredentialBoxLabelAlignment(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "Test", "",
		[]credentialRow{
			{Label: "Short", Value: "aaa"},
			{Label: "Longer label", Value: "bbb"},
		}, false)
	got := buf.String()
	// Find the content lines containing the values (skip blank padding rows)
	var aLine, bLine string
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.Contains(line, "aaa") {
			aLine = line
		}
		if strings.Contains(line, "bbb") {
			bLine = line
		}
	}
	if aLine == "" || bLine == "" {
		t.Fatalf("value lines not found in:\n%s", got)
	}
	aPos := strings.Index(aLine, "aaa")
	bPos := strings.Index(bLine, "bbb")
	if aPos != bPos {
		t.Fatalf("values must be aligned: aaa at %d, bbb at %d in:\n%s", aPos, bPos, got)
	}
}

func TestComponentTableStructure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Kubernetes auth", Detail: "secret store", Status: "✓ ready", Sensitive: true},
		{Name: "KV v2 mount", Detail: "secret store", Status: "✓ ready", Sensitive: true},
		{Name: "Execution engine", Detail: "argo-workflows v1.0.24", Status: "✓ deployed"},
		{Name: "Oberth", Detail: "v0.12.43", Status: "✓ deployed"},
	}
	componentTable(&buf, rows, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// top + header + separator + 4 data rows + bottom = 8 lines
	if len(lines) != 8 {
		t.Fatalf("expected 8 lines, got %d:\n%s", len(lines), got)
	}
	// Top border
	if !strings.HasPrefix(lines[0], "┌") || !strings.HasSuffix(lines[0], "┐") {
		t.Fatalf("top border malformed: %q", lines[0])
	}
	if !strings.Contains(lines[0], "┬") {
		t.Fatalf("top border missing column separator: %q", lines[0])
	}
	// Header
	if !strings.Contains(lines[1], "Component") || !strings.Contains(lines[1], "Detail") || !strings.Contains(lines[1], "Status") {
		t.Fatalf("header row missing column names: %q", lines[1])
	}
	// Separator
	if !strings.HasPrefix(lines[2], "├") || !strings.HasSuffix(lines[2], "┤") {
		t.Fatalf("separator malformed: %q", lines[2])
	}
	if !strings.Contains(lines[2], "┼") {
		t.Fatalf("separator missing column intersection: %q", lines[2])
	}
	// Bottom border
	bottom := lines[len(lines)-1]
	if !strings.HasPrefix(bottom, "└") || !strings.HasSuffix(bottom, "┘") {
		t.Fatalf("bottom border malformed: %q", bottom)
	}
	if !strings.Contains(bottom, "┴") {
		t.Fatalf("bottom border missing column separator: %q", bottom)
	}
	// Data rows contain expected content
	if !strings.Contains(lines[3], "Kubernetes auth") || !strings.Contains(lines[3], "secret store") {
		t.Fatalf("first data row missing content: %q", lines[3])
	}
	if !strings.Contains(lines[6], "Oberth") || !strings.Contains(lines[6], "v0.12.43") {
		t.Fatalf("last data row missing content: %q", lines[6])
	}
}

func TestComponentTableSensitiveRows(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Kubernetes auth", Detail: "secret store", Status: "✓ ready", Sensitive: true},
		{Name: "Oberth", Detail: "v0.12.43", Status: "✓ deployed"},
	}
	componentTable(&buf, rows, false)
	got := buf.String()
	// Sensitive rows must NOT have lock prefix (removed for cleaner output).
	if strings.Contains(got, "\U0001f512") {
		t.Fatalf("sensitive row must not have lock emoji: %q", got)
	}
	// Both rows must render their names without any prefix.
	if !strings.Contains(got, "Kubernetes auth") {
		t.Fatalf("sensitive row missing name: %q", got)
	}
	if !strings.Contains(got, "Oberth") {
		t.Fatalf("non-sensitive row missing name: %q", got)
	}
}

func TestComponentTableColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Auth", Detail: "store", Status: "✓ ready", Sensitive: true},
	}
	componentTable(&buf, rows, true)
	got := buf.String()
	// Dim borders
	if !strings.Contains(got, "\033[2m") {
		t.Fatalf("color=true must produce dim borders, got %q", got)
	}
	// Bold header
	if !strings.Contains(got, "\033[1m") {
		t.Fatalf("color=true must produce bold header, got %q", got)
	}
	// Yellow for sensitive
	if !strings.Contains(got, "\033[33m") {
		t.Fatalf("color=true must produce yellow for sensitive rows, got %q", got)
	}
	// Green checkmark
	if !strings.Contains(got, "\033[32m✓\033[0m") {
		t.Fatalf("color=true must produce green checkmark, got %q", got)
	}
}

func TestComponentTableNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Auth", Detail: "store", Status: "✓ ready", Sensitive: true},
	}
	componentTable(&buf, rows, false)
	got := buf.String()
	if strings.Contains(got, "\033[") {
		t.Fatalf("color=false must not contain ANSI codes, got %q", got)
	}
}

func TestComponentTableWarningStatus(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Network policy", Detail: "disabled", Status: "⚠ warn"},
	}
	componentTable(&buf, rows, true)
	got := buf.String()
	if !strings.Contains(got, "\033[33m⚠\033[0m") {
		t.Fatalf("color=true must produce yellow warning, got %q", got)
	}
}

func TestComponentTableFailedStatus(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Something", Detail: "broken", Status: "✗ failed"},
	}
	componentTable(&buf, rows, true)
	got := buf.String()
	if !strings.Contains(got, "\033[31m✗\033[0m") {
		t.Fatalf("color=true must produce red cross, got %q", got)
	}
}

func TestComponentTableEmptyRows(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	componentTable(&buf, nil, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// top + header + separator + bottom = 4 lines (no data rows)
	if len(lines) != 4 {
		t.Fatalf("empty table expected 4 lines, got %d:\n%s", len(lines), got)
	}
}

func TestCredentialBoxPadding(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	credentialBox(&buf, "Test", "Footer",
		[]credentialRow{
			{Label: "Key1", Value: "val1"},
			{Label: "Key2", Value: "val2"},
		}, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	// Verify structure: top, blank, row1, blank, row2, blank, bottom = 7
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines for padded 2-row box, got %d:\n%s", len(lines), got)
	}

	// Line after the top border must be a padded blank row (│ + spaces + │).
	if !strings.HasPrefix(lines[1], "│") || !strings.HasSuffix(lines[1], "│") {
		t.Fatalf("blank row after top border malformed: %q", lines[1])
	}
	if strings.TrimSpace(strings.Trim(lines[1], "│")) != "" {
		t.Fatalf("row after top border is not blank: %q", lines[1])
	}

	// Line before the bottom border must be a padded blank row.
	if !strings.HasPrefix(lines[5], "│") || !strings.HasSuffix(lines[5], "│") {
		t.Fatalf("blank row before bottom border malformed: %q", lines[5])
	}
	if strings.TrimSpace(strings.Trim(lines[5], "│")) != "" {
		t.Fatalf("row before bottom border is not blank: %q", lines[5])
	}

	// Line between the two content rows must be a padded blank row.
	if !strings.HasPrefix(lines[3], "│") || !strings.HasSuffix(lines[3], "│") {
		t.Fatalf("blank row between content rows malformed: %q", lines[3])
	}
	if strings.TrimSpace(strings.Trim(lines[3], "│")) != "" {
		t.Fatalf("row between content rows is not blank: %q", lines[3])
	}

	// Content rows are at positions 2 and 4.
	if !strings.Contains(lines[2], "val1") {
		t.Fatalf("first content row missing val1: %q", lines[2])
	}
	if !strings.Contains(lines[4], "val2") {
		t.Fatalf("second content row missing val2: %q", lines[4])
	}
}

func TestComponentTableMinimumWidths(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "kubernetes auth", Detail: "secret store", Status: "✓ ready", Sensitive: true},
		{Name: "credentialed policy", Detail: "secret store", Status: "✓ ready", Sensitive: true},
	}
	componentTable(&buf, rows, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// With content that fits within minimums, all lines must have the same
	// width (the minimum total: 31+31+14 + 4 borders = 80 display columns).
	borderWidth := utf8.RuneCountInString(lines[0])
	if borderWidth != tableMinComponent+tableMinDetail+tableMinStatus+4 {
		t.Fatalf("expected minimum table width %d, got %d",
			tableMinComponent+tableMinDetail+tableMinStatus+4, borderWidth)
	}
	for _, line := range lines[1:] {
		if w := utf8.RuneCountInString(line); w != borderWidth {
			t.Fatalf("line width %d does not match border width %d: %q", w, borderWidth, line)
		}
	}
}

func TestComponentTableAutoSize(t *testing.T) {
	t.Parallel()
	longURL := "ssh://git@github.com/valariantech"
	var buf bytes.Buffer
	rows := []tableRow{
		{Name: "Upstream", Detail: longURL, Status: "✓ connected"},
		{Name: "Oberth", Detail: "v0.12.43", Status: "✓ deployed"},
	}
	componentTable(&buf, rows, false)
	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// The Detail column must expand to fit the long URL. All lines must
	// have the same display width.
	borderWidth := utf8.RuneCountInString(lines[0])
	expectedMinDetail := len(longURL) + 2
	expectedMinWidth := tableMinComponent + expectedMinDetail + tableMinStatus + 4
	if borderWidth < expectedMinWidth {
		t.Fatalf("table width %d too narrow for content (expected >= %d)", borderWidth, expectedMinWidth)
	}
	for i, line := range lines {
		if w := utf8.RuneCountInString(line); w != borderWidth {
			t.Fatalf("line %d width %d does not match border width %d: %q", i, w, borderWidth, line)
		}
	}
	// The long URL must appear in full without truncation.
	if !strings.Contains(got, longURL) {
		t.Fatalf("long URL must appear in full: %q", got)
	}
}

// --- tableWriter tests ---

func TestTableWriterStreamingOutput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteRow("Kubernetes auth", "secret store", "✓ ready")
	tw.WriteSensitiveRow("Transit key", "secret store", "✓ ready")
	tw.WriteRow("Oberth", "v0.12.46", "✓ deployed")
	tw.WriteFooter()

	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// top + header + separator + 3 data rows + bottom = 7 lines
	if len(lines) != 7 {
		t.Fatalf("expected 7 lines, got %d:\n%s", len(lines), got)
	}
	// Top border
	if !strings.HasPrefix(lines[0], "┌") || !strings.HasSuffix(lines[0], "┐") {
		t.Fatalf("top border malformed: %q", lines[0])
	}
	// Header
	if !strings.Contains(lines[1], "Component") || !strings.Contains(lines[1], "Detail") || !strings.Contains(lines[1], "Status") {
		t.Fatalf("header row missing column names: %q", lines[1])
	}
	// Data rows
	if !strings.Contains(lines[3], "Kubernetes auth") || !strings.Contains(lines[3], "secret store") {
		t.Fatalf("first data row missing content: %q", lines[3])
	}
	if !strings.Contains(lines[5], "Oberth") || !strings.Contains(lines[5], "v0.12.46") {
		t.Fatalf("last data row missing content: %q", lines[5])
	}
	// Bottom border
	if !strings.HasPrefix(lines[6], "└") || !strings.HasSuffix(lines[6], "┘") {
		t.Fatalf("bottom border malformed: %q", lines[6])
	}
}

func TestTableWriterFooterIdempotent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteFooter()
	first := buf.String()
	tw.WriteFooter() // second call must be a no-op
	if buf.String() != first {
		t.Fatal("second WriteFooter must not produce additional output")
	}
}

func TestTableWriterSpanRow(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteRow("Deploy key", "generated", "✓ stored")
	tw.WriteSpanRow("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINKAaSupc8+h0niEn3nu7mGL1z88U2r0FA1gHZs+n1ea oberth")
	tw.WriteRow("Key registration", "github.com", "✓ accepted")
	tw.WriteFooter()

	got := buf.String()
	if !strings.Contains(got, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINKAaSupc8+h0niEn3nu7mGL1z88U2r0FA1gHZs+n1ea oberth") {
		t.Fatalf("span row must carry the full key without truncation:\n%s", got)
	}
	// The span row must use │ on both edges.
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.Contains(line, "ssh-ed25519") {
			if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
				t.Fatalf("span row must have │ borders: %q", line)
			}
			break
		}
	}
}

func TestTableWriterSpanRowWidthMatchesTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteSpanRow("short content")
	tw.WriteFooter()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	borderWidth := utf8.RuneCountInString(lines[0])
	for _, line := range lines[1:] {
		if width := utf8.RuneCountInString(line); width != borderWidth {
			t.Fatalf("line width %d does not match table width %d: %q", width, borderWidth, line)
		}
	}
}

func TestTableWriterRegisterContentAutoSize(t *testing.T) {
	t.Parallel()
	longURL := "ssh://git@github.com/valariantech"
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.RegisterContent("Upstream", longURL, "✓ connected")
	tw.WriteHeader()
	tw.WriteRow("Upstream", longURL, "✓ connected")
	tw.WriteRow("Oberth", "v0.12.46", "✓ deployed")
	tw.WriteFooter()

	got := buf.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	// All lines must have the same display width.
	borderWidth := utf8.RuneCountInString(lines[0])
	for i, line := range lines[1:] {
		if w := utf8.RuneCountInString(line); w != borderWidth {
			t.Fatalf("line %d width %d does not match border width %d: %q", i+1, w, borderWidth, line)
		}
	}
	// The long URL must appear in full.
	if !strings.Contains(got, longURL) {
		t.Fatalf("registered long URL must appear in full:\n%s", got)
	}
}

func TestTableWriterCompleteRowNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.StartPromptRow("Upstream", "github.com/your-org: ")
	// Simulate user typing (in tests, input goes to readLine, not the writer).
	tw.CompleteRow("Upstream", "ssh://git@github.com/oberthci", "✓ connected", false)
	tw.WriteFooter()

	got := buf.String()
	if !strings.Contains(got, "✓ connected") {
		t.Fatalf("completed row missing status:\n%s", got)
	}
	if !strings.Contains(got, "ssh://git@github.com/oberthci") {
		t.Fatalf("completed row missing detail:\n%s", got)
	}
	// No ANSI codes in non-color mode.
	if strings.Contains(got, "\033[") {
		t.Fatalf("non-color mode must not contain ANSI codes:\n%s", got)
	}
}

func TestTableWriterColorOutput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, true)
	tw.WriteHeader()
	tw.WriteSensitiveRow("Auth", "store", "✓ ready")
	tw.WriteRow("Net", "off", "⚠ warn")
	tw.WriteFooter()

	got := buf.String()
	// Dim borders
	if !strings.Contains(got, "\033[2m") {
		t.Fatalf("color=true must produce dim borders:\n%s", got)
	}
	// Bold header
	if !strings.Contains(got, "\033[1m") {
		t.Fatalf("color=true must produce bold header:\n%s", got)
	}
	// Yellow for sensitive
	if !strings.Contains(got, "\033[33m") {
		t.Fatalf("color=true must produce yellow for sensitive rows:\n%s", got)
	}
	// Green checkmark
	if !strings.Contains(got, "\033[32m✓\033[0m") {
		t.Fatalf("color=true must produce green checkmark:\n%s", got)
	}
	// Yellow warning
	if !strings.Contains(got, "\033[33m⚠\033[0m") {
		t.Fatalf("color=true must produce yellow warning:\n%s", got)
	}
}

// --- startPrompt / completePrompt / writeResult tests ---

func TestStartPromptNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	startPrompt(&buf, false, "Upstream", "github.com/your-org: ")
	got := buf.String()
	if !strings.HasPrefix(got, "  Upstream") {
		t.Fatalf("expected indented label, got %q", got)
	}
	if !strings.Contains(got, "github.com/your-org: ") {
		t.Fatalf("expected prompt text, got %q", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("startPrompt must not end with newline, got %q", got)
	}
	if strings.Contains(got, "\033[") {
		t.Fatalf("no-color mode must not contain ANSI codes, got %q", got)
	}
}

func TestStartPromptColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	startPrompt(&buf, true, "Upstream", "github.com/your-org: ")
	got := buf.String()
	if !strings.Contains(got, "\033[1m") {
		t.Fatalf("color=true must produce bold label, got %q", got)
	}
	if !strings.Contains(got, "Upstream") {
		t.Fatalf("expected label, got %q", got)
	}
}

func TestWriteResultWithSymbol(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeResult(&buf, false, "Upstream", "github.com", "✓ connected")
	got := buf.String()
	if !strings.HasPrefix(got, "  ✓") {
		t.Fatalf("expected symbol at start, got %q", got)
	}
	if !strings.Contains(got, "Upstream") {
		t.Fatalf("expected label, got %q", got)
	}
	if !strings.Contains(got, "github.com") {
		t.Fatalf("expected detail, got %q", got)
	}
	if !strings.Contains(got, "connected") {
		t.Fatalf("expected status word, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("writeResult must end with newline, got %q", got)
	}
}

func TestWriteResultWithoutSymbol(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeResult(&buf, false, "Upstream", "", "skipped")
	got := buf.String()
	if !strings.HasPrefix(got, "    ") {
		t.Fatalf("no-symbol result must start with 4-space indent, got %q", got)
	}
	if !strings.Contains(got, "skipped") {
		t.Fatalf("expected status, got %q", got)
	}
}

func TestWriteResultColorizedSymbol(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeResult(&buf, true, "Auth", "store", "✓ ready")
	got := buf.String()
	if !strings.Contains(got, "\033[32m✓\033[0m") {
		t.Fatalf("color=true must produce green checkmark, got %q", got)
	}
}

func TestWriteResultWarning(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeResult(&buf, true, "Net", "disabled", "⚠ warn")
	got := buf.String()
	if !strings.Contains(got, "\033[33m⚠\033[0m") {
		t.Fatalf("color=true must produce yellow warning, got %q", got)
	}
}

func TestWriteResultError(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	writeResult(&buf, true, "Key", "path", "✗ failed")
	got := buf.String()
	if !strings.Contains(got, "\033[31m✗\033[0m") {
		t.Fatalf("color=true must produce red cross, got %q", got)
	}
}

func TestCompletePromptNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	completePrompt(&buf, false, "Upstream", "ssh://git@github.com/org", "✓ connected")
	got := buf.String()
	// No ANSI codes in non-color mode — the cursor-up escape is skipped.
	if strings.Contains(got, "\033[") {
		t.Fatalf("non-color mode must not contain ANSI codes, got %q", got)
	}
	if !strings.Contains(got, "connected") {
		t.Fatalf("expected status, got %q", got)
	}
}

func TestCompletePromptColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	completePrompt(&buf, true, "Upstream", "ssh://git@github.com/org", "✓ connected")
	got := buf.String()
	// Must contain the cursor-up-and-clear sequence.
	if !strings.Contains(got, "\033[1A\r\033[2K") {
		t.Fatalf("color mode must contain cursor-up-and-clear, got %q", got)
	}
	if !strings.Contains(got, "\033[32m✓\033[0m") {
		t.Fatalf("color mode must colorize the checkmark, got %q", got)
	}
}

func TestSplitStatusSymbol(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input      string
		wantSymbol string
		wantText   string
	}{
		{"✓ connected", "✓", "connected"},
		{"⚠ warn", "⚠", "warn"},
		{"✗ failed", "✗", "failed"},
		{"… pending", "…", "pending"},
		{"skipped", "", "skipped"},
		{"", "", ""},
		{"✓", "", "✓"}, // no space after symbol
	} {
		symbol, text := splitStatusSymbol(tc.input)
		if symbol != tc.wantSymbol || text != tc.wantText {
			t.Errorf("splitStatusSymbol(%q) = (%q, %q), want (%q, %q)",
				tc.input, symbol, text, tc.wantSymbol, tc.wantText)
		}
	}
}

// --- AppendRow tests ---

func TestTableWriterAppendRowNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteRow("Oberth", "v0.12.46", "✓ deployed")
	tw.WriteFooter()
	tw.AppendRow("Upstream", "github.com", "✓ connected", false)
	tw.AppendRow("Uplink", "tester@box", "✓ registered", false)

	got := buf.String()
	// In no-color mode, AppendRow just prints data rows without cursor tricks.
	// The footer is only written once (by WriteFooter). AppendRow does not
	// redraw the border in no-color mode.
	if !strings.Contains(got, "Upstream") || !strings.Contains(got, "github.com") {
		t.Fatalf("appended row missing content:\n%s", got)
	}
	if !strings.Contains(got, "Uplink") || !strings.Contains(got, "tester@box") {
		t.Fatalf("second appended row missing content:\n%s", got)
	}
	// No ANSI codes.
	if strings.Contains(got, "\033[") {
		t.Fatalf("no-color mode must not contain ANSI codes:\n%s", got)
	}
}

func TestTableWriterAppendRowColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, true)
	tw.WriteHeader()
	tw.WriteRow("Oberth", "v0.12.46", "✓ deployed")
	tw.WriteFooter()

	beforeAppend := buf.String()
	tw.AppendRow("Upstream", "github.com", "✓ connected", false)
	afterAppend := buf.String()

	// In color mode, AppendRow erases the bottom border (cursor-up+clear),
	// writes the new row, and redraws the border.
	if !strings.Contains(afterAppend, "\033[1A\r\033[2K") {
		t.Fatalf("color mode AppendRow must contain cursor-up-and-clear:\n%s", afterAppend)
	}
	if !strings.Contains(afterAppend, "Upstream") || !strings.Contains(afterAppend, "github.com") {
		t.Fatalf("appended row missing content:\n%s", afterAppend)
	}
	// After AppendRow, the output should end with a bottom border.
	lines := strings.Split(strings.TrimRight(afterAppend, "\n"), "\n")
	lastLine := lines[len(lines)-1]
	// Strip ANSI codes for structural check.
	plain := stripANSI(lastLine)
	if !strings.HasPrefix(plain, "└") || !strings.HasSuffix(plain, "┘") {
		t.Fatalf("after AppendRow, output must end with bottom border: %q (plain: %q)", lastLine, plain)
	}
	_ = beforeAppend
}

func TestTableWriterAppendSpanRowNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, false)
	tw.WriteHeader()
	tw.WriteRow("Deploy key", "generated", "✓ stored")
	tw.WriteFooter()
	tw.AppendSpanRow("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINKAaSupc8+h0niEn3nu7mGL1z88U2r0FA1gHZs+n1ea oberth")

	got := buf.String()
	if !strings.Contains(got, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINKAaSupc8+h0niEn3nu7mGL1z88U2r0FA1gHZs+n1ea oberth") {
		t.Fatalf("appended span row must carry the full key:\n%s", got)
	}
}

func TestTableWriterAppendRowSensitive(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tw := newTableWriter(&buf, true)
	tw.WriteHeader()
	tw.WriteFooter()
	tw.AppendRow("Auth", "store", "✓ ready", true)

	got := buf.String()
	// Yellow highlighting for sensitive rows.
	if !strings.Contains(got, "\033[33m") {
		t.Fatalf("sensitive AppendRow must produce yellow highlighting:\n%s", got)
	}
}

// --- heldCredentials tests ---

func TestHeldCredentialsFlush(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var creds heldCredentials
	creds.add("Root token", "hvs.abc123")
	creds.add("Unseal key", "xyz789")
	creds.add("Bearer token", "oberth_tok")
	creds.flush(&buf, false)

	got := buf.String()
	if !strings.Contains(got, "Credentials (save now, shown once)") {
		t.Fatalf("missing credential box title:\n%s", got)
	}
	if !strings.Contains(got, "hvs.abc123") {
		t.Fatalf("missing root token:\n%s", got)
	}
	if !strings.Contains(got, "xyz789") {
		t.Fatalf("missing unseal key:\n%s", got)
	}
	if !strings.Contains(got, "oberth_tok") {
		t.Fatalf("missing bearer token:\n%s", got)
	}
}

func TestHeldCredentialsFlushIdempotent(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var creds heldCredentials
	creds.add("Token", "abc")
	creds.flush(&buf, false)
	first := buf.String()
	creds.flush(&buf, false)
	if buf.String() != first {
		t.Fatal("second flush must be a no-op")
	}
}

func TestHeldCredentialsFlushEmpty(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var creds heldCredentials
	creds.flush(&buf, false)
	if buf.String() != "" {
		t.Fatal("flush with no credentials must produce no output")
	}
}

func TestErasePromptLinesNoColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	erasePromptLines(&buf, false, 3)
	if buf.String() != "" {
		t.Fatal("erasePromptLines with color=false must produce no output")
	}
}

func TestErasePromptLinesColor(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	erasePromptLines(&buf, true, 2)
	got := buf.String()
	// Should contain two cursor-up-and-clear sequences.
	if strings.Count(got, "\033[1A\r\033[2K") != 2 {
		t.Fatalf("expected 2 cursor-up-and-clear sequences, got %q", got)
	}
}

// stripANSI removes ANSI escape sequences from a string for structural checks.
func stripANSI(s string) string {
	var result strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
				inEscape = false
			}
			continue
		}
		result.WriteRune(r)
	}
	return result.String()
}

// --- credential emission: a wired sink must fully replace the boxed flush ---
//
// The TUI wizard's Output is a log surface (tail + retained lines). When it
// wires a CredentialSink, no credential value — root token, unseal key,
// bearer token — may reach the output stream in any form: the box format
// wraps every line in border runes, which defeats any output-side token
// detection, so the only safe contract is "sink set ⇒ zero credential bytes
// on the writer".

func TestEmitCredentialsPrefersSinkAndWritesNothing(t *testing.T) {
	var creds heldCredentials
	creds.add("Root token", "hvs-fake-root")
	creds.add("Unseal key", "unseal-fake-b64")
	creds.add("Bearer token", "oberth_fake_bearer")

	var out bytes.Buffer
	var got [][2]string
	deps := Deps{CredentialSink: func(label, value string) {
		got = append(got, [2]string{label, value})
	}}

	emitCredentials(&creds, deps, &out, false)

	if out.Len() != 0 {
		t.Fatalf("sink delivery must write NOTHING to the output stream, wrote: %q", out.String())
	}
	want := [][2]string{
		{"Root token", "hvs-fake-root"},
		{"Unseal key", "unseal-fake-b64"},
		{"Bearer token", "oberth_fake_bearer"},
	}
	if len(got) != len(want) {
		t.Fatalf("sink received %d credentials, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("credential %d = %v, want %v", i, got[i], want[i])
		}
	}

	// Shown-once latch is shared: neither a second emit nor a direct flush
	// may release the values again.
	got = nil
	emitCredentials(&creds, deps, &out, false)
	creds.flush(&out, false)
	if len(got) != 0 || out.Len() != 0 {
		t.Fatal("credentials released twice — the shown-once latch must cover sink and flush")
	}
}

func TestEmitCredentialsWithoutSinkFlushesBox(t *testing.T) {
	var creds heldCredentials
	creds.add("Bearer token", "oberth_fake_bearer")

	var out bytes.Buffer
	emitCredentials(&creds, Deps{}, &out, false)

	if !strings.Contains(out.String(), "oberth_fake_bearer") {
		t.Fatal("without a sink the boxed flush must still print the credential")
	}
}
