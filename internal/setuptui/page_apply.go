package setuptui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/oberthci/oberth/internal/installer"
)

// applyStep tracks one step in the apply process.
type applyStep struct {
	name     string
	status   string // "pending", "running", "done", "failed"
	duration time.Duration
}

// applyLogMsg delivers a single masked log line to the TUI.
type applyLogMsg struct {
	line string
}

type applyPage struct {
	steps        []applyStep
	currentStep  int
	holdState    bool
	holdError    string
	holdDetail   string
	logTail      string
	logLines     []string
	startTime    time.Time
	masker       *masker
	ceremony     *ceremonyPage
	showCeremony bool
	applyDone    bool

	// Cancellable context for the installer goroutine — a confirmed abort
	// calls cancel() so the installer stops mutating (Bug 7).
	cancel context.CancelFunc

	// Channel-based message passing from the installer goroutine. A fresh
	// channel is created per run (startApply): the previous run's channel is
	// closed by its runInstaller defer, and sending on a closed channel
	// panics — re-entry after a failed run (r retry, esc → fix → apply
	// again) MUST get a new one.
	msgCh chan tea.Msg

	// execInstaller runs the real installer; tests inject a stub so retry
	// and failure paths are testable without a cluster. Nil selects
	// installer.Execute.
	execInstaller func(ctx context.Context, cfg installer.Config, deps installer.InstallDeps) error
}

func newApplyPage() *applyPage {
	return &applyPage{
		masker:   newMasker(),
		ceremony: newCeremonyPage(),
		msgCh:    make(chan tea.Msg, 64),
		steps: []applyStep{
			{name: "render chart", status: "pending"},
			{name: "apply namespace · rbac · networkpolicy", status: "pending"},
			{name: "deploy oberth (digest-pinned)", status: "pending"},
			{name: "rollout ready", status: "pending"},
			{name: "install openbao", status: "pending"},
			{name: "secretstore verify — server leg", status: "pending"},
			{name: "secretstore probe — release leg", status: "pending"},
			{name: "tls + ssh host keys · fingerprints", status: "pending"},
			{name: "mint uplink — ceremony pauses here", status: "pending"},
			{name: "upstream discovery + deploy keys", status: "pending"},
			{name: "audit chain genesis · verify · readyz", status: "pending"},
		},
	}
}

// wipeSecrets zeros every secret this page or its ceremony still holds.
// Called on every teardown path — acknowledged, aborted, or interrupted —
// so no exit route leaves credential bytes live on the heap (S2/S10).
func (p *applyPage) wipeSecrets() {
	p.ceremony.zeroAll()
	p.masker.wipe()
}

func (p *applyPage) title() string {
	if p.holdState {
		return "ignition — hold"
	}
	return "ignition"
}

func (p *applyPage) question() string {
	if p.holdState {
		return "Holding."
	}
	return "Applying."
}

func (p *applyPage) keys() string {
	if p.showCeremony {
		return sKey.Render("r") + " reveal · " + sKey.Render("c") + " copy · " + sKey.Render("enter") + " acknowledge"
	}
	if p.holdState {
		return sKey.Render("r") + " retry · " + sKey.Render(fmt.Sprintf("1..%d", len(reviewSectionPages))) + " revisit page · " + sKey.Render("ctrl+c") + " abort"
	}
	return sKey.Render("ctrl+c") + " abort"
}

// bandPercent returns the apply step progress as a fraction (0.0-1.0) for the
// progress band. During apply, the band tracks step completion, not page count.
func (p *applyPage) bandPercent() float64 {
	done := 0
	for _, s := range p.steps {
		if s.status == "done" {
			done++
		}
	}
	if len(p.steps) == 0 {
		return 0
	}
	return float64(done) / float64(len(p.steps))
}

func (p *applyPage) init(state *WizardState) tea.Cmd {
	p.startTime = time.Now()

	// In dry-mode, we don't actually apply.
	if state.Config.DryRun {
		return nil
	}

	// Start the real apply by running installer.Execute in a goroutine and
	// listening on the channel for progress messages.
	return p.startApply(state)
}

