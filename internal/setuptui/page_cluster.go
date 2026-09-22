package setuptui

import (
	"fmt"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"k8s.io/client-go/tools/clientcmd"
)

const kindCreateEntry = "Create kind cluster"

// kubeContext represents one entry from kubeconfig.
type kubeContext struct {
	name    string
	server  string
	isLocal bool
	version string
}

type clusterPage struct {
	contexts       []kubeContext
	cursor         int
	currentContext string // the kubeconfig current-context at load time
	checking       bool
	errMsg         string
}

func newClusterPage() *clusterPage {
	return &clusterPage{}
}

func (p *clusterPage) title() string    { return "launch site" }
func (p *clusterPage) question() string { return "Where will oberth run?" }
func (p *clusterPage) keys() string {
	return sKey.Render("↑/↓") + " move · " + sKey.Render("enter") + " select · " + sKey.Render("esc") + " back"
}

func (p *clusterPage) init(state *WizardState) tea.Cmd {
	p.loadContexts()
	// Pre-select the context that matches ClusterInfo.
	for i, ctx := range p.contexts {
		if ctx.name == state.SelectedContext {
			p.cursor = i
			break
		}
	}
	return nil
}

func (p *clusterPage) loadContexts() {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	rawConfig, err := config.RawConfig()
	if err != nil {
		p.contexts = nil
		return
	}

	p.currentContext = rawConfig.CurrentContext
	p.contexts = make([]kubeContext, 0, len(rawConfig.Contexts))
	for name, ctx := range rawConfig.Contexts {
		cluster, ok := rawConfig.Clusters[ctx.Cluster]
		server := ""
		if ok {
			server = cluster.Server
		}
		isLocal := isLocalServer(server)
		p.contexts = append(p.contexts, kubeContext{
			name:    name,
			server:  server,
			isLocal: isLocal,
		})
	}

	// Sort: current context first, then by name.
	current := rawConfig.CurrentContext
	sortContexts(p.contexts, current)

	// On macOS with no existing contexts, offer to create a kind cluster.
	// The installer's Execute already handles kind creation on darwin;
	// the TUI just needs to let the user through.
	if len(p.contexts) == 0 && runtime.GOOS == "darwin" {
		p.contexts = append(p.contexts, kubeContext{
			name:    kindCreateEntry,
			isLocal: true,
		})
	}
}

func (p *clusterPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case clusterInfoMsg:
		p.checking = false
		if msg.err != nil {
			p.errMsg = msg.err.Error()
			return p, nil
		}
		state.ClusterInfo = msg
		state.SelectedContext = msg.context
		return p, func() tea.Msg { return pageCompleteMsg{} }

	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			if p.cursor > 0 {
				p.cursor--
			}
			p.errMsg = ""
		case "down", "j":
			if p.cursor < len(p.contexts)-1 {
				p.cursor++
			}
			p.errMsg = ""
		case "enter":
			if len(p.contexts) == 0 {
				p.errMsg = "no kubeconfig contexts found"
				return p, nil
			}
			selected := p.contexts[p.cursor].name
			if selected == kindCreateEntry {
				p.checking = false
				p.errMsg = ""
				return p, func() tea.Msg {
					return clusterInfoMsg{
						context: "",
						engine:  "kind",
						isLocal: true,
					}
				}
			}
			// Guard: the installer targets the current kubeconfig context
			// (no --context flag). Selecting a non-current context would
			// apply to the wrong cluster silently.
			if p.currentContext != "" && selected != p.currentContext {
				p.errMsg = fmt.Sprintf("installing into a non-current context is not supported yet — run: kubectl config use-context %s", selected)
				return p, nil
			}
			p.checking = true
			p.errMsg = ""
			return p, probeCluster(selected)
		case "esc":
			return p, func() tea.Msg { return pageBackMsg{} }
		}
	}
	return p, nil
}

func (p *clusterPage) view(_ *WizardState, width, _ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n")
	b.WriteString("\n")

	if len(p.contexts) == 0 {
		b.WriteString("  " + sFail.Render("no kubeconfig contexts found") + "\n")
		b.WriteString("  " + sMuted.Render("install Docker Desktop and kind, then relaunch") + "\n")
		return b.String()
	}

	// Build the bordered list. Names are padded to one column width before
	// styling, so local/remote lines up whatever the context names are.
	nameWidth := 0
	for _, ctx := range p.contexts {
		nameWidth = max(nameWidth, lipgloss.Width(ctx.name))
	}
	nameWidth = min(nameWidth, 40)

	var listContent strings.Builder
	for i, ctx := range p.contexts {
		cursor := "  "
		nameStyle := sText
		if i == p.cursor {
			cursor = lipgloss.NewStyle().Foreground(cPurple).Render(" ❯")
			nameStyle = lipgloss.NewStyle().Foreground(cFg).Bold(true)
		}

		locality := sInfo.Render("local")
		if !ctx.isLocal {
			locality = sHold.Render("remote")
		}

		line := fmt.Sprintf("%s %s %s", cursor, nameStyle.Render(padTo(truncateRunes(ctx.name, nameWidth), nameWidth)), locality)
		if ctx.version != "" {
			line += "    " + sMuted.Render(ctx.version)
		}
		listContent.WriteString(line + "\n")
	}

	noun := "contexts"
	if len(p.contexts) == 1 {
		noun = "context"
	}
	tally := sMuted.Render(fmt.Sprintf("  %d %s", len(p.contexts), noun))
	listContent.WriteString("\n" + tally)

	boxWidth := min(60, width-20)
	if boxWidth < 30 {
		boxWidth = 30
	}
	box := sListBox.Width(boxWidth).Render(listContent.String())

	// Center the box.
	pad := (width - lipgloss.Width(box)) / 2
	if pad < 0 {
		pad = 0
	}
	for _, line := range strings.Split(box, "\n") {
		b.WriteString(strings.Repeat(" ", pad) + line + "\n")
	}

	if p.checking {
		b.WriteString("\n  " + lipgloss.NewStyle().Foreground(cPurple).Render("⠸") + " checking cluster...\n")
	}

	if p.errMsg != "" {
		b.WriteString("\n  " + sFail.Render(p.errMsg) + "\n")
	}

	return b.String()
}

// isLocalServer heuristically determines if a server URL is local.
func isLocalServer(server string) bool {
	for _, prefix := range []string{
		"https://127.", "https://localhost", "https://192.168.",
		"https://10.", "https://172.16.", "https://172.17.",
		"https://172.18.", "https://172.19.", "https://172.2",
		"https://172.3",
	} {
		if strings.HasPrefix(server, prefix) {
			return true
		}
	}
	return false
}

// sortContexts sorts contexts with the current context first, then
// alphabetically.
func sortContexts(contexts []kubeContext, current string) {
	// Simple insertion sort — number of contexts is always small.
	for i := 1; i < len(contexts); i++ {
		j := i
		for j > 0 {
			// Current context always first.
			if contexts[j].name == current && contexts[j-1].name != current {
				contexts[j], contexts[j-1] = contexts[j-1], contexts[j]
				j--
				continue
			}
			if contexts[j-1].name == current {
				break
			}
			if contexts[j].name < contexts[j-1].name {
				contexts[j], contexts[j-1] = contexts[j-1], contexts[j]
				j--
				continue
			}
			break
		}
	}
}
