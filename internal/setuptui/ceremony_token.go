package setuptui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ceremonyEntry is one once-only credential held by the ceremony: the uplink
// bearer token, the OpenBao root token, or an unseal key. The value lives in
// one []byte owned by this model — never fmt-formatted into an immortal
// string, never logged, never placed in WizardState — and is zeroed when the
// ceremony is acknowledged or torn down.
type ceremonyEntry struct {
	label string
	value []byte
}

// ceremonyPage handles the once-only credential ceremony (S1/S2/S8). A
// production install can mint several credentials (root token, unseal keys,
// bearer token); every one of them is delivered here structurally via the
// installer's CredentialSink — never parsed back out of the output stream —
// and every one is zeroed on acknowledge and on every teardown path.
type ceremonyPage struct {
	entries      []ceremonyEntry
	cursor       int // selected entry for 'c' copy
	revealed     bool
	copied       bool
	acknowledged bool
}

func newCeremonyPage() *ceremonyPage {
	return &ceremonyPage{}
}

func (p *ceremonyPage) title() string    { return "crew manifest" }
func (p *ceremonyPage) question() string { return "" }
func (p *ceremonyPage) keys() string {
	nav := ""
	if len(p.entries) > 1 {
		nav = sKey.Render("↑/↓") + " select · "
	}
	return nav + sKey.Render("r") + " reveal · " + sKey.Render("c") + " copy · " + sKey.Render("enter") + " acknowledge (no esc — explicit ack required)"
}

func (p *ceremonyPage) init(_ *WizardState) tea.Cmd {
	return nil
}

// addCredential takes ownership of one credential value: it copies the bytes
// into the ceremony's own buffer and ZEROS THE SOURCE (S2 — the buffer that
// delivered it must not linger on the heap). Multiple credentials accumulate;
// acknowledgment covers and zeros all of them.
func (p *ceremonyPage) addCredential(label string, value []byte) {
	cp := make([]byte, len(value))
	copy(cp, value)
	for i := range value {
		value[i] = 0
	}
	p.entries = append(p.entries, ceremonyEntry{label: label, value: cp})
	p.acknowledged = false
	p.revealed = false
	p.copied = false
}

// hasCredentials reports whether any un-zeroed credential is held.
func (p *ceremonyPage) hasCredentials() bool {
	return len(p.entries) > 0
}

func (p *ceremonyPage) update(msg tea.Msg, _ *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			if p.cursor > 0 {
				p.cursor--
			}
			return p, nil
		case "down", "j":
			if p.cursor < len(p.entries)-1 {
				p.cursor++
			}
			return p, nil
		case "r":
			p.revealed = true
			return p, nil
		case "c":
			if p.cursor < len(p.entries) && len(p.entries[p.cursor].value) > 0 {
				p.copied = true
				p.revealed = true
				// OSC 52 clipboard copy of the SELECTED credential
				// (S8: opt-in only, explicitly warned).
				return p, tea.SetClipboard(string(p.entries[p.cursor].value))
			}
		case "enter":
			if p.revealed || p.copied {
				p.acknowledged = true
				// Zero every credential (S2).
				p.zeroAll()
				return p, func() tea.Msg { return pageCompleteMsg{} }
			}
			// Cannot acknowledge without reveal or copy.
			return p, nil
		}
		// No esc — explicit ack required (design doc 5.14).
	}
	return p, nil
}

func (p *ceremonyPage) view(_ *WizardState, width, _ int) string {
	var b strings.Builder

	plural := "token"
	if len(p.entries) > 1 {
		plural = "credentials"
	}
	b.WriteString("  " + sText.Render("Store the "+plural+" now — they will not exist again.") + "\n\n")

	// Credential ceremony box (double border, Red — the one heavy border).
	var boxContent strings.Builder
	boxContent.WriteString("\n")
	boxContent.WriteString(sMuted.Render("stored server-side as digests only. this screen is the only copy that will exist.") + "\n\n")

	labelWidth := 0
	for _, e := range p.entries {
		if len(e.label) > labelWidth {
			labelWidth = len(e.label)
		}
	}

	for i, e := range p.entries {
		display := sHighlight.Render(strings.Repeat("█", 40))
		if p.revealed && len(e.value) > 0 {
			display = lipgloss.NewStyle().
				Background(cLine).
				Foreground(cFg).
				Render(string(e.value))
		}
		marker := "  "
		if len(p.entries) > 1 && i == p.cursor {
			marker = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
		}
		label := e.label + strings.Repeat(" ", labelWidth-len(e.label))
		boxContent.WriteString(marker + sMuted.Render(label) + "   " + display + "\n")
	}
	boxContent.WriteString("\n            " + sKey.Render("r") + " reveal · " + sKey.Render("c") + " copy selected\n\n")

	boxContent.WriteString("  " + sMuted.Render("bearer token destination: ") +
		sInfo.Render(".claude/settings.local.json") +
		sMuted.Render(" — you paste it; oberth never writes it") + "\n")
	boxContent.WriteString("  " + sMuted.Render("clipboard (osc 52) is outside oberth's control — prefer reveal-and-type") + "\n\n")

	// Acknowledge button.
	if p.revealed || p.copied {
		boxContent.WriteString("  " + sKey.Render("enter") +
			sMuted.Render(" — i have stored every value above") + "\n")
	} else {
		boxContent.WriteString("  " + sMuted.Render("enter — i have stored every value above") +
			sMuted.Render("             (enabled after reveal or copy)") + "\n")
	}

	boxWidth := min(78, width-8)
	box := sCeremonyBox.Width(boxWidth).Render(boxContent.String())

	// Center the box.
	pad := (width - lipgloss.Width(box)) / 2
	if pad < 0 {
		pad = 0
	}
	for _, line := range strings.Split(box, "\n") {
		b.WriteString(strings.Repeat(" ", pad) + line + "\n")
	}

	b.WriteString("\n  " + sGo.Render("leaving this screen discards the values — alt buffer: no scrollback, no logs") + "\n")

	return b.String()
}

// zeroAll securely zeros every credential buffer (S2).
func (p *ceremonyPage) zeroAll() {
	for i := range p.entries {
		for j := range p.entries[i].value {
			p.entries[i].value[j] = 0
		}
		p.entries[i].value = nil
	}
	p.entries = nil
	p.cursor = 0
}
