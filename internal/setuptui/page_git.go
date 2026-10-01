package setuptui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// gitPage is informational: how code reaches oberth and what happens next.
// It shows the essentials a first-time user needs — how to clone, what a
// push does, the green/red contract. Queue mechanics, run IDs, and the
// exact stderr the server answers with belong in the docs, not here.
type gitPage struct{}

func newGitPage() *gitPage {
	return &gitPage{}
}

func (p *gitPage) title() string    { return "comms check" }
func (p *gitPage) question() string { return "How code arrives." }
func (p *gitPage) keys() string {
	return sKey.Render("enter") + " continue · " + sKey.Render("esc") + " back"
}

func (p *gitPage) init(_ *WizardState) tea.Cmd { return nil }

func (p *gitPage) update(msg tea.Msg, _ *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		}
	}
	return p, nil
}

// gitCloneURL renders one ssh clone URL with the <repo> placeholder muted.
func gitCloneURL(host string) string {
	return sInfo.Render("ssh://git@"+host+":30022/") + sMuted.Render("<repo>") + sInfo.Render(".git")
}

func (p *gitPage) view(state *WizardState, _, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	identity := ""
	if state.UplinkIdentity != "" {
		identity = " (" + state.UplinkIdentity + ")"
	}
	b.WriteString("  " + sText.Render("Push to Oberth over SSH") +
		sMuted.Render(" — every push is linked to your identity"+identity+".") + "\n\n")

	nodeHost := "<node-ip>"
	if state.ClusterInfo.nodeIP != "" {
		nodeHost = state.ClusterInfo.nodeIP
	}
	const urlColumn = 46
	b.WriteString("  " + sMuted.Render("Clone URLs") + "\n")
	b.WriteString("    " + padTo(gitCloneURL("localhost"), urlColumn) + sMuted.Render("(from this machine)") + "\n")
	b.WriteString("    " + padTo(gitCloneURL(nodeHost), urlColumn) + sMuted.Render("(from your network)") + "\n\n")

	b.WriteString("  " + sMuted.Render("What happens on push") + "\n")
	b.WriteString("    " + sText.Render("push") + sMuted.Render(" → ") +
		sText.Render("CI runs") + sMuted.Render(" → ") +
		sGo.Render("green") + sMuted.Render(" publishes upstream · ") +
		sFail.Render("red") + sMuted.Render(" opens an issue") + "\n\n")

	b.WriteString("  " + sMuted.Render("Tags are immutable — only ") + sGo.Render("green") +
		sMuted.Render(" branches reach the upstream forge.") + "\n")

	return b.String()
}
