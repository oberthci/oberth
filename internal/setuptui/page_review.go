package setuptui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type reviewPage struct {
	showDryRun bool
	dryRunText string
}

func newReviewPage() *reviewPage {
	return &reviewPage{}
}

func (p *reviewPage) title() string    { return "go/no-go" }
func (p *reviewPage) question() string { return "Review the plan." }
func (p *reviewPage) keys() string {
	return sKey.Render(fmt.Sprintf("1..%d", len(reviewSectionPages))) + " revisit · " + sKey.Render("d") + " preview command · " + sKey.Render("enter") + " apply · " + sKey.Render("esc") + " back"
}

func (p *reviewPage) init(_ *WizardState) tea.Cmd {
	p.showDryRun = false
	return nil
}

func (p *reviewPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "enter":
			if !p.allGo(state) {
				return p, nil
			}
			return p, func() tea.Msg { return pageCompleteMsg{} }
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		case "d":
			p.showDryRun = !p.showDryRun
			if p.showDryRun {
				p.dryRunText = BuildCommandLine(state)
			}
			return p, nil
		// Jump keys 1..7 → corresponding pages, via the shared section map
		// (the apply page's HOLD state uses the same table).
		case "1", "2", "3", "4", "5", "6", "7":
			section := int(msg.String()[0] - '0')
			if target, ok := reviewSectionPages[section]; ok {
				return p, func() tea.Msg { return pageJumpMsg{page: target} }
			}
		}
	}
	return p, nil
}

func (p *reviewPage) allGo(state *WizardState) bool {
	rows := p.buildRows(state)
	for _, row := range rows {
		if row.status == "HOLD" {
			return false
		}
	}
	return true
}

type reviewRow struct {
	num     int
	label   string
	summary string
	status  string // GO, HOLD
	note    string
}

func (p *reviewPage) buildRows(state *WizardState) []reviewRow {
	// Section numbers are the keys of reviewSectionPages; validity comes
	// from the page each section revisits.
	rows := []reviewRow{
		{1, "cluster", formatClusterSummary(state), goStatus(state.pageValid[pageCluster]), ""},
		{2, "namespaces", formatNamespacesSummary(state), goStatus(state.pageValid[pageNamespaces]), ""},
		{3, "network", formatNetworkSummary(state), goStatus(state.pageValid[pageExecution]), ""},
		{4, "store", formatStoreSummary(state), goStatus(state.pageValid[pageStore] || state.pageValid[pageStoreConnect]), ""},
		{5, "tls", formatTLSSummary(state), goStatus(state.pageValid[pageTLS]), ""},
		{6, "uplink", formatUplinkSummary(state), goStatus(state.pageValid[pageUplink]), ""},
		{7, "forge", formatForgeSummary(state), goStatus(state.pageValid[pageForge]), ""},
	}
	return rows
}

func goStatus(valid bool) string {
	if valid {
		return "GO"
	}
	return "HOLD"
}

