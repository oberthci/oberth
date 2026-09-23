package setuptui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type storeConnectPage struct {
	address string
	errMsg  string
}

func newStoreConnectPage() *storeConnectPage {
	return &storeConnectPage{}
}

func (p *storeConnectPage) title() string    { return "propellant" }
func (p *storeConnectPage) question() string { return "Connect the store." }
func (p *storeConnectPage) keys() string {
	return sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *storeConnectPage) init(state *WizardState) tea.Cmd {
	if state.StoreAddress != "" {
		p.address = state.StoreAddress
	}
	p.errMsg = ""
	return nil
}

func (p *storeConnectPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			if err := validateStoreAddress(p.address); err != nil {
				p.errMsg = err.Error()
				return p, nil
			}
			state.StoreAddress = p.address
			state.Config.ArgoVaultAddress = p.address
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		case "backspace":
			if len(p.address) > 0 {
				p.address = p.address[:len(p.address)-1]
			}
		default:
			text := msg.String()
			if len(text) == 1 {
				p.address += text
			}
		}
	}
	return p, nil
}

func (p *storeConnectPage) view(_ *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	// Address field.
	cursor := lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
	labelStyle := lipgloss.NewStyle().Foreground(cPurple)
	input := inputBox(p.address, "https://…", true)

	_, _ = fmt.Fprintf(&b, "  %s%s %s\n", cursor, labelStyle.Render(fmt.Sprintf("%-16s", "address")), input)
	b.WriteString("    " + sMuted.Render("where your OpenBao or Vault answers — https only") + "\n\n")

	// Auth note.
	b.WriteString("  " + sMuted.Render("oberth signs in with its own kubernetes identity (role oberth-secretstore)") + "\n")
	b.WriteString("  " + sMuted.Render("and ") + sText.Render("never") +
		sMuted.Render(" asks for an admin token — grant the role with setup-secretstore.sh") + "\n\n")

	// Post-install note — specific knobs, not vague hand-waving.
	b.WriteString("  " + sMuted.Render("after install, configure the CA and approve secret paths:") + "\n")
	b.WriteString("    " + sInfo.Render("helm upgrade oberth ... --set argo.vault.caCert=$(base64 < ca.pem)") + "\n")
	b.WriteString("    " + sInfo.Render("oberth access allow <repo> <step> <path>") + "\n")

	if p.errMsg != "" {
		b.WriteString("\n  " + sFail.Render(p.errMsg) + "\n")
	}

	return b.String()
}
