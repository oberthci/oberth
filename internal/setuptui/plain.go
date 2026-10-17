package setuptui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/installer"
)

// runPlain is the plain/accessible flow (S12): the same steps, questions and
// validation as the TUI, asked as sequential prompts — no color, no cursor
// addressing (#047, #011). It is reached only through --plain/--accessible
// or a non-interactive terminal, never by a key press inside the TUI.
func runPlain(ctx context.Context, opts Options, st *WizardState, resumed bool, loadErr error, input io.Reader, output io.Writer) error {
	p := &plainRun{ctx: ctx, opts: opts, st: st, reader: bufio.NewReader(input), out: output, questions: buildQuestions()}
	return p.run(resumed, loadErr, input)
}

type plainRun struct {
	ctx       context.Context
	opts      Options
	st        *WizardState
	reader    *bufio.Reader
	out       io.Writer
	questions []*question
}

func (p *plainRun) say(format string, a ...any) { _, _ = fmt.Fprintf(p.out, format, a...) }

// para prints wrapped, indented text.
func (p *plainRun) para(text string) {
	for _, line := range wrapText(text, 76) {
		p.say("  %s\n", line)
	}
}

func (p *plainRun) persist() {
	if p.opts.DryMode {
		return
	}
	if err := saveSetupState(p.st); err != nil {
		p.say("  WARNING: answers could not be saved: %v\n", err)
	}
}

// ask reads one answer; empty selects def.
func (p *plainRun) ask(label, def string) (string, error) {
	if def != "" {
		p.say("  %s [%s]: ", label, def)
	} else {
		p.say("  %s: ", label)
	}
	line, err := readLine(p.ctx, p.reader)
	if errors.Is(err, installer.ErrInterrupted) {
		return "", err
	}
	if err != nil && line == "" {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		answer = def
	}
	return answer, nil
}

func (p *plainRun) run(resumed bool, loadErr error, input io.Reader) error {
	statePath, _ := setupStatePath()
	p.say("\n  OBERTH SETUP\n\n")
	p.say("  Steps: ")
	for i := 0; i < stepCount; i++ {
		if i > 0 {
			p.say(" · ")
		}
		p.say("%d. %s", i+1, stepID(i).name())
	}
	p.say("\n")
	switch {
	case loadErr != nil:
		p.say("  Previous answers were not loaded: %v\n", loadErr)
	case resumed:
		p.say("  Resuming the answers saved in %s.\n", displayHome(statePath))
	}

	if resumed && !p.opts.DryMode && (p.st.Install.Status == installPendingForge || p.st.Install.Status == installPartial) {
		p.say("\n  The last install is waiting: %s\n", headline(p.st, nil))
		answer, err := p.choose("Verify it now (yes), or review the answers (no)?", "yes", "yes", "no")
		if err != nil {
			return err
		}
		if answer == "yes" {
			return p.verify()
		}
	}

	for {
		for step := stepCluster; step < stepReview; step++ {
			if !p.st.done[step] {
				if err := p.walk(step); err != nil {
					return err
				}
			}
		}
		again, err := p.review()
		if err != nil {
			return err
		}
		if again {
			continue
		}
		if p.opts.DryMode {
			p.say("\n  --dry-mode: no changes applied. Equivalent command:\n\n  %s\n", BuildCommandLine(p.st))
			return nil
		}
		answer, err := p.ask(fmt.Sprintf("Install now? yes, no, or a step number (1-%d) to change", int(stepClient)+1), "yes")
		if err != nil {
			return err
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= int(stepClient)+1 {
			p.st.done[n-1] = false
			continue
		}
		switch strings.ToLower(answer) {
		case "yes", "y":
			return p.install(input)
		case "no", "n":
			p.say("  Nothing applied. Answers are saved in %s; rerun `oberth setup` to continue.\n", displayHome(statePath))
			return nil
		}
		p.say("  ERROR: answer yes, no, or a step number\n")
	}
}

// walk asks one step's questions.
func (p *plainRun) walk(step stepID) error {
	p.say("\n  Step %d of %d — %s\n", int(step)+1, stepCount, step.name())
	for _, q := range questionsOf(p.questions, step) {
		if !q.appliesTo(p.st) {
			continue
		}
		if err := p.askQuestion(q); err != nil {
			return err
		}
	}
	p.st.done[step] = true
	p.persist()
	return nil
}

// choose asks for one of a fixed set of words.
func (p *plainRun) choose(label, def string, allowed ...string) (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := p.ask(label, def)
		if err != nil {
			return "", err
		}
		for _, a := range allowed {
			if strings.EqualFold(answer, a) || (len(answer) == 1 && strings.HasPrefix(a, strings.ToLower(answer))) {
				return a, nil
			}
		}
		p.say("  ERROR: answer %s\n", strings.Join(allowed, " or "))
	}
	return "", fmt.Errorf("%s: three invalid answers", label)
}