// listenForMsg returns a tea.Cmd that blocks on the message channel and
// delivers one message. Chained in update() to consume the full stream.
// The channel is captured HERE, on the update goroutine — the closure runs
// on a tea worker goroutine, and reading p.msgCh there would race with a
// retry reassigning it.
func (p *applyPage) listenForMsg() tea.Cmd {
	ch := p.msgCh
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return applyDoneMsg{}
		}
		return msg
	}
}

// startApply launches installer.Execute in a goroutine and returns a tea.Cmd
// that begins listening for progress messages.
// forgeUpstreamURL derives the upstream URL from the wizard's forge
// selection. Returns "" for unknown forge types (the installer skips the
// upstream step in that case).
func forgeUpstreamURL(forgeType, org string) string {
	if org == "" {
		return ""
	}
	switch forgeType {
	case "github":
		return "github.com/" + org
	case "codeberg":
		return "codeberg.org/" + org
	case "gitlab":
		return "gitlab.com/" + org
	default:
		return ""
	}
}

func (p *applyPage) startApply(state *WizardState) tea.Cmd {
	cfg := state.Config

	// Wire the wizard-collected onboarding data into Config so the
	// installer's non-interactive onboarding path uses it instead of
	// prompting (which would EOF on the empty reader).
	cfg.ForgeType = state.ForgeType
	cfg.ForgeOrg = state.ForgeOrg
	cfg.ForgeURL = forgeUpstreamURL(state.ForgeType, state.ForgeOrg)
	cfg.UplinkIdentity = state.UplinkIdentity
	cfg.SSHPublicKeyPath = state.SSHKeyPath
	// The user already confirmed on the review page.
	cfg.Yes = true

	// Dev builds stamp BinaryVersion as "dev-<sha>". The installer's
	// Validate guards against literal "dev" but not "dev-*", and there is
	// no --chart/--chart-version escape hatch in the wizard. Clear
	// ChartVersion so Validate does not derive a chart version from the
	// dev stamp — the installer will use the latest published chart.
	if strings.HasPrefix(cfg.BinaryVersion, "dev") {
		cfg.ChartVersion = ""
	}

	// Reset for this run. A previous run's channel is closed by its
	// runInstaller defer, and its steps carry terminal state — retry ('r')
	// and re-entry after esc→fix→apply need a fresh channel (sending on
	// the closed one panics) and a clean tracker.
	p.resetForRun()

	// Derive a cancellable context — a confirmed abort calls cancel() so the
	// installer stops mutating instead of running until process death (Bug 7).
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	// Capture the run's own channel: the closures run on other goroutines,
	// and a later retry reassigns p.msgCh.
	ch := p.msgCh

	return func() tea.Msg {
		go p.runInstaller(ctx, cfg, ch)
		// Return the first message from the channel.
		msg, ok := <-ch
		if !ok {
			return applyDoneMsg{}
		}
		return msg
	}
}

// resetForRun prepares the page for a (re)run of the installer: fresh
// message channel, all steps pending, hold and completion state cleared.
// Secrets already collected by the ceremony are deliberately kept — a retry
// after a partial failure must not discard credentials that already exist
// nowhere else.
func (p *applyPage) resetForRun() {
	p.msgCh = make(chan tea.Msg, 64)
	for i := range p.steps {
		p.steps[i].status = "pending"
		p.steps[i].duration = 0
	}
	p.currentStep = 0
	p.holdState = false
	p.holdError = ""
	p.holdDetail = ""
	p.applyDone = false
	p.logTail = ""
	p.logLines = nil
	p.startTime = time.Now()
}

// stepNameToIndex maps the stable step identifiers emitted by the installer's
// StepProgressSink to the TUI step tracker indices. Adding a new installer
// step requires one entry here and one sink call in the installer.
var stepNameToIndex = map[string]int{
	"render chart":        0,
	"apply namespace":     1,
	"deploy oberth":       2,
	"rollout ready":       3,
	"install openbao":     4,
	"secretstore server":  5,
	"secretstore release": 6,
	"tls fingerprints":    7,
	"mint uplink":         8,
	"upstream discovery":  9,
	"audit genesis":       10,
}

