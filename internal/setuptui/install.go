package setuptui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/oberthci/oberth/internal/installer"
)

// applyStep tracks one installer phase.
type applyStep struct {
	name   string
	status string // pending, running, done, failed, skipped
}

// applyStepMsg reports progress from the installer's StepProgressSink.
type applyStepMsg struct {
	step   int
	status string
	err    error
}

// applyLogMsg delivers a single masked log line.
type applyLogMsg struct{ line string }

// applyDoneMsg reports that the installer returned.
type applyDoneMsg struct{ err error }

// ceremonyTokenMsg delivers one once-only credential to the ceremony. An
// empty label means the bare-line backstop found an oberth_ token in the
// output stream; it defaults to "Bearer token".
type ceremonyTokenMsg struct {
	label string
	token []byte
}

// installPhases are the rows of the progress screen, in the order the
// installer runs them: on failure the first row that is not done is the one
// blamed, so a misordered list blames the wrong phase.
var installPhases = []string{
	"render chart",
	"apply namespace · rbac · networkpolicy",
	"deploy oberth (digest-pinned)",
	"rollout ready",
	"install openbao",
	"secretstore verify — server leg",
	"secretstore probe — release leg",
	"tls + ssh host keys · fingerprints",
	"upstream discovery + deploy keys",
	"mint uplink",
	"audit chain genesis · verify · readyz",
}

// stepNameToIndex maps the installer's StepProgressSink identifiers to rows.
var stepNameToIndex = map[string]int{
	"render chart":        0,
	"apply namespace":     1,
	"deploy oberth":       2,
	"rollout ready":       3,
	"install openbao":     4,
	"secretstore server":  5,
	"secretstore release": 6,
	"tls fingerprints":    7,
	"upstream discovery":  8,
	"mint uplink":         9,
	"audit genesis":       10,
}

// phaseSteps names the wizard step whose answer a failing phase most likely
// depends on, so the failure dialog can send the user back there (#047).
var phaseSteps = []stepID{
	stepCluster, stepCluster, stepCluster, stepCluster,
	stepStore, stepStore, stepStore,
	stepTLS,
	stepForge,
	stepUplink,
	stepStore,
}

// failure is one failed install attempt, kept for the log and the report.
type failure struct {
	attempt int
	phase   string
	err     string
	at      time.Time
}

const (
	installRunning = iota
	installFailedView
	installCeremony
)

const (
	failRetry = iota
	failBack
	failLog
	failAbort
)

// installScreen runs installer.Execute and shows one line per phase, with a
// log view, a failure dialog and the credential ceremony.
type installScreen struct {
	steps    []applyStep
	logLines []string
	masker   *masker
	ceremony *ceremony
	view     int
	focus    focusRing
	started  bool
	done     bool
	err      error
	attempts int
	failures []failure
	failStep stepID

	cancel context.CancelFunc
	msgCh  chan tea.Msg

	// logPath is the file every masked log line is also written to, so the
	// whole log survives the session and can be read in any pager (#059).
	logPath string
	logFile *os.File
}

func newInstallScreen() *installScreen {
	s := &installScreen{masker: newMasker(), ceremony: newCeremony()}
	s.resetSteps()
	return s
}

func (s *installScreen) resetSteps() {
	s.steps = make([]applyStep, len(installPhases))
	for i, name := range installPhases {
		s.steps[i] = applyStep{name: name, status: "pending"}
	}
}

func (s *installScreen) stepStatus(prefix string) string {
	for _, step := range s.steps {
		if strings.HasPrefix(step.name, prefix) {
			return step.status
		}
	}
	return ""
}