// askQuestion prompts until the shared validation accepts the answer.
func (p *plainRun) askQuestion(q *question) error {
	p.say("\n  %s\n", q.heading)
	p.para(q.explain(p.st))
	for attempt := 0; attempt < 3; attempt++ {
		answer, err := p.answer(q)
		if err != nil {
			return err
		}
		if answer == nil {
			continue
		}
		if err := q.save(p.st, answer); err != nil {
			if errors.Is(err, errRescan) {
				p.say("  Scanned ~/.ssh again.\n")
				attempt--
				continue
			}
			p.say("  ERROR: %v\n", err)
			continue
		}
		if q.probe != nil {
			if work := q.probe(p.st); work != nil {
				p.say("  Checking…\n")
				result := work()
				if result.err != nil {
					p.say("  ERROR: %v\n", result.err)
					continue
				}
				result.apply(p.st)
				p.say("  OK\n")
			}
		}
		return nil
	}
	return fmt.Errorf("%s: three invalid answers", q.heading)
}

// answer reads a choice (by number, value or label) or the text fields. A
// nil answer means the input was rejected and is asked again.
func (p *plainRun) answer(q *question) ([]string, error) {
	if !q.isChoice() {
		current := q.values(p.st)
		answer := make([]string, len(q.fields))
		for i, field := range q.fields {
			v, err := p.ask(field+" (- for none)", current[i])
			if err != nil {
				return nil, err
			}
			if v == "-" {
				v = ""
			}
			answer[i] = v
		}
		return answer, nil
	}
	opts := q.options(p.st)
	selected := q.selected(p.st)
	def := ""
	for i, o := range opts {
		line := fmt.Sprintf("  %d) %s", i+1, o.label)
		if o.detail != "" {
			line += " — " + o.detail
		}
		p.say("%s\n", line)
		if o.value == selected {
			def = strconv.Itoa(i + 1)
		}
	}
	if def == "" && len(opts) > 0 {
		def = "1"
	}
	v, err := p.ask("Choice", def)
	if err != nil {
		return nil, err
	}
	if n, convErr := strconv.Atoi(v); convErr == nil {
		if n >= 1 && n <= len(opts) {
			return []string{opts[n-1].value}, nil
		}
		p.say("  ERROR: choose 1 to %d\n", len(opts))
		return nil, nil
	}
	for _, o := range opts {
		if strings.EqualFold(v, o.value) || strings.EqualFold(v, o.label) {
			return []string{o.value}, nil
		}
	}
	p.say("  ERROR: choose 1 to %d\n", len(opts))
	return nil, nil
}

// review prints the plan and re-checks every answer; again reports that a
// step was reopened because its answer no longer holds.
func (p *plainRun) review() (bool, error) {
	p.say("\n  Step %d of %d — %s\n", int(stepReview)+1, stepCount, stepReview.name())
	for step := stepCluster; step < stepReview; step++ {
		p.say("  %d. %-22s %s\n", int(step)+1, step.name(), stepSummary(p.st, step))
	}
	if q, err := revalidate(p.questions, p.st); err != nil {
		p.say("  ERROR: %s: %v\n", q.step.name(), err)
		p.st.done[q.step] = false
		return true, nil
	}
	if !p.st.Config.CreateKind {
		p.say("  Checking the cluster connection…\n")
		result := probeContext(p.st.Config.ContextName, p.st.Config.KubeconfigPath)()
		if result.err != nil {
			p.say("  ERROR: cluster: %v\n", result.err)
			p.st.done[stepCluster] = false
			return true, nil
		}
		result.apply(p.st)
	}
	p.st.done[stepReview] = true
	if !p.opts.DryMode {
		p.say("\n  Equivalent command:\n  %s\n", BuildCommandLine(p.st))
	}
	return false, nil
}

