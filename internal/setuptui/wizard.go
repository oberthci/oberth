package setuptui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/oberthci/oberth/internal/installer"
)

// Options holds the CLI-level options for the setup wizard.
type Options struct {
	ChartPath  string
	ImageRef   string
	DryMode    bool // wizard only, print the command, no apply
	Plain      bool // no TUI, sequential prompts
	Accessible bool // screen reader mode
	Fresh      bool // ignore the answers saved by a previous run
	// BinaryVersion is the oberth binary's version, threaded from main so
	// an apply resolves the same chart version `oberth install` would.
	BinaryVersion string
}

// checkHostTools refuses to start without helm and kubectl; tests replace it
// so they do not depend on the host's tools.
var checkHostTools = installer.CheckHostTools

// executeInstaller runs the install; tests replace it.
var executeInstaller = installer.Execute

// Run is the entry point of `oberth setup`. The interaction model is a
// startup decision: --plain / --accessible (or a non-interactive terminal)
// select the sequential prompts, which ask the same questions.
func Run(ctx context.Context, opts Options, input io.Reader, output io.Writer) error {
	// Refuse before the first screen: every install needs helm and kubectl,
	// and finding out at the Helm step wastes the whole walkthrough (#030).
	if err := checkHostTools(runtime.GOOS, nil); err != nil {
		return err
	}
	st := newWizardState(opts)
	var resumed bool
	var loadErr error
	if !opts.Fresh {
		resumed, loadErr = loadSetupState(st)
	}
	if opts.Plain || opts.Accessible {
		return runPlain(ctx, opts, st, resumed, loadErr, input, output)
	}

	w := newWizard(opts, st)
	if loadErr != nil {
		w.notice = "Previous answers were not loaded: " + loadErr.Error()
	} else if resumed {
		w.notice = "Resumed the answers saved by your previous run."
	}
	p := tea.NewProgram(w, tea.WithContext(ctx), tea.WithOutput(output))
	finalModel, err := p.Run()

	// Defensive teardown regardless of how the program ended: no exit
	// route may leave secret bytes live on the heap (S2/S10).
	if wiz, ok := finalModel.(*wizard); ok {
		wiz.teardownSecrets()
	}
	w.teardownSecrets()

	if err != nil {
		if runtimeInterrupted(err) {
			return installer.ErrInterrupted
		}
		return fmt.Errorf("setup wizard: %w", err)
	}
	return w.finish(output)
}

// runtimeInterrupted reports whether the TUI program ended because a signal
// reached it rather than because of a wizard failure.
func runtimeInterrupted(err error) bool {
	return errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled)
}

// screen is one full-frame view of the wizard.
type screen interface {
	frame(w *wizard) frameSpec
	update(w *wizard, msg tea.Msg) tea.Cmd
}

type exitReason int

const (
	exitMenu exitReason = iota
	exitAbort
	exitDry
	exitAfterFailure
)

// wizard is the root tea.Model: a main menu over the steps, one screen at a
// time, every screen in the same frame.
type wizard struct {
	opts      Options
	state     WizardState
	questions []*question
	screen    screen
	install   *installScreen
	width     int
	height    int
	scroll    int
	logScroll int
	showLog   bool

	confirmQuit bool
	exit        exitReason
	notice      string // shown on the menu: resume / state-file problems
	lastVerify  *verifyResult
}

func newWizard(opts Options, st *WizardState) *wizard {
	w := &wizard{opts: opts, state: *st, questions: buildQuestions()}
	w.install = newInstallScreen()
	w.screen = newMenuScreen(w)
	return w
}

func (w *wizard) Init() tea.Cmd { return tea.RequestWindowSize }

func (w *wizard) setScreen(s screen) {
	w.screen = s
	w.scroll = 0
}

// persist saves the answers, except in --dry-mode, which changes nothing.
func (w *wizard) persist() {
	if w.opts.DryMode {
		return
	}
	if err := saveSetupState(&w.state); err != nil {
		w.notice = "Answers could not be saved: " + err.Error()
	}
}

