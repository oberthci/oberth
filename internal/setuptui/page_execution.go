package setuptui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type executionPage struct {
	fields [2]execField
	focus  int
	errMsg string
}

type execField struct {
	label       string
	value       string
	fieldType   string // "number", "select", "port"
	options     []string
	optionIndex int
	description string
}

func newExecutionPage() *executionPage {
	return &executionPage{
		fields: [2]execField{
			// Default to "auto" to match installer.Config.NetworkPolicy default
			// ("auto" → false on k3s due to kube-router DNAT incompatibility).
			{label: "network policy", value: "auto", fieldType: "select",
				options: []string{"auto", "strict", "off"}, optionIndex: 0,
				description: "auto — where supported · strict — firewall CI jobs' network egress · off"},
			{label: "external anchoring", value: "off", fieldType: "select",
				options: []string{"off", "on"}, optionIndex: 0,
				description: "off — contacts no external service · on — local Rekor log as audit witness"},
		},
	}
}

func (p *executionPage) title() string    { return "flight plan" }
func (p *executionPage) question() string { return "Network and audit." }
func (p *executionPage) keys() string {
	return sKey.Render("↑/↓") + " fields · " + sKey.Render("←/→") + " options · " + sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *executionPage) init(state *WizardState) tea.Cmd {
	if state.Config.NetworkPolicy != "" {
		// Config holds the installer vocabulary (auto|true|false); the page
		// displays friendlier labels. Reverse-map for round-tripping.
		display := map[string]string{"true": "strict", "auto": "auto", "false": "off"}
		if label, ok := display[state.Config.NetworkPolicy]; ok {
			for i, opt := range p.fields[0].options {
				if opt == label {
					p.fields[0].optionIndex = i
					p.fields[0].value = opt
				}
			}
		}
	}
	if state.Config.InstallRekor {
		p.fields[1].optionIndex = 1
		p.fields[1].value = "on"
	}
	p.focus = 0
	p.errMsg = ""
	return nil
}

func (p *executionPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "tab", "down":
			p.focus = (p.focus + 1) % len(p.fields)
			p.errMsg = ""
		case "shift+tab", "up":
			p.focus = (p.focus + len(p.fields) - 1) % len(p.fields)
			p.errMsg = ""
		case "left":
			f := &p.fields[p.focus]
			if f.fieldType == "select" && f.optionIndex > 0 {
				f.optionIndex--
				f.value = f.options[f.optionIndex]
			}
		case "right":
			f := &p.fields[p.focus]
			if f.fieldType == "select" && f.optionIndex < len(f.options)-1 {
				f.optionIndex++
				f.value = f.options[f.optionIndex]
			}
		case "enter":
			// Write the installer's vocabulary, never the display label —
			// installer.Config rejects anything but auto|true|false, and the
			// --dry-mode command must be a valid `oberth install` invocation.
			if np, ok := canonicalNetworkPolicy(p.fields[0].value); ok {
				state.Config.NetworkPolicy = np
			} else {
				p.errMsg = "network policy must be strict, auto, or off"
				return p, nil
			}
			state.Config.InstallRekor = p.fields[1].value == "on"
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		}
	}
	return p, nil
}

func (p *executionPage) view(_ *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	for i, f := range p.fields {
		cursor := "  "
		labelStyle := sMuted
		if i == p.focus {
			cursor = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
			labelStyle = lipgloss.NewStyle().Foreground(cPurple)
		}

		var input string
		switch f.fieldType {
		case "select":
			input = lipgloss.NewStyle().
				Background(cLine).
				Foreground(cFg).
				Padding(0, 1).
				Render(f.value + " ▾")
		default:
			input = lipgloss.NewStyle().
				Background(cLine).
				Foreground(cFg).
				Padding(0, 1).
				Render(f.value)
		}

		_, _ = fmt.Fprintf(&b, "  %s%s %s\n", cursor, labelStyle.Render(fmt.Sprintf("%-20s", f.label)), input)

		if i == p.focus && f.description != "" {
			b.WriteString("    " + sMuted.Render(f.description) + "\n")
		}
		b.WriteString("\n")
	}

	if p.errMsg != "" {
		b.WriteString("  " + sFail.Render(p.errMsg) + "\n")
	}

	return b.String()
}
