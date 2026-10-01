package setuptui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// forgePage asks where green code is published: which forge, which
// organization owns the repositories, and how oberth authenticates to it.
// Three labelled sections share one focus — up/down (or tab/shift+tab)
// moves between them, left/right picks within the focused one. Focus is
// shown by the section label alone; the only ❯ on the page marks the
// chosen forge.
type forgePage struct {
	forgeOptions []string
	forgeCursor  int
	org          string
	authCursor   int // index into forgeAuthOptions
	focusField   int // forgeFocusForge, forgeFocusOrg, forgeFocusAuth
	errMsg       string

	// Discovery state. The probe is a stub today, so the key is not
	// advertised and the section renders only once there is a result.
	discovering     bool
	discoveryResult *forgeDiscoveryMsg
}

const (
	forgeFocusForge = 0
	forgeFocusOrg   = 1
	forgeFocusAuth  = 2
	forgeFieldCount = 3
)

// forgeAuthOptions is the authentication radio. The token option has no
// delivery path to OpenBao yet: the cursor may rest on it so the user can
// read what is coming, but enter never accepts it.
var forgeAuthOptions = []struct {
	value       string
	label       string
	description string
	comingSoon  bool
}{
	{value: "deploy-key", label: "deploy key per repo",
		description: "generated at install, you add the public half"},
	{value: "token", label: "forge token via openbao",
		description: "coming soon", comingSoon: true},
}

// forgeComingSoon lists forge types displayed but not yet functional (no URL
// mapping in forgeUpstreamURL). The cursor may rest on them; enter rejects.
var forgeComingSoon = map[string]bool{
	"forgejo": true,
}

func newForgePage() *forgePage {
	return &forgePage{
		forgeOptions: []string{"codeberg", "github", "forgejo", "gitlab"},
	}
}

func (p *forgePage) title() string    { return "ground station" }
func (p *forgePage) question() string { return "Where does green code go?" }
func (p *forgePage) keys() string {
	return sKey.Render("↑/↓") + " sections · " + sKey.Render("←/→") + " choose · " + sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *forgePage) init(state *WizardState) tea.Cmd {
	for i, opt := range p.forgeOptions {
		if opt == state.ForgeType {
			p.forgeCursor = i
			break
		}
	}
	if state.ForgeOrg != "" {
		p.org = state.ForgeOrg
	}
	p.authCursor = 0
	for i, opt := range forgeAuthOptions {
		if opt.value == state.ForgeAuth {
			p.authCursor = i
		}
	}
	p.focusField = forgeFocusForge
	p.errMsg = ""
	p.discoveryResult = nil
	return nil
}

func (p *forgePage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case forgeDiscoveryMsg:
		p.discovering = false
		p.discoveryResult = &msg
		if msg.err != nil {
			p.errMsg = msg.err.Error()
		}
		return p, nil

	case tea.KeyPressMsg:
		key := msg.String()
		switch key {
		case "down", "tab":
			// Inside the authentication list, down walks the options first;
			// past the last one (or from any other section) it moves on to
			// the next section, wrapping like tab.
			if key == "down" && p.focusField == forgeFocusAuth && p.authCursor < len(forgeAuthOptions)-1 {
				p.authCursor++
			} else {
				p.focusField = (p.focusField + 1) % forgeFieldCount
			}
			p.errMsg = ""
		case "up", "shift+tab":
			if key == "up" && p.focusField == forgeFocusAuth && p.authCursor > 0 {
				p.authCursor--
			} else {
				p.focusField = (p.focusField + forgeFieldCount - 1) % forgeFieldCount
			}
			p.errMsg = ""
		case "left":
			switch p.focusField {
			case forgeFocusForge:
				if p.forgeCursor > 0 {
					p.forgeCursor--
				}
			case forgeFocusAuth:
				if p.authCursor > 0 {
					p.authCursor--
				}
			}
		case "right":
			switch p.focusField {
			case forgeFocusForge:
				if p.forgeCursor < len(p.forgeOptions)-1 {
					p.forgeCursor++
				}
			case forgeFocusAuth:
				if p.authCursor < len(forgeAuthOptions)-1 {
					p.authCursor++
				}
			}
		case "d":
			// Guard: do not steal 'd' from the organization text field.
			if p.focusField == forgeFocusOrg {
				p.org += "d"
				return p, nil
			}
			if p.org == "" {
				p.errMsg = "enter an organization first"
				return p, nil
			}
			p.discovering = true
			p.errMsg = ""
			return p, probeForge(p.forgeOptions[p.forgeCursor], p.org)
		case "enter":
			if p.org == "" {
				p.errMsg = "organization is required"
				return p, nil
			}
			selectedForge := p.forgeOptions[p.forgeCursor]
			if forgeComingSoon[selectedForge] {
				p.errMsg = selectedForge + " support is coming soon — choose codeberg, github, or gitlab"
				return p, nil
			}
			if forgeAuthOptions[p.authCursor].comingSoon {
				p.errMsg = "forge token via openbao is coming soon — choose deploy key per repo for now"
				return p, nil
			}
			state.ForgeType = p.forgeOptions[p.forgeCursor]
			state.ForgeOrg = p.org
			state.ForgeAuth = forgeAuthOptions[p.authCursor].value
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		case "backspace":
			if p.focusField == forgeFocusOrg && len(p.org) > 0 {
				p.org = p.org[:len(p.org)-1]
			}
		default:
			if p.focusField == forgeFocusOrg && len(key) == 1 {
				p.org += key
			}
		}
	}
	return p, nil
}