// openStep shows a step's first screen.
func (w *wizard) openStep(step stepID) tea.Cmd {
	switch step {
	case stepReview:
		r := newReviewScreen()
		w.setScreen(r)
		return r.start(w)
	case stepInstall:
		if w.install.started || w.state.Install.Status != installNone {
			f := newFinishScreen(w)
			w.setScreen(f)
			return f.start(w)
		}
		r := newReviewScreen()
		w.setScreen(r)
		return r.start(w)
	}
	for _, q := range questionsOf(w.questions, step) {
		if q.appliesTo(&w.state) {
			w.setScreen(newQuestionScreen(q, &w.state))
			return nil
		}
	}
	w.state.done[step] = true
	return w.openStep(w.nextStep())
}

// nextStep is the first step not done yet, else the review.
func (w *wizard) nextStep() stepID {
	for i := stepCluster; i < stepReview; i++ {
		if !w.state.done[i] {
			return i
		}
	}
	return stepReview
}

// nextQuestion moves past a saved question: the next one in its step, or —
// at the end of the step — the next step that is not done.
func (w *wizard) nextQuestion(q *question) tea.Cmd {
	qs := questionsOf(w.questions, q.step)
	seen := false
	for _, next := range qs {
		if seen && next.appliesTo(&w.state) {
			w.setScreen(newQuestionScreen(next, &w.state))
			return nil
		}
		if next == q {
			seen = true
		}
	}
	w.state.done[q.step] = true
	w.persist()
	return w.openStep(w.nextStep())
}

// prevQuestion is <Go Back>: the previous question of the step, or the menu.
func (w *wizard) prevQuestion(q *question) tea.Cmd {
	qs := questionsOf(w.questions, q.step)
	var prev *question
	for _, p := range qs {
		if p == q {
			break
		}
		if p.appliesTo(&w.state) {
			prev = p
		}
	}
	if prev != nil {
		w.setScreen(newQuestionScreen(prev, &w.state))
		return nil
	}
	w.toMenu(q.step)
	return nil
}

func (w *wizard) toMenu(cursor stepID) {
	m := newMenuScreen(w)
	m.cursor = int(cursor)
	w.setScreen(m)
}

// openQuestion shows a specific question (review and failure jump here).
func (w *wizard) openQuestion(q *question) {
	w.setScreen(newQuestionScreen(q, &w.state))
}

func (w *wizard) quit(reason exitReason) tea.Cmd {
	w.exit = reason
	return tea.Quit
}

func (w *wizard) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		w.width, w.height = msg.Width, msg.Height
		return w, nil

	case tea.KeyPressMsg:
		key := msg.String()
		if key != "ctrl+c" {
			w.confirmQuit = false
		}
		if key == "ctrl+c" {
			// S10: abort is confirmed, never instant.
			if !w.confirmQuit {
				w.confirmQuit = true
				return w, nil
			}
			w.teardownSecrets()
			return w, w.quit(exitAbort)
		}
		if w.showLog {
			w.updateLog(key)
			return w, nil
		}

	// Install progress belongs to the install screen whatever is showing.
	case applyStepMsg, applyLogMsg, applyDoneMsg, ceremonyTokenMsg:
		return w, w.install.consume(w, msg)
	}
	return w, w.screen.update(w, msg)
}

// teardownSecrets zeros every secret still held. Idempotent (S2/S10).
func (w *wizard) teardownSecrets() {
	if w.install == nil {
		return
	}
	if w.install.cancel != nil {
		w.install.cancel()
	}
	w.install.ceremony.zeroAll()
	w.install.masker.wipe()
}

func (w *wizard) View() tea.View {
	v := tea.NewView(w.render())
	v.AltScreen = true
	return v
}

func (w *wizard) render() string {
	if w.width == 0 || w.height == 0 {
		return ""
	}
	footer := ""
	if w.confirmQuit {
		footer = "ctrl+c again to quit · any other key continues"
	}
	if w.showLog {
		return renderFrame(w.width, w.height, w.logFrame(), &w.logScroll, footer)
	}
	return renderFrame(w.width, w.height, w.screen.frame(w), &w.scroll, footer)
}

