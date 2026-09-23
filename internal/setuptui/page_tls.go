package setuptui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type tlsPage struct {
	sans   []string
	errMsg string
}

func newTLSPage() *tlsPage {
	return &tlsPage{}
}

func (p *tlsPage) title() string    { return "heat shield" }
func (p *tlsPage) question() string { return "How should oberth serve TLS?" }
func (p *tlsPage) keys() string {
	return sKey.Render("↑/↓") + " choose · " + sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *tlsPage) init(state *WizardState) tea.Cmd {
	// Self-signed is the only implemented mode; BYO has no installer
	// backing (no --tls-cert/--tls-key flags exist).
	state.TLSMode = "self-signed"

	// Pre-fill SANs from cluster info: service DNS, node name, node IP.
	// The context name ("default", "k3s-tuxbox") is not a useful SAN —
	// the node name and IP are what clients actually connect to.
	p.sans = []string{}
	ns := state.Config.Namespace
	if ns == "" {
		ns = "oberth"
	}
	p.sans = append(p.sans, "oberth."+ns+".svc")
	if state.ClusterInfo.nodeName != "" {
		p.sans = append(p.sans, state.ClusterInfo.nodeName)
	}
	// The node IP is a SAN too. init runs again on every revisit (esc back,
	// review jump), so only add it once — a duplicate would show up as an
	// extra --tls-extra-ip in the dry-mode command and in the review count.
	if ip := state.ClusterInfo.nodeIP; ip != "" && !slices.Contains(state.Config.TLSExtraIPs, ip) {
		state.Config.TLSExtraIPs = append(state.Config.TLSExtraIPs, ip)
	}

	p.errMsg = ""
	return nil
}

func (p *tlsPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			state.TLSMode = "self-signed"
			state.Config.TLSExtraDNSNames = p.sans
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		}
	}
	return p, nil
}

func (p *tlsPage) view(state *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	// Every name and address the certificate will be valid for.
	validFor := append(slices.Clone(p.sans), state.Config.TLSExtraIPs...)

	b.WriteString("    " + lipgloss.NewStyle().Foreground(cPurple).Render("(*)") +
		" " + sText.Render("generate a self-signed certificate (ed25519)") + "\n")
	b.WriteString("          " + sMuted.Render("valid for: "+strings.Join(validFor, " · ")) + "\n\n")

	// Fingerprint note — the done page shows the retrieval commands.
	b.WriteString("  " + sMuted.Render("after install, retrieve the fingerprint and verify it out of band") + "\n")
	b.WriteString("  " + sMuted.Render("bring-your-own certificate support is coming soon") + "\n")

	if p.errMsg != "" {
		b.WriteString("\n  " + sFail.Render(p.errMsg) + "\n")
	}

	return b.String()
}
