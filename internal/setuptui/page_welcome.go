package setuptui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The ASCII wordmark, rendered in brand Purple.
const wordmark = `
 ██████╗ ██████╗ ███████╗██████╗ ████████╗██╗  ██╗
██╔═══██╗██╔══██╗██╔════╝██╔══██╗╚══██╔══╝██║  ██║
██║   ██║██████╔╝█████╗  ██████╔╝   ██║   ███████║
██║   ██║██╔══██╗██╔══╝  ██╔══██╗   ██║   ██╔══██║
╚██████╔╝██████╔╝███████╗██║  ██║   ██║   ██║  ██║
 ╚═════╝ ╚═════╝ ╚══════╝╚═╝  ╚═╝   ╚═╝   ╚═╝  ╚═╝`

type welcomePage struct {
	detectedLine string
}

func newWelcomePage() *welcomePage {
	return &welcomePage{}
}

func (p *welcomePage) title() string    { return "mission briefing" }
func (p *welcomePage) question() string { return "" }
func (p *welcomePage) keys() string {
	return sKey.Render("enter") + " begin · " + sKey.Render("q") + " quit"
}

func (p *welcomePage) init(state *WizardState) tea.Cmd {
	// Build detected environment line from cluster info if available.
	if state.ClusterInfo.version != "" {
		p.detectedLine = fmt.Sprintf("%s %s · %s · %d node ready · %d cores",
			state.ClusterInfo.engine,
			state.ClusterInfo.version,
			state.ClusterInfo.context,
			state.ClusterInfo.nodeCount,
			state.ClusterInfo.cores,
		)
	}
	return probeCluster("")
}

func (p *welcomePage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case clusterInfoMsg:
		state.ClusterInfo = msg
		if msg.err == nil {
			p.detectedLine = fmt.Sprintf("%s %s · %s · %d node ready · %d cores",
				msg.engine, msg.version, msg.context, msg.nodeCount, msg.cores)
			state.SelectedContext = msg.context
		} else {
			p.detectedLine = sMuted.Render("cluster detection: ") + sFail.Render(msg.err.Error())
		}
		return p, nil

	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "q", "esc":
			return p, tea.Quit
		}
		// No other letter does anything here. Accessible and plain modes
		// are startup flags (--accessible, --plain), never a key press: one
		// stray letter must not change the whole interaction model.
	}
	return p, nil
}

func (p *welcomePage) view(_ *WizardState, width, height int) string {
	var b strings.Builder

	// Center the wordmark vertically and horizontally.
	wordmarkStyled := lipgloss.NewStyle().Foreground(cPurple).Render(wordmark)
	tagline := sText.Render("Machine-speed code. ") +
		lipgloss.NewStyle().Foreground(cPurple).Render("Human-grade control.")

	detected := ""
	if p.detectedLine != "" {
		detected = sMuted.Render(p.detectedLine)
	}

	prompt := sMuted.Render("press ") + sKey.Render("enter") + sMuted.Render(" to begin setup")
	hints := sKey.Render("q") + sMuted.Render(" quit · ") + sKey.Render("?") + sMuted.Render(" help")

	content := lipgloss.JoinVertical(lipgloss.Center,
		"",
		wordmarkStyled,
		"",
		tagline,
		"",
		"",
		detected,
		"",
		prompt,
		"",
		hints,
	)

	// Center content in the available space.
	centeredContent := lipgloss.NewStyle().
		Width(width).
		Height(height).
		AlignHorizontal(lipgloss.Center).
		AlignVertical(lipgloss.Center).
		Render(content)

	b.WriteString(centeredContent)
	return b.String()
}