func (p *reviewPage) view(state *WizardState, width, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	if p.showDryRun {
		b.WriteString("  " + sInfo.Render(p.dryRunText) + "\n\n")
		b.WriteString("  " + sKey.Render("d") + sMuted.Render(" toggle preview") + "\n")
		return b.String()
	}

	rows := p.buildRows(state)

	// One summary column: every status lands at the same visible column so
	// the eight rows scan as a table, not as eight sentences. The column is
	// as wide as the longest summary, capped so a long store address cannot
	// push GO/HOLD off the screen.
	summaryWidth := 0
	for _, row := range rows {
		summaryWidth = max(summaryWidth, lipgloss.Width(row.summary))
	}
	summaryWidth = min(summaryWidth, max(20, width-30))

	for _, row := range rows {
		numStr := sKey.Render(fmt.Sprintf("  %d", row.num))
		labelStr := lipgloss.NewStyle().Foreground(cPurple).Render(fmt.Sprintf("  %-12s", row.label))
		summaryStr := padTo(sText.Render(truncateRunes(row.summary, summaryWidth)), summaryWidth)

		statusStyle := sGo
		if row.status == "HOLD" {
			statusStyle = sHold
		}
		statusStr := statusStyle.Render(fmt.Sprintf("%4s", row.status))

		b.WriteString(numStr + labelStr + summaryStr + "  " + statusStr + "\n")

		if row.note != "" {
			b.WriteString("     " + sHold.Render("! "+row.note) + "\n")
		}
	}

	b.WriteString("\n")

	// Secrets guarantee line.
	b.WriteString("  " + sMuted.Render("no secret leaves memory: ") +
		sGo.Render("none") + sMuted.Render(" on disk · ") +
		sGo.Render("none") + sMuted.Render(" in etcd · token shown once") + "\n\n")

	// Apply button.
	allGo := p.allGo(state)
	if allGo {
		centered := lipgloss.NewStyle().Width(80).AlignHorizontal(lipgloss.Center).
			Render(sButton.Render("▶ apply the plan — enter"))
		b.WriteString(centered + "\n")
	} else {
		centered := lipgloss.NewStyle().Width(80).AlignHorizontal(lipgloss.Center).
			Render(sButtonDim.Render("▶ apply the plan — enter"))
		b.WriteString(centered + "\n")
	}

	return b.String()
}

// Helper formatters for the review summary column.

func formatClusterSummary(state *WizardState) string {
	if state.ClusterInfo.context == "" {
		return "not selected"
	}
	parts := []string{state.ClusterInfo.context}
	if state.ClusterInfo.version != "" {
		parts = append(parts, state.ClusterInfo.version)
	}
	if state.ClusterInfo.isLocal {
		parts = append(parts, "local")
	} else {
		parts = append(parts, "remote")
	}
	return strings.Join(parts, " · ")
}

func formatNamespacesSummary(state *WizardState) string {
	ns := state.Config.Namespace
	if ns == "" {
		ns = "oberth"
	}
	argo := state.Config.ArgoNamespace
	if argo == "" {
		argo = "oberth-argo" // must match installer.DefaultArgoNamespace
	}
	openbao := state.Config.OpenBaoNamespace
	if openbao == "" {
		openbao = "openbao"
	}
	return ns + " / " + argo + " / " + openbao
}

func formatNetworkSummary(state *WizardState) string {
	// Config holds the installer vocabulary (auto|true|false); show the
	// human-facing label the wizard collected instead of the raw value.
	np := state.Config.NetworkPolicy
	switch np {
	case "", "auto":
		np = "auto"
	case "true":
		np = "strict"
	case "false":
		np = "off"
	}
	anchoring := "off"
	if state.Config.InstallRekor {
		anchoring = "on"
	}
	return "policy " + np + " · anchoring " + anchoring
}

func formatStoreSummary(state *WizardState) string {
	switch state.StoreMode {
	case "install-dev":
		return "install openbao — dev"
	case "install-prod":
		return "install openbao — production"
	case "connect":
		if state.StoreAddress != "" {
			return "connect existing · " + state.StoreAddress
		}
		return "connect existing"
	}
	return "undecided"
}

func formatTLSSummary(state *WizardState) string {
	mode := state.TLSMode
	if mode == "" {
		mode = "self-signed"
	}
	names := len(state.Config.TLSExtraDNSNames) + len(state.Config.TLSExtraIPs)
	plural := "names"
	if names == 1 {
		plural = "name"
	}
	return mode + fmt.Sprintf(" · valid for %d %s", names, plural)
}

func formatUplinkSummary(state *WizardState) string {
	if state.UplinkIdentity == "" {
		return "not configured"
	}
	return state.UplinkIdentity
}

func formatForgeSummary(state *WizardState) string {
	if state.ForgeOrg == "" {
		return "not configured"
	}
	auth := strings.ReplaceAll(state.ForgeAuth, "-", " ")
	return state.ForgeType + " / " + state.ForgeOrg + " · " + auth
}
