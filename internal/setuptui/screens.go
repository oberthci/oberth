package setuptui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// ---------- main menu ----------

// menuScreen lists the steps, with ✓ for the finished ones. Any step can be
// reopened from here; a rerun of setup starts here with the saved answers.
type menuScreen struct {
	cursor int
	focus  focusRing
}

func newMenuScreen(w *wizard) *menuScreen {
	m := &menuScreen{focus: focusRing{targets: 1, buttons: 2}}
	m.cursor = int(w.nextStep())
	if w.install.started || w.state.Install.Status != installNone {
		m.cursor = int(stepInstall)
	}
	return m
}

func (m *menuScreen) update(w *wizard, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch key.String() {
	case "esc":
		return w.quit(exitMenu)
	case "enter":
		if m.focus.button() == 0 {
			return w.quit(exitMenu)
		}
		return w.openStep(stepID(m.cursor))
	}
	if m.focus.handle(key.String()) || m.focus.button() >= 0 {
		return nil
	}
	switch key.String() {
	case "up", "k":
		m.cursor = max(0, m.cursor-1)
	case "down", "j":
		m.cursor = min(stepCount-1, m.cursor+1)
	case "home":
		m.cursor = 0
	case "end":
		m.cursor = stepCount - 1
	default:
		if t := key.String(); len(t) == 1 && t[0] >= '1' && t[0] < byte('1'+stepCount) {
			m.cursor = int(t[0] - '1')
		}
	}
	return nil
}

func (m *menuScreen) frame(w *wizard) frameSpec {
	statePath, _ := setupStatePath()
	explain := "Walk the steps in order, or choose any step to change it; ✓ marks a finished step. " +
		"Answers are saved in " + displayHome(statePath) + " (never secrets), so running setup again resumes here."
	if w.opts.DryMode {
		explain += " --dry-mode: the last step prints the equivalent oberth install command; nothing is applied or saved."
	}
	if w.notice != "" {
		explain += "\n\n" + w.notice
	}
	spec := frameSpec{
		title:     "Main menu",
		heading:   "Install Oberth",
		explain:   explain,
		focusLine: -1,
		buttons:   []string{"Exit", "Continue"},
		button:    m.focus.button(),
		keys:      "↑/↓ choose · enter open · tab buttons · esc exit · ctrl+c quit",
	}
	nameWidth := 0
	for _, n := range stepNames {
		nameWidth = max(nameWidth, lipgloss.Width(n))
	}
	inner := max(20, w.width-4)
	for i := 0; i < stepCount; i++ {
		step := stepID(i)
		mark := "  "
		status, statusStyle := stepStatus(w, step)
		if status == "✓" {
			mark = sGo.Render("✓ ")
			status = ""
		}
		name := padTo(fmt.Sprintf("%d. %s", i+1, step.name()), nameWidth+3)
		cursor := "  "
		if i == m.cursor {
			cursor = lipgloss.NewStyle().Foreground(cPurple).Render("❯ ")
			if m.focus.button() < 0 {
				name = sSelected.Render(name)
			} else {
				name = sHighlight.Render(name)
			}
			spec.focusLine = len(spec.body)
		} else {
			name = sText.Render(name)
		}
		line := cursor + mark + name
		summary := stepSummary(&w.state, step)
		if status != "" {
			summary = status
		}
		room := inner - lipgloss.Width(line) - 2
		if summary != "" && room > 8 {
			line += "  " + statusStyle.Render(truncateRunes(summary, room))
		}
		spec.body = append(spec.body, line)
	}
	return spec
}

// stepStatus is "✓" for a finished step, or a short status for the install.
func stepStatus(w *wizard, step stepID) (string, lipgloss.Style) {
	if step == stepInstall {
		switch w.state.Install.Status {
		case installComplete:
			return "✓", sGo
		case installPendingForge:
			return "! installed · waiting for forge authorization — open to verify", sHold
		case installPartial:
			return "! installed · onboarding unfinished — open to verify", sHold
		case installFailed:
			return "✗ failed at " + w.state.Install.Phase, sFail
		}
		if w.opts.DryMode {
			return "prints the equivalent command (--dry-mode)", sMuted
		}
		return "", sMuted
	}
	if w.state.done[step] {
		return "✓", sGo
	}
	return "", sMuted
}

// stepSummary is the one-line answer of a step, for the menu and review.
func stepSummary(st *WizardState, step stepID) string {
	if !st.done[step] {
		return ""
	}
	c := st.Config
	switch step {
	case stepCluster:
		target := c.ContextName
		switch {
		case c.CreateKind:
			target = "new kind cluster oberth (" + st.contextForCommands() + ")"
		case c.KubeconfigPath != "":
			target = c.ContextName + " in " + displayHome(c.KubeconfigPath)
		}
		if st.ClusterInfo.version != "" && !c.CreateKind {
			target += " · " + st.ClusterInfo.engine + " " + st.ClusterInfo.version
		}
		np := map[string]string{"": "auto", "auto": "auto", "true": "strict", "false": "off"}[c.NetworkPolicy]
		return fmt.Sprintf("%s · namespaces %s/%s/%s · egress policy %s", target, c.Namespace, c.ArgoNamespace, c.OpenBaoNamespace, np)
	case stepStore:
		switch st.StoreMode {
		case storeInstallDev:
			return "OpenBao dev (in memory)"
		case storeConnect:
			return "existing store at " + st.StoreAddress
		}
		if c.InstallRekor {
			return "OpenBao production · Rekor witness"
		}
		return "OpenBao production"
	case stepTLS:
		dnsNames, ips := serverCertificateNames(st)
		extra := append(append([]string{}, c.TLSExtraDNSNames...), c.TLSExtraIPs...)
		s := fmt.Sprintf("%d names", len(dnsNames)+len(ips))
		if len(extra) > 0 {
			s += " incl. " + strings.Join(extra, ", ")
		}
		return s + " · clients use " + clientHost(st)
	case stepForge:
		key := "generated upstream key"
		if st.ForgeAuth == forgeAuthExisting {
			key = "existing key " + displayHome(c.UpstreamPrivateKeyPath)
		}
		summary := st.ForgeType + "/" + st.ForgeOrg + " · " + key
		for _, u := range c.AlsoUpstreams {
			summary += " · also " + displayUpstream(u)
		}
		return summary
	case stepUplink:
		return st.UplinkIdentity + " · " + displayHome(st.SSHKeyPath)
	case stepClient:
		return map[string]string{"both": "CLI and MCP", "cli": "CLI only", "mcp": "MCP only", "none": "not configured"}[st.ClientAccess]
	case stepReview:
		return "reviewed"
	}
	return ""
}