// runInstaller runs the installer in a goroutine and sends progress messages
// to ch — the channel owned by exactly this run. It closes ch when done.
//
// p.steps is owned by the Bubble Tea update goroutine; this method snapshots
// step names and count into locals before the execute call and tracks
// completion in a goroutine-local map — never reading p.steps again — to
// avoid a data race between the two goroutines.
func (p *applyPage) runInstaller(ctx context.Context, cfg installer.Config, ch chan tea.Msg) {
	defer close(ch)

	// Snapshot step metadata — after this point p.steps must not be read
	// from this goroutine.
	stepNames := make([]string, len(p.steps))
	for i := range p.steps {
		stepNames[i] = p.steps[i].name
	}
	totalSteps := len(stepNames)

	// Local completion set — tracks which step indices the
	// StepProgressSink has reported "done", so the error/completion
	// paths can iterate without touching p.steps.
	doneSet := make(map[int]bool)

	// Mark the first step as running.
	ch <- applyStepMsg{
		step:   0,
		total:  totalSteps,
		name:   stepNames[0],
		status: "running",
	}

	w := &applyWriter{
		ch:     ch,
		masker: p.masker,
	}

	execute := p.execInstaller
	if execute == nil {
		execute = installer.Execute
	}

	// Use a non-interactive reader instead of os.Stdin to prevent stdin
	// contention with bubbletea's raw mode (Bug 7). IsTerminal returns
	// false so host.go never wires term.ReadPassword/MakeRaw to os.Stdin,
	// and the installer enters the pre-filled onboarding path instead of
	// the interactive prompting path.
	//
	// CredentialSink is the S1/S2/S3 load-bearing wire: every once-only
	// credential (root token, unseal keys, bearer token) arrives here as
	// structured data. It is registered with the masker FIRST — on this
	// goroutine, before any later output line could echo it — then handed
	// to the ceremony, which copies and zeros the delivery buffer. Without
	// this sink the installer would flush credentials as boxed text into
	// the log stream, where the bare-line "oberth_" detection can never
	// match (every box line starts with a border rune) and the raw values
	// would land in the retained log lines.
	//
	// StepProgressSink replaces the old substring-pattern matching: the
	// installer calls the sink with stable step identifiers at logical
	// completion points; the TUI maps those to step indices via
	// stepNameToIndex.
	err := execute(ctx, cfg, installer.InstallDeps{
		Output:     w,
		Input:      strings.NewReader(""),
		IsTerminal: func() bool { return false },
		CredentialSink: func(label, value string) {
			b := []byte(value)
			p.masker.register(b)
			// Blocking send — credential delivery must never be dropped.
			ch <- ceremonyTokenMsg{label: label, token: b}
		},
		StepProgressSink: func(step, status string) {
			idx, ok := stepNameToIndex[step]
			if !ok || idx >= totalSteps {
				return
			}
			if status == "done" {
				doneSet[idx] = true
			}
			ch <- applyStepMsg{
				step:   idx,
				total:  totalSteps,
				name:   stepNames[idx],
				status: status,
			}
		},
	})

	// On failure, mark the first non-done step as failed so the HOLD
	// view names the step that blocked progress.
	if err != nil {
		// ErrOnboardingPartial is a structured partial success: some steps
		// completed, but manual action is needed for the rest. Do NOT mark
		// remaining steps as failed — leave them pending so the done page
		// shows accurate counts and guidance.
		if errors.Is(err, installer.ErrOnboardingPartial) {
			ch <- applyDoneMsg{err: err}
			return
		}

		failStep := totalSteps - 1
		for i := 0; i < totalSteps; i++ {
			if !doneSet[i] {
				failStep = i
				break
			}
		}
		ch <- applyStepMsg{
			step:   failStep,
			total:  totalSteps,
			name:   stepNames[failStep],
			status: "failed",
			err:    err,
		}
		ch <- applyDoneMsg{err: err}
		return
	}

	// Mark remaining unreported steps as skipped — only steps the
	// StepProgressSink reported "done" are genuinely done. Skipped steps
	// show a dim indicator instead of a green checkmark (UX-12).
	for i := 0; i < totalSteps; i++ {
		if !doneSet[i] {
			ch <- applyStepMsg{
				step:   i,
				total:  totalSteps,
				name:   stepNames[i],
				status: "skipped",
			}
		}
	}

	ch <- applyDoneMsg{}
}

