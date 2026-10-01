package setuptui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type storePage struct {
	cursor int // 0=install-dev, 1=install-prod, 2=connect
}

func newStorePage() *storePage {
	return &storePage{cursor: 1}
}

func (p *storePage) title() string    { return "propellant" }
func (p *storePage) question() string { return "Where do release secrets live?" }
func (p *storePage) keys() string {
	return sKey.Render("↑/↓") + " choose · " + sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *storePage) init(state *WizardState) tea.Cmd {
	switch state.StoreMode {
	case "install-dev":
		p.cursor = 0
	case "install-prod":
		p.cursor = 1
	case "connect":
		p.cursor = 2
	default:
		p.cursor = 1
	}
	return nil
}

func (p *storePage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			if p.cursor > 0 {
				p.cursor--
			}
		case "down", "j":
			if p.cursor < 2 {
				p.cursor++
			}
		case "enter":
			switch p.cursor {
			case 0:
				state.StoreMode = "install-dev"
				state.Config.InstallSecretStoreDev = true
				state.Config.InstallSecretStore = false
				state.Config.SecretStoreUndecided = false
			case 1:
				state.StoreMode = "install-prod"
				state.Config.InstallSecretStore = true
				state.Config.InstallSecretStoreDev = false
				state.Config.SecretStoreUndecided = false
			case 2:
				state.StoreMode = "connect"
				state.Config.InstallSecretStore = false
				state.Config.InstallSecretStoreDev = false
				state.Config.SecretStoreUndecided = false
			}
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		}
	}
	return p, nil
}

func (p *storePage) view(_ *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	options := []struct {
		label       string
		description string
	}{
		{"install openbao — dev", "evaluation only · unseals itself"},
		{"install openbao — production", "runs in this cluster · you hold the unseal keys"},
		{"connect existing openbao / vault", "bring your own store — reachable over https"},
	}

	for i, opt := range options {
		cursor := "      "
		radio := "( )"
		if i == p.cursor {
			cursor = "    " + lipgloss.NewStyle().Foreground(cPurple).Render("❯") + " "
			radio = "(•)"
		}

		radioStyled := lipgloss.NewStyle().Foreground(cPurple).Render(radio)
		label := sText.Render(opt.label)
		desc := sMuted.Render(opt.description)

		b.WriteString(cursor + radioStyled + " " + label + "\n")
		b.WriteString("          " + desc + "\n\n")
	}

	return b.String()
}