func (p *forgePage) view(_ *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	// Forge — a horizontal radio; ❯ marks the chosen forge.
	b.WriteString("  " + sectionLabel("Forge", p.focusField == forgeFocusForge) + "\n")
	b.WriteString("    ")
	for i, opt := range p.forgeOptions {
		label := opt
		if forgeComingSoon[opt] {
			label = opt + " (soon)"
		}
		if i == p.forgeCursor {
			chosen := lipgloss.NewStyle().Foreground(cPurple).Bold(p.focusField == forgeFocusForge)
			if forgeComingSoon[opt] {
				chosen = sHold
			}
			b.WriteString(chosen.Render("❯ " + label))
		} else {
			style := sMuted
			if forgeComingSoon[opt] {
				style = sHold
			}
			b.WriteString("  " + style.Render(label))
		}
		if i < len(p.forgeOptions)-1 {
			b.WriteString("     ")
		}
	}
	b.WriteString("\n\n")

	// Organization — a text field; the cursor inside the box shows focus.
	b.WriteString("  " + sectionLabel("Organization", p.focusField == forgeFocusOrg) + "\n")
	b.WriteString("    " + inputBox(p.org, "your-org", p.focusField == forgeFocusOrg) + "\n")
	b.WriteString("    " + sMuted.Render("the owner or organization that holds your repositories") + "\n\n")

	// Authentication — a vertical radio.
	b.WriteString("  " + sectionLabel("Authentication", p.focusField == forgeFocusAuth) + "\n")
	for i, opt := range forgeAuthOptions {
		radio := "( )"
		if i == p.authCursor {
			radio = "(•)"
		}
		descStyle := sMuted
		if opt.comingSoon {
			descStyle = sHold
		}
		b.WriteString("    " + lipgloss.NewStyle().Foreground(cPurple).Render(radio) + " " +
			sText.Render(opt.label) + sMuted.Render(" — ") + descStyle.Render(opt.description) + "\n")
	}

	// Discovery results — only when there is something to show.
	if p.discovering {
		b.WriteString("\n    " + lipgloss.NewStyle().Foreground(cPurple).Render("⠸") + " looking up repositories...\n")
	} else if p.discoveryResult != nil && p.discoveryResult.err == nil {
		adopted := 0
		total := len(p.discoveryResult.repos)
		b.WriteString("\n  " + sMuted.Render("Repositories") + "\n")
		for _, r := range p.discoveryResult.repos {
			mark := sGo.Render("✓")
			if !r.adopted {
				mark = sMuted.Render("○")
			} else {
				adopted++
			}
			b.WriteString("    " + mark + " " + sText.Render(r.name))
			if r.errMsg != "" {
				b.WriteString(" " + sMuted.Render("("+r.errMsg+")"))
			}
			b.WriteString("\n")
		}
		_, _ = fmt.Fprintf(&b, "    %s of %d ready for oberth\n",
			sHighlight.Render(fmt.Sprintf("%d", adopted)), total)
	}

	if p.errMsg != "" {
		b.WriteString("\n  " + sFail.Render(p.errMsg) + "\n")
	}

	return b.String()
}