// applyWriter is an io.Writer that captures installer output, masks secrets,
// and sends log lines to the TUI channel. Step tracking is handled by the
// StepProgressSink, not by output scraping.
type applyWriter struct {
	ch     chan tea.Msg
	masker *masker
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (w *applyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// Incomplete line — put it back for next write.
			w.buf.WriteString(line)
			break
		}
		line = strings.TrimRight(line, "\n\r")
		if line == "" {
			continue
		}
		w.processLine(line)
	}
	return len(p), nil
}

func (w *applyWriter) processLine(line string) {
	// Check for bearer token: lines starting with "oberth_" are token
	// values (see extractUplinkToken). This backstop covers the rare case
	// where a credential appears in the log stream without a CredentialSink
	// delivery.
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "oberth_") {
		w.ch <- ceremonyTokenMsg{token: []byte(trimmed)}
		return // Never send the token to the log tail.
	}

	// Send as log tail (masked). Step tracking is handled by the
	// installer's StepProgressSink, not by output scraping.
	masked := w.masker.mask(trimmed)
	select {
	case w.ch <- applyLogMsg{line: masked}:
	default:
		// Channel full — drop the log line rather than blocking the installer.
	}
}

func (p *applyPage) update(msg tea.Msg, state *WizardState) (page, tea.Cmd) {
	switch msg := msg.(type) {
	case applyStepMsg:
		if msg.step < len(p.steps) {
			p.steps[msg.step].status = msg.status
			p.steps[msg.step].duration = msg.duration
			p.currentStep = msg.step

			if msg.status == "failed" {
				p.holdState = true
				p.holdError = p.steps[msg.step].name
				if msg.err != nil {
					p.holdDetail = msg.err.Error()
				}
				return p, p.listenForMsg()
			}
		}
		return p, p.listenForMsg()

	case applyLogMsg:
		p.logTail = msg.line
		if len(p.logLines) < 1000 {
			p.logLines = append(p.logLines, msg.line)
		}
		return p, p.listenForMsg()

	case applyDoneMsg:
		p.applyDone = true
		// Defense-in-depth: if holdState is already set (by a failed
		// applyStepMsg), no stray nil-error done message can advance
		// past the HOLD view. This guards against the dual-consumer
		// bug where a closed channel synthesizes applyDoneMsg{}.
		if p.holdState {
			return p, nil
		}
		if msg.err != nil {
			// ErrOnboardingPartial is a structured partial success: the
			// deployment is up but manual steps remain (e.g. deploy key
			// registration). Advance to the done page with accurate step
			// counts instead of entering HOLD.
			if errors.Is(msg.err, installer.ErrOnboardingPartial) {
				// Fall through to ceremony check and done-page transition.
			} else {
				p.holdState = true
				p.holdError = msg.err.Error()
				return p, nil
			}
		}
		// If the ceremony holds credentials that haven't been acknowledged,
		// show the ceremony first.
		if p.ceremony.hasCredentials() && !p.ceremony.acknowledged {
			p.showCeremony = true
			return p, nil
		}
		// All steps done — transition to done page.
		return p, func() tea.Msg { return pageCompleteMsg{} }

	case ceremonyTokenMsg:
		// The credential ceremony interrupts the apply as values arrive.
		// Register with the masker for log-tail safety (S3) first — the
		// masker copies — then hand the buffer to the ceremony page, which
		// copies and ZEROS the source (S2: no stray copy survives on the
		// message value). The sink path pre-registers on the installer
		// goroutine; registering again here is a harmless duplicate that
		// also covers the bare-line backstop path.
		p.masker.register(msg.token)
		label := msg.label
		if label == "" {
			label = "Bearer token"
		}
		p.ceremony.addCredential(label, msg.token)
		p.showCeremony = true
		return p, p.listenForMsg()

	case tea.KeyPressMsg:
		// If the ceremony is showing, delegate to it.
		if p.showCeremony {
			newCeremony, cmd := p.ceremony.update(msg, nil)
			p.ceremony = newCeremony.(*ceremonyPage)
			if p.ceremony.acknowledged {
				p.showCeremony = false
				// Bug 1/2: the ceremony must NOT emit pageCompleteMsg.
				// Check the real apply state after dismissing the overlay.
				if p.holdState {
					// Installer failed while the ceremony was showing —
					// land on the HOLD view so the user sees the error.
					return p, nil
				}
				if p.applyDone {
					// Installer finished cleanly — advance to done.
					return p, func() tea.Msg { return pageCompleteMsg{} }
				}
				// Installer still running — the existing listener
				// chain (from the ceremonyTokenMsg handler) is already
				// alive and draining the channel. Returning nil avoids
				// creating a duplicate consumer on p.msgCh.
				return p, nil
			}
			// Not acknowledged — forward non-pageCompleteMsg commands
			// (clipboard copy, etc.).
			return p, cmd
		}

		switch msg.String() {
		case "r":
			if p.holdState {
				// Retry: actually re-run the installer. The previous run's
				// goroutine has exited and closed its channel; startApply
				// resets the tracker and creates a fresh channel — flipping
				// UI state alone would spin forever with nothing running.
				if p.cancel != nil {
					p.cancel()
				}
				return p, p.startApply(state)
			}
		case "esc":
			if p.holdState {
				return p, func() tea.Msg { return pageBackMsg{} }
			}
		// Number keys in HOLD state to jump back to a page — the SAME
		// section numbering the review page teaches (1..7 → cluster,
		// namespaces, network, store, tls, uplink, forge), not raw page
		// indices: the two must never drift apart again.
		case "1", "2", "3", "4", "5", "6", "7":
			if p.holdState {
				section := int(msg.String()[0] - '0')
				if target, ok := reviewSectionPages[section]; ok {
					return p, func() tea.Msg { return pageJumpMsg{page: target} }
				}
			}
		}
	}

	return p, nil
}