// start launches one install attempt on a fresh channel: the previous run's
// channel is closed by its runner, and sending on it would panic.
func (s *installScreen) start(w *wizard) tea.Cmd {
	cfg := installConfig(&w.state)
	s.resetSteps()
	s.started, s.done, s.err = true, false, nil
	s.view = installRunning
	s.focus = focusRing{buttons: 1}
	s.attempts++
	s.msgCh = make(chan tea.Msg, 64)
	s.openLogFile()
	if s.attempts > 1 {
		s.appendLog(fmt.Sprintf("──── attempt %d · %s ────", s.attempts, time.Now().Format(time.TimeOnly)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	ch := s.msgCh
	go s.run(ctx, cfg, ch)
	return listen(ch)
}

// listen delivers the next message of one run's channel. The channel is
// captured here, never read from the screen on the worker goroutine.
func listen(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// run executes the installer and reports to ch, which it closes. It never
// touches s.steps: that belongs to the UI goroutine.
func (s *installScreen) run(ctx context.Context, cfg installer.Config, ch chan tea.Msg) {
	defer close(ch)
	doneSet := map[int]bool{}
	out := &applyWriter{ch: ch, masker: s.masker}
	// A non-terminal reader keeps the installer off bubbletea's stdin and on
	// its pre-filled onboarding path. CredentialSink is the S1/S2/S3 wire:
	// every once-only credential arrives as data, is registered with the
	// masker before any later line could echo it, then reaches the ceremony.
	err := executeInstaller(ctx, cfg, installer.InstallDeps{
		Output:     out,
		Input:      strings.NewReader(""),
		IsTerminal: func() bool { return false },
		CredentialSink: func(label, value string) {
			b := []byte(value)
			s.masker.register(b)
			ch <- ceremonyTokenMsg{label: label, token: b}
		},
		StepProgressSink: func(step, status string) {
			idx, ok := stepNameToIndex[step]
			if !ok {
				return
			}
			if status == "done" {
				doneSet[idx] = true
			} else {
				delete(doneSet, idx)
			}
			ch <- applyStepMsg{step: idx, status: status}
		},
	})
	out.flush()
	if err != nil && !errors.Is(err, installer.ErrOnboardingPartial) {
		failStep := len(installPhases) - 1
		for i := range installPhases {
			if !doneSet[i] {
				failStep = i
				break
			}
		}
		ch <- applyStepMsg{step: failStep, status: "failed", err: err}
	}
	ch <- applyDoneMsg{err: err}
}

// consume handles one message from the running installer.
func (s *installScreen) consume(w *wizard, msg tea.Msg) tea.Cmd {
	next := listen(s.msgCh)
	switch msg := msg.(type) {
	case applyStepMsg:
		if msg.step < len(s.steps) {
			s.steps[msg.step].status = msg.status
		}
	case applyLogMsg:
		s.appendLog(msg.line)
	case ceremonyTokenMsg:
		// The masker copies; the ceremony copies and zeros the delivery
		// buffer (S2: no stray copy survives on the message value).
		s.masker.register(msg.token)
		label := msg.label
		if label == "" {
			label = "Bearer token"
		}
		s.ceremony.add(label, msg.token)
		s.view = installCeremony
	case applyDoneMsg:
		s.done = true
		s.err = msg.err
		return s.finishRun(w)
	}
	return next
}

// openLogFile creates setup-log-<timestamp>.txt beside setup.json for the
// first attempt and reopens it for later ones. A log that cannot be written
// is not a reason to stop an install: the in-memory log still works.
func (s *installScreen) openLogFile() {
	if s.logFile != nil {
		return
	}
	if s.logPath != "" {
		file, err := os.OpenFile(s.logPath, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: the wizard's own log
		if err == nil {
			s.logFile = file
		}
		return
	}
	statePath, err := setupStatePath()
	if err != nil {
		return
	}
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, "setup-log-"+time.Now().Format("20060102-150405")+".txt")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a name the wizard builds
	if err != nil {
		return
	}
	s.logPath, s.logFile = path, file
}

// appendLog keeps one masked line in memory (bounded) and in the log file
// (complete).
func (s *installScreen) appendLog(line string) {
	if len(s.logLines) < 5000 {
		s.logLines = append(s.logLines, line)
	}
	if s.logFile != nil {
		_, _ = s.logFile.WriteString(line + "\n")
	}
}

func (s *installScreen) closeLogFile() {
	if s.logFile != nil {
		_ = s.logFile.Close()
		s.logFile = nil
	}
}

// finishRun records the outcome and picks the next view.
func (s *installScreen) finishRun(w *wizard) tea.Cmd {
	s.closeLogFile()
	rec := &w.state.Install
	rec.At = time.Now()
	rec.Phase, rec.Error = "", ""
	switch {
	case s.err == nil:
		for i := range s.steps {
			if s.steps[i].status != "done" {
				s.steps[i].status = "skipped"
			}
		}
		rec.Status = installComplete
	case errors.Is(s.err, installer.ErrOnboardingPartial):
		// The forge key is pending exactly when the installer took its
		// key-pending path: the uplink was minted but upstream discovery
		// stayed pending. Any other partial result is a different problem
		// and must not be blamed on the key (#016).
		rec.Status = installPartial
		rec.Error = "onboarding did not finish; see the log"
		if s.stepStatus("upstream discovery") == "pending" && s.stepStatus("mint uplink") == "done" {
			rec.Status = installPendingForge
			rec.Error = "the forge has not accepted the upstream key yet"
		}
	default:
		failed := len(s.steps) - 1
		for i, step := range s.steps {
			if step.status == "failed" {
				failed = i
				break
			}
		}
		rec.Status = installFailed
		rec.Phase = s.steps[failed].name
		rec.Error = s.masker.mask(s.err.Error())
		s.failStep = responsibleStep(failed, rec.Error, &w.state)
		s.failures = append(s.failures, failure{attempt: s.attempts, phase: rec.Phase, err: rec.Error, at: rec.At})
	}
	if rec.Status != installFailed {
		w.state.done[stepInstall] = true
	}
	w.persist()
	if s.ceremony.pending() {
		s.view = installCeremony
		return nil
	}
	return s.afterCeremony(w)
}

// afterCeremony leaves the ceremony: to the failure dialog, the finish
// screen, or back to the progress rows while the install still runs.
func (s *installScreen) afterCeremony(w *wizard) tea.Cmd {
	switch {
	case !s.done:
		s.view = installRunning
		s.focus = focusRing{buttons: 1}
	case s.err != nil && !errors.Is(s.err, installer.ErrOnboardingPartial):
		s.view = installFailedView
		s.focus = focusRing{buttons: 4}
	default:
		if w.screen != s {
			return nil // the user has moved on; the menu shows the outcome
		}
		f := newFinishScreen(w)
		w.setScreen(f)
		return f.start(w)
	}
	return nil
}

// responsibleStep names the step to revisit for a failure: the error text
// first (it names the cause), else the phase.
func responsibleStep(phase int, errText string, st *WizardState) stepID {
	lower := strings.ToLower(errText)
	switch {
	case strings.Contains(lower, "upstream-key-file"), strings.Contains(lower, "upstream key"),
		strings.Contains(lower, "passphrase"), strings.Contains(lower, "upstream add"),
		strings.Contains(lower, "--forge"), strings.Contains(lower, "organization"):
		return stepForge
	case strings.Contains(lower, "uplink"), strings.Contains(lower, "ssh-public-key"):
		return stepUplink
	case strings.Contains(lower, "kubeconfig"), strings.Contains(lower, "context"),
		strings.Contains(lower, "kind"), strings.Contains(lower, "cluster unreachable"):
		return stepCluster
	case strings.Contains(lower, "openbao"), strings.Contains(lower, "secret store"),
		strings.Contains(lower, "secretstore"), strings.Contains(lower, "vault"), strings.Contains(lower, "rekor"):
		return stepStore
	case strings.Contains(lower, "tls"), strings.Contains(lower, "certificate"):
		return stepTLS
	}
	if phase == len(phaseSteps)-1 && !st.Config.InstallRekor {
		return stepCluster
	}
	if phase >= 0 && phase < len(phaseSteps) {
		return phaseSteps[phase]
	}
	return stepCluster
}

func (s *installScreen) update(w *wizard, msg tea.Msg) tea.Cmd {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch s.view {
	case installCeremony:
		cmd, leave := s.ceremony.update(key)
		if leave {
			return tea.Batch(cmd, s.afterCeremony(w))
		}
		return cmd
	case installFailedView:
		k := key.String()
		if k == "esc" {
			return s.goBack(w)
		}
		if k == "l" {
			w.openLog()
			return nil
		}
		if k == "enter" {
			switch s.focus.button() {
			case failRetry:
				return s.start(w)
			case failBack:
				return s.goBack(w)
			case failLog:
				w.openLog()
			case failAbort:
				return w.quit(exitAfterFailure)
			}
			return nil
		}
		if !s.focus.handle(k) {
			s.scrollKey(w, k)
		}
		return nil
	}
	// Running: the only action is the log; ctrl+c (twice) aborts.
	switch key.String() {
	case "enter", "l":
		w.openLog()
	default:
		s.scrollKey(w, key.String())
	}
	return nil
}

func (s *installScreen) scrollKey(w *wizard, key string) {
	switch key {
	case "up", "k":
		w.scroll--
	case "down", "j":
		w.scroll++
	case "pgup":
		w.scroll -= max(1, w.height-8)
	case "pgdown":
		w.scroll += max(1, w.height-8)
	}
}

// goBack opens the step the failure points at; completing it leads back to
// the review and from there to the install again.
func (s *installScreen) goBack(w *wizard) tea.Cmd {
	return w.openStep(s.failStep)
}

func (s *installScreen) frame(w *wizard) frameSpec {
	switch s.view {
	case installCeremony:
		return s.ceremony.frame(w)
	case installFailedView:
		return s.failureFrame(w)
	}
	spec := frameSpec{
		title:     stepInstall.title(),
		heading:   "Installing Oberth",
		explain:   "One line per installer phase. Credentials that are created along the way are shown once, on their own screen.",
		focusLine: -1,
		buttons:   []string{"Show log"},
		button:    0,
		keys:      "enter or l show log · ctrl+c twice abort",
	}
	spec.body = s.phaseLines()
	if line := activityLine(s.logLines); line != "" {
		spec.status = truncateRunes(line, max(20, w.width-6))
	}
	return spec
}

// activityLine is the status line under the phase list: the newest
// meaningful installer message. Table borders and blank lines are skipped
// and a table row reads "Upstream identity: ~/.ssh/id_ed25519 · ✓ in OpenBao",
// so the line changes with progress instead of flashing raw table bytes (#070).
func activityLine(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.Trim(line, "─━┌┐└┘├┤┬┴┼│╭╮╰╯ ") == "" {
			continue
		}
		if !strings.HasPrefix(line, "│") {
			return line
		}
		var cells []string
		for _, cell := range strings.Split(strings.Trim(line, "│"), "│") {
			if cell = strings.TrimSpace(cell); cell != "" {
				cells = append(cells, cell)
			}
		}
		switch len(cells) {
		case 0:
			continue
		case 1:
			return cells[0]
		}
		return cells[0] + ": " + strings.Join(cells[1:], " · ")
	}
	return ""
}

func (s *installScreen) phaseLines() []string {
	var lines []string
	running := -1
	if s.started && !s.done {
		for i, step := range s.steps {
			if step.status == "pending" {
				running = i
				break
			}
		}
	}
	for i, step := range s.steps {
		status := step.status
		if i == running {
			status = "running"
		}
		var mark string
		switch status {
		case "done":
			mark = sGo.Render("✓")
		case "running":
			mark = sInfo.Render("›")
		case "failed":
			mark = sFail.Render("✗")
		case "skipped":
			mark = sMuted.Render("–")
		default:
			mark = sMuted.Render("○")
		}
		name := sText.Render(step.name)
		if status == "pending" || status == "skipped" {
			name = sMuted.Render(step.name)
		}
		lines = append(lines, mark+" "+name)
	}
	return lines
}

func (s *installScreen) failureFrame(w *wizard) frameSpec {
	inner := max(20, w.width-4)
	rec := w.state.Install
	spec := frameSpec{
		title:     stepInstall.title(),
		heading:   "The install failed",
		explain:   fmt.Sprintf("Phase %q failed. Most likely the answer in step %d, %s, needs changing: Go Back opens it, and its last screen leads to the review and the install again. Retry runs the install unchanged.", rec.Phase, int(s.failStep)+1, s.failStep.name()),
		focusLine: -1,
		buttons:   []string{"Retry", "Go Back", "Show log", "Abort"},
		button:    s.focus.button(),
		keys:      "←/→ buttons · enter select · esc go back · l log · ↑/↓ scroll",
	}
	spec.body = append(spec.body, sFail.Render("Error:"))
	for _, l := range wrapText(rec.Error, inner-2) {
		spec.body = append(spec.body, "  "+sText.Render(l))
	}
	spec.body = append(spec.body, "")
	spec.body = append(spec.body, s.phaseLines()...)
	if len(s.failures) > 1 {
		spec.body = append(spec.body, "", sMuted.Render(fmt.Sprintf("Earlier attempts (%d):", len(s.failures)-1)))
		for _, f := range s.failures[:len(s.failures)-1] {
			for _, l := range wrapText(fmt.Sprintf("#%d %s: %s", f.attempt, f.phase, f.err), inner-2) {
				spec.body = append(spec.body, "  "+sMuted.Render(l))
			}
		}
	}
	return spec
}

// applyWriter is an io.Writer that turns installer output into masked log
// lines. Step tracking comes from the StepProgressSink, not from output.
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
			w.buf.WriteString(line)
			break
		}
		w.processLine(line)
	}
	return len(p), nil
}

// flush emits a final unterminated line.
func (w *applyWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.processLine(w.buf.String())
		w.buf.Reset()
	}
}

func (w *applyWriter) processLine(line string) {
	trimmed := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
	if trimmed == "" {
		return
	}
	// Backstop: a bare oberth_ line is a bearer token that bypassed the
	// CredentialSink. It goes to the ceremony, never to the log.
	if strings.HasPrefix(trimmed, "oberth_") {
		w.ch <- ceremonyTokenMsg{token: []byte(trimmed)}
		return
	}
	// Blocking: dropping lines would hide the warnings issue #014 is about.
	w.ch <- applyLogMsg{line: w.masker.mask(trimmed)}
}