// install runs the installer with its normal terminal output; once-only
// credentials are printed here, once, through the credential sink.
func (p *plainRun) install(input io.Reader) error {
	p.say("\n  Step %d of %d — %s\n", int(stepInstall)+1, stepCount, stepInstall.name())
	statuses := map[string]string{}
	// Credentials still reach the operator here, once, but are also
	// registered so nothing derived from the installer's output (the error
	// persisted in setup.json) can carry one.
	secrets := newMasker()
	defer secrets.wipe()
	err := executeInstaller(p.ctx, installConfig(p.st), installer.InstallDeps{
		Output:     p.out,
		Input:      input,
		IsTerminal: func() bool { return false },
		StepProgressSink: func(step, status string) {
			statuses[step] = status
		},
		CredentialSink: func(label, value string) {
			secrets.register([]byte(value))
			p.say("\n  %s (shown once, save it now): %s\n", label, value)
		},
	})
	rec := &p.st.Install
	rec.Phase, rec.Error = "", ""
	phase := -1
	switch {
	case err == nil:
		rec.Status = installComplete
	case errors.Is(err, installer.ErrOnboardingPartial):
		rec.Status = installPartial
		rec.Error = "onboarding did not finish"
		if statuses["upstream discovery"] == "pending" && statuses["mint uplink"] == "done" {
			rec.Status = installPendingForge
			rec.Error = "the forge has not accepted the upstream key yet"
		}
	default:
		phase = len(installPhases) - 1
		for name, i := range stepNameToIndex {
			if statuses[name] != "done" && i < phase {
				phase = i
			}
		}
		rec.Status = installFailed
		rec.Phase = installPhases[phase]
		rec.Error = persistableError(secrets.mask(err.Error()))
	}
	if rec.Status != installFailed {
		p.st.done[stepInstall] = true
	}
	p.persist()
	switch rec.Status {
	case installFailed:
		step := responsibleStep(phase, rec.Error, p.st)
		p.say("\n  The install failed at %q. Most likely step %d, %s, needs changing; rerun `oberth setup` and choose it.\n",
			rec.Phase, int(step)+1, step.name())
		return err
	case installPendingForge:
		p.say("\n  %s\n", headline(p.st, nil))
		for _, line := range recoveryCommands(p.st) {
			p.say("  %s\n", line)
		}
		p.say("  Or rerun `oberth setup` and answer yes to verify.\n")
		return installer.ErrOnboardingPartial
	case installPartial:
		return err
	}
	return nil
}

// verify re-runs the forge registration and reports readiness (#002).
func (p *plainRun) verify() error {
	p.say("  Verifying…\n")
	v := verifyInstall(p.ctx, *p.st, true)
	if v.ready && v.upstream {
		p.st.Install.Status = installComplete
		p.st.Install.Error = ""
		p.persist()
	}
	for _, line := range finishLines(&wizard{state: *p.st, install: newInstallScreen()}, &v) {
		p.say("  %s\n", line)
	}
	if p.st.Install.Status == installComplete {
		return nil
	}
	return installer.ErrOnboardingPartial
}

// lineResult carries one line read off the prompt reader.
type lineResult struct {
	line string
	err  error
}

// readLine reads one line from r and gives up the moment ctx is canceled.
// main wires SIGINT into ctx, so ctrl+c at a prompt surfaces as
// installer.ErrInterrupted (exit 130) instead of waiting for the next enter.
func readLine(ctx context.Context, r *bufio.Reader) (string, error) {
	ch := make(chan lineResult, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- lineResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", installer.ErrInterrupted
	case res := <-ch:
		return res.line, res.err
	}
}

// persistableError keeps the first line of an installer error, bounded, for
// setup.json and the report: enough to say what failed, never a log dump.
func persistableError(message string) string {
	if i := strings.IndexByte(message, '\n'); i >= 0 {
		message = message[:i]
	}
	const limit = 300
	if len(message) > limit {
		message = message[:limit] + "…"
	}
	return message
}