func (p *applyPage) view(state *WizardState, width, height int) string {
	// Show the ceremony overlay if active.
	if p.showCeremony {
		return p.ceremony.view(state, width, height)
	}

	if p.holdState {
		return p.viewHold(width)
	}

	var b strings.Builder

	b.WriteString("  " + sQuestion.Render(p.question()) + "\n\n")

	for _, step := range p.steps {
		var mark string
		switch step.status {
		case "done":
			mark = sGo.Render("✓")
		case "running":
			mark = lipgloss.NewStyle().Foreground(cPurple).Render("⠹")
		case "failed":
			mark = sFail.Render("✗")
		case "skipped":
			mark = sMuted.Render("–")
		default:
			mark = sMuted.Render("○")
		}

		name := sText.Render(step.name)
		dur := ""
		if step.duration > 0 {
			dur = sMuted.Render(fmt.Sprintf("%6.1fs", step.duration.Seconds()))
		}

		_, _ = fmt.Fprintf(&b, "  %s %-52s %s\n", mark, name, dur)
	}

	if p.logTail != "" {
		b.WriteString("\n  " + sMuted.Render(p.masker.mask(p.logTail)) + "\n")
	}

	return b.String()
}

func (p *applyPage) viewHold(_ int) string {
	var b strings.Builder

	b.WriteString("  " + sQuestion.Render("Holding.") + "\n\n")

	// Summary line.
	doneCount := 0
	failedName := ""
	pendingCount := 0
	for _, step := range p.steps {
		switch step.status {
		case "done":
			doneCount++
		case "failed":
			failedName = step.name
		default:
			if step.status == "pending" {
				pendingCount++
			}
		}
	}

	b.WriteString("  " + sGo.Render(fmt.Sprintf("✓ %d steps green", doneCount)) +
		" · " + sFail.Render("✗ "+failedName) +
		" · " + sMuted.Render(fmt.Sprintf("○ %d waiting", pendingCount)) + "\n\n")

	// HOLD gutter.
	holdContent := fmt.Sprintf("HOLD — %s", p.holdError)
	if p.holdDetail != "" {
		holdContent += "\n\n" + p.holdDetail
	}
	holdContent += "\n\n" + sKey.Render("r") + " retry step · " +
		sKey.Render("esc") + " back · " +
		sKey.Render("ctrl+c") + " abort"

	gutter := sGutter.Render(holdContent)
	b.WriteString("  " + gutter + "\n\n")

	b.WriteString("  " + sMuted.Render("no silent retries · no skipped steps") + "\n")

	return b.String()
}