// ---------- the log view (#014) ----------

func (w *wizard) openLog() {
	w.showLog = true
	w.logScroll = 1 << 30 // start at the end; renderFrame clamps
}

func (w *wizard) updateLog(key string) {
	page := max(1, w.height-8)
	switch key {
	case "esc", "enter", "l", "q":
		w.showLog = false
	case "up", "k":
		w.logScroll--
	case "down", "j":
		w.logScroll++
	case "pgup":
		w.logScroll -= page
	case "pgdown", "space":
		w.logScroll += page
	case "home", "g":
		w.logScroll = 0
	case "end", "G":
		w.logScroll = 1 << 30
	}
}

func (w *wizard) logFrame() frameSpec {
	inner := max(20, w.width-4)
	var body []string
	for _, line := range w.install.logLines {
		for _, l := range wrapText(line, inner) {
			body = append(body, sMuted.Render(l))
		}
	}
	if len(body) == 0 {
		body = []string{sMuted.Render("(nothing logged yet)")}
	}
	return frameSpec{
		title:     stepInstall.title() + " — log",
		heading:   "Install log",
		explain:   "Everything the installer printed, secrets masked. Earlier attempts are kept above the latest one.",
		body:      body,
		focusLine: -1,
		buttons:   []string{"Close"},
		button:    0,
		keys:      "↑/↓ pgup/pgdn home/end scroll · enter or esc close",
	}
}

// ---------- after the program: what to print on the normal screen ----------

func (w *wizard) finish(out io.Writer) error {
	say := func(format string, a ...any) { _, _ = fmt.Fprintf(out, format, a...) }
	statePath, _ := setupStatePath()
	switch w.exit {
	case exitAbort:
		if w.install.started && !w.install.done {
			ns := w.state.namespace()
			say("Oberth resources may remain in namespace %s — run `helm uninstall oberth -n %s` to clean up.\n", ns, ns)
		}
		return installer.ErrInterrupted
	case exitDry:
		// --dry-mode: the equivalent non-interactive command. It contains
		// no secret by construction: BuildCommandLine reads only
		// non-secret fields.
		say("--dry-mode: no changes applied. Equivalent command:\n\n  %s\n", BuildCommandLine(&w.state))
		return nil
	case exitAfterFailure:
		say("Install failed at %q: %s\n", w.state.Install.Phase, w.state.Install.Error)
		say("Answers are saved in %s; rerun `oberth setup` to fix the step and retry.\n", displayHome(statePath))
		return fmt.Errorf("install failed: %s", w.state.Install.Error)
	}
	switch w.state.Install.Status {
	case installComplete:
		say("Oberth is installed and ready.\n")
		return nil
	case installPendingForge:
		say("Oberth is installed but waits for forge authorization of its upstream key.\n")
		for _, line := range recoveryCommands(&w.state) {
			say("  %s\n", line)
		}
		say("Or rerun `oberth setup` and choose Verify.\n")
		return installer.ErrOnboardingPartial
	case installPartial:
		say("Oberth is installed, but onboarding did not finish: %s\n", w.state.Install.Error)
		say("Rerun `oberth setup` and choose Verify, or see the saved report.\n")
		return installer.ErrOnboardingPartial
	case installFailed:
		say("The last install failed at %q. Answers are saved in %s; rerun `oberth setup` to continue.\n",
			w.state.Install.Phase, displayHome(statePath))
		return nil
	}
	if !w.opts.DryMode && statePath != "" {
		say("Setup answers are saved in %s (no secrets); rerun `oberth setup` to continue.\n", displayHome(statePath))
	}
	return nil
}

// ---------- the equivalent command ----------