// ---------- review ----------

// preflightDoneMsg reports the review's cluster re-check. It names the
// cluster it checked: a late result for a cluster the operator has since
// changed must not mark the review checked (review finding).
type preflightDoneMsg struct {
	context, kubeconfig string
	result              probeResult
}

// reviewScreen shows every answer and the equivalent command, and re-checks
// the answers before the install can start: resumed answers may be stale.
type reviewScreen struct {
	focus    focusRing
	failing  *question
	errMsg   string
	checking bool
	checked  bool
}

func newReviewScreen() *reviewScreen {
	r := &reviewScreen{focus: focusRing{targets: 0, buttons: 2}}
	r.focus.focusButton(buttonContinue)
	return r
}

func (r *reviewScreen) start(w *wizard) tea.Cmd {
	r.failing, r.errMsg, r.checked = nil, "", false
	if q, err := revalidate(w.questions, &w.state); err != nil {
		r.failing = q
		r.errMsg = fmt.Sprintf("%s: %v — Continue opens that screen.", q.step.name(), err)
		return nil
	}
	if w.state.Config.CreateKind {
		r.checked = true
		return nil
	}
	r.checking = true
	context, kubeconfig := w.state.Config.ContextName, w.state.Config.KubeconfigPath
	work := probeContext(context, kubeconfig)
	return func() tea.Msg { return preflightDoneMsg{context: context, kubeconfig: kubeconfig, result: work()} }
}

func (r *reviewScreen) update(w *wizard, msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case preflightDoneMsg:
		if msg.context != w.state.Config.ContextName || msg.kubeconfig != w.state.Config.KubeconfigPath {
			return nil // a stale check for a cluster no longer selected
		}
		r.checking = false
		if msg.result.err != nil {
			r.failing = questionsOf(w.questions, stepCluster)[0]
			r.errMsg = "Cluster: " + msg.result.err.Error() + " — Continue opens the cluster step."
			return nil
		}
		msg.result.apply(&w.state)
		r.checked = true
		return nil
	case tea.KeyPressMsg:
		key := msg.String()
		switch key {
		case "esc":
			w.toMenu(stepReview)
			return nil
		case "enter":
			if r.focus.button() == buttonBack {
				w.toMenu(stepReview)
				return nil
			}
			return r.proceed(w)
		case "up", "k":
			w.scroll--
			return nil
		case "down", "j":
			w.scroll++
			return nil
		case "pgup":
			w.scroll -= max(1, w.height-8)
			return nil
		case "pgdown":
			w.scroll += max(1, w.height-8)
			return nil
		}
		r.focus.handle(key)
	}
	return nil
}

func (r *reviewScreen) proceed(w *wizard) tea.Cmd {
	if r.failing != nil {
		w.openQuestion(r.failing)
		return nil
	}
	if r.checking || !r.checked {
		return nil
	}
	w.state.done[stepReview] = true
	if w.opts.DryMode {
		return w.quit(exitDry)
	}
	w.persist()
	w.setScreen(w.install)
	return w.install.start(w)
}

func (r *reviewScreen) frame(w *wizard) frameSpec {
	inner := max(20, w.width-4)
	explain := "Check the plan. Continue starts the install; every answer was checked on its own screen and is checked again now."
	if w.opts.DryMode {
		explain = "Check the plan. Continue prints the equivalent oberth install command and exits — --dry-mode applies nothing."
	}
	spec := frameSpec{
		title:     stepReview.title(),
		heading:   "Review the plan",
		explain:   explain,
		focusLine: -1,
		errMsg:    r.errMsg,
		buttons:   []string{"Go Back", "Continue"},
		button:    r.focus.button(),
		disabled:  map[int]bool{buttonContinue: r.checking},
		keys:      "↑/↓ pgup/pgdn scroll · ←/→ buttons · enter select · esc menu · ctrl+c quit",
	}
	for i := stepCluster; i < stepReview; i++ {
		head := fmt.Sprintf("%d. %s", int(i)+1, i.name())
		spec.body = append(spec.body, sHighlight.Render(head))
		for _, l := range wrapText(stepSummary(&w.state, i), inner-3) {
			spec.body = append(spec.body, "   "+sText.Render(l))
		}
	}
	spec.body = append(spec.body, "", sMuted.Render("Equivalent command:"))
	for _, line := range strings.Split(BuildCommandLine(&w.state), "\n") {
		spec.body = append(spec.body, wrapCommand(strings.TrimSpace(line), inner-2, sInfo)...)
	}
	switch {
	case r.checking:
		spec.status = "Checking the cluster connection…"
	case r.checked && r.errMsg == "":
		spec.status = "All answers checked."
	}
	return spec
}
