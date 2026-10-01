package setuptui

import (
	"fmt"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// dns1123LabelRegexp validates a DNS-1123 label.
var dns1123LabelRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// namespaceRule is the human reading of dns1123LabelRegexp, shared by the
// TUI and plain paths so a rejected name is explained the same way twice.
const namespaceRule = "lowercase letters, digits and dashes only — start and end with a letter or digit"

type namespacesPage struct {
	fields [3]fieldState
	focus  int
	errMsg string
}

type fieldState struct {
	label       string
	value       string
	description string
	defaultVal  string
}

func newNamespacesPage() *namespacesPage {
	return &namespacesPage{
		fields: [3]fieldState{
			{label: "oberth", defaultVal: "oberth", description: "where oberth itself runs"},
			// defaultVal must match installer.DefaultArgoNamespace ("oberth-argo").
			{label: "pipelines", defaultVal: "oberth-argo", description: "where CI jobs run — kept apart from oberth itself"},
			{label: "openbao", defaultVal: "openbao", description: "where the secret store runs"},
		},
	}
}

func (p *namespacesPage) title() string    { return "flight plan" }
func (p *namespacesPage) question() string { return "Name the namespaces." }
func (p *namespacesPage) keys() string {
	return sKey.Render("↑/↓") + " fields · " + sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *namespacesPage) init(state *WizardState) tea.Cmd {
	if state.Config.Namespace != "" {
		p.fields[0].value = state.Config.Namespace
	} else {
		p.fields[0].value = p.fields[0].defaultVal
	}
	if state.Config.ArgoNamespace != "" {
		p.fields[1].value = state.Config.ArgoNamespace
	} else {
		p.fields[1].value = p.fields[1].defaultVal
	}
	if state.Config.OpenBaoNamespace != "" {
		p.fields[2].value = state.Config.OpenBaoNamespace
	} else {
		p.fields[2].value = p.fields[2].defaultVal
	}
	p.focus = 0
	p.errMsg = ""
	return nil
}

func (p *namespacesPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "tab", "down":
			p.focus = (p.focus + 1) % 3
			p.errMsg = ""
		case "shift+tab", "up":
			p.focus = (p.focus + 2) % 3
			p.errMsg = ""
		case "enter":
			if err := p.validate(); err != "" {
				p.errMsg = err
				return p, nil
			}
			state.Config.Namespace = p.fields[0].value
			state.Config.ArgoNamespace = p.fields[1].value
			state.Config.OpenBaoNamespace = p.fields[2].value
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		case "backspace":
			v := p.fields[p.focus].value
			if len(v) > 0 {
				p.fields[p.focus].value = v[:len(v)-1]
			}
		default:
			text := msg.String()
			if len(text) == 1 && isNSChar(text[0]) {
				p.fields[p.focus].value += text
			}
		}
	}
	return p, nil
}

func (p *namespacesPage) validate() string {
	for _, f := range p.fields {
		if !dns1123LabelRegexp.MatchString(f.value) {
			return fmt.Sprintf("%s: %s", f.label, namespaceRule)
		}
	}
	// Namespaces must be distinct.
	if p.fields[0].value == p.fields[1].value {
		return "oberth and pipelines namespaces must differ"
	}
	if p.fields[0].value == p.fields[2].value {
		return "oberth and openbao namespaces must differ"
	}
	if p.fields[1].value == p.fields[2].value {
		return "pipelines and openbao namespaces must differ"
	}
	return ""
}

func (p *namespacesPage) view(_ *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	for i, f := range p.fields {
		cursor := "  "
		labelStyle := sMuted
		if i == p.focus {
			cursor = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
			labelStyle = lipgloss.NewStyle().Foreground(cPurple)
		}

		input := inputBox(f.value, f.defaultVal, i == p.focus)

		// Pad the plain label, then style it — padding a styled string
		// counts the escape codes and the columns drift.
		_, _ = fmt.Fprintf(&b, "  %s%s %s\n", cursor, labelStyle.Render(fmt.Sprintf("%-14s", f.label)), input)

		// Description under focused field.
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

func isNSChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
}