// BuildCommandLine generates the equivalent `oberth install` command from
// the wizard state, for --dry-mode and the review screen.
func BuildCommandLine(state *WizardState) string {
	c := state.Config
	args := []string{"oberth install"}
	if c.CreateKind {
		args = append(args, "--create-kind")
	}
	if c.KubeconfigPath != "" {
		args = append(args, "--kubeconfig="+shellQuote(c.KubeconfigPath))
	}
	if c.ContextName != "" && !c.CreateKind {
		args = append(args, "--context="+shellQuote(c.ContextName))
	}
	if c.UpstreamPrivateKeyPath != "" {
		args = append(args, "--upstream-key-file="+shellQuote(c.UpstreamPrivateKeyPath))
	}
	if c.Dev {
		args = append(args, "--dev")
	}
	if c.Namespace != "" && c.Namespace != installer.DefaultNamespace {
		args = append(args, "--namespace="+c.Namespace)
	}
	if c.ArgoNamespace != "" {
		args = append(args, "--argo-namespace="+c.ArgoNamespace)
	}
	if c.OpenBaoNamespace != "" && c.OpenBaoNamespace != installer.DefaultOpenBaoNamespace {
		args = append(args, "--openbao-namespace="+c.OpenBaoNamespace)
	}
	if c.InstallSecretStore {
		args = append(args, "--install-secretstore")
	}
	if c.InstallSecretStoreDev {
		args = append(args, "--install-secretstore-dev")
	}
	if c.InstallRekor {
		args = append(args, "--install-rekor")
	}
	if c.NetworkPolicy != "" {
		args = append(args, "--network-policy="+c.NetworkPolicy)
	}
	if c.ArgoVaultAddress != "" {
		args = append(args, "--argo-vault-address="+shellQuote(c.ArgoVaultAddress))
	}
	if c.ArgoVaultCredentialedRole != "" {
		args = append(args, "--argo-vault-credentialed-role="+c.ArgoVaultCredentialedRole)
	}
	for _, dns := range c.TLSExtraDNSNames {
		args = append(args, "--tls-extra-dns-name="+dns)
	}
	for _, ip := range c.TLSExtraIPs {
		args = append(args, "--tls-extra-ip="+ip)
	}
	// --yes suppresses the installer's own remote-cluster consent gate.
	// Recommend it ONLY when a probe positively identified a remote
	// cluster — never because detection failed or was skipped.
	if state.ClusterInfo.err == nil && state.ClusterInfo.context != "" && !state.ClusterInfo.isLocal {
		args = append(args, "--yes")
	}
	if c.ChartPath != "" {
		args = append(args, "--chart="+shellQuote(c.ChartPath))
	}
	if c.ImageRef != "" {
		args = append(args, "--image="+shellQuote(c.ImageRef))
	}
	if state.ForgeOrg != "" {
		args = append(args, "--forge="+shellQuote(state.ForgeType), "--organization="+shellQuote(state.ForgeOrg),
			"--upstream-url="+shellQuote(forgeUpstreamURL(state.ForgeType, state.ForgeOrg)))
	}
	if state.UplinkIdentity != "" {
		args = append(args, "--uplink-identity="+shellQuote(state.UplinkIdentity))
	}
	if state.SSHKeyPath != "" {
		args = append(args, "--ssh-public-key="+shellQuote(state.SSHKeyPath))
	}
	if state.ClientAccess != "" {
		args = append(args, "--client-access="+state.ClientAccess)
	}
	return strings.Join(args, " \\\n  ")
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// installConfig is the installer.Config the install step runs: the answers
// plus the onboarding wiring the installer's non-interactive path needs.
func installConfig(st *WizardState) installer.Config {
	cfg := st.Config
	cfg.ForgeType = st.ForgeType
	cfg.ForgeOrg = st.ForgeOrg
	cfg.ForgeURL = forgeUpstreamURL(st.ForgeType, st.ForgeOrg)
	cfg.UplinkIdentity = st.UplinkIdentity
	cfg.SSHPublicKeyPath = st.SSHKeyPath
	cfg.ClientAccess = st.ClientAccess
	cfg.SecretStoreUndecided = false
	// The user confirmed on the review screen.
	cfg.Yes = true
	// Dev builds stamp BinaryVersion as "dev-<sha>"; let the installer pick
	// the published chart instead of deriving a version from the stamp.
	if strings.HasPrefix(cfg.BinaryVersion, "dev") {
		cfg.ChartVersion = ""
	}
	return cfg
}
