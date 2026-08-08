package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// DefaultStepTimeout is used when a Step does not declare an override.
const DefaultStepTimeout = 30 * time.Minute

// processWaitDelay bounds os/exec's wait for stdout/stderr descriptors held by
// a process that escaped the step's process group.
const processWaitDelay = time.Second

// Runner executes a selected pipeline inside the short-lived Job container.
type Runner struct {
	Trigger       periapsis.Trigger
	StepTimeout   time.Duration
	Secrets       []string
	SummaryWriter io.Writer
}

// StepError describes one failed subprocess without retaining command arguments.
type StepError struct {
	Burn        string
	Step        string
	ExitCode    int
	TimedOut    bool
	Interrupted bool
	Err         error
}

func (e *StepError) Error() string {
	switch {
	case e.TimedOut:
		return fmt.Sprintf("step %s/%s timed out", e.Burn, e.Step)
	case e.Interrupted:
		return fmt.Sprintf("step %s/%s was interrupted", e.Burn, e.Step)
	default:
		return fmt.Sprintf("step %s/%s failed with exit code %d: %v", e.Burn, e.Step, e.ExitCode, e.Err)
	}
}

func (e *StepError) Unwrap() error { return e.Err }

// Run selects burns for the configured trigger and executes them sequentially.
func (r Runner) Run(ctx context.Context, pipeline periapsis.Pipeline, dir string, log io.Writer) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if log == nil {
		log = io.Discard
	}
	trigger := r.Trigger
	if trigger == "" {
		trigger = periapsis.TriggerCI
	}
	masker := NewMasker(r.Secrets)
	started := time.Now().UTC()
	summary := Summary{Version: SummaryVersion, Trigger: string(trigger), Status: StatusPassed, StartedAt: formatTime(started)}

	selected, err := periapsis.Select(pipeline, trigger)
	if err == nil {
		selected.Burns, err = periapsis.OrderedBurns(selected)
	}
	if err != nil {
		return r.finish(summary, masker, err)
	}

	defaultTimeout := r.StepTimeout
	if defaultTimeout <= 0 {
		defaultTimeout = DefaultStepTimeout
	}
	for burnIndex, burn := range selected.Burns {
		for stepIndex, step := range burn.Steps {
			if trigger == periapsis.TriggerRelease && stepContainsSetX(step) {
				prefix := fmt.Sprintf("[%s/%s] ", burn.Name, step.Name)
				writeEvent(log, masker, prefix, "WARNING: step contains 'set -x' in a release burn; shell tracing can expose transformed secret values through the redactor")
			}
			result, stepErr := runStep(ctx, burn.Name, step, dir, log, masker, defaultTimeout)
			summary.Steps = append(summary.Steps, result)
			if stepErr != nil {
				summary.Steps = appendSkipped(summary.Steps, selected.Burns, burnIndex, stepIndex+1, log, masker)
				return r.finish(summary, masker, stepErr)
			}
		}
	}
	return r.finish(summary, masker, nil)
}

func appendSkipped(results []StepResult, burns []periapsis.Burn, failedBurn, nextStep int, log io.Writer, masker Masker) []StepResult {
	for burnIndex := failedBurn; burnIndex < len(burns); burnIndex++ {
		firstStep := 0
		if burnIndex == failedBurn {
			firstStep = nextStep
		}
		for stepIndex := firstStep; stepIndex < len(burns[burnIndex].Steps); stepIndex++ {
			burn := burns[burnIndex].Name
			step := burns[burnIndex].Steps[stepIndex].Name
			timestamp := formatTime(time.Now().UTC())
			results = append(results, StepResult{
				Burn: burn, Step: step, Status: StepSkipped, ExitCode: -1,
				StartedAt: timestamp, FinishedAt: timestamp,
			})
			writeEvent(log, masker, fmt.Sprintf("[%s/%s] ", burn, step), "skipped after earlier failure")
		}
	}
	return results
}

func (r Runner) finish(summary Summary, masker Masker, runErr error) (Summary, error) {
	summary.FinishedAt = formatTime(time.Now().UTC())
	if runErr != nil {
		summary.Status = StatusFailed
		var stepErr *StepError
		if errors.As(runErr, &stepErr) && stepErr.Interrupted {
			summary.Status = StatusInterrupted
		}
		summary.Error = masker.Mask(runErr.Error())
	}
	if err := WriteSummary(r.SummaryWriter, summary); err != nil {
		if runErr == nil {
			summary.Status = StatusFailed
			summary.Error = masker.Mask(err.Error())
			return summary, err
		}
		return summary, errors.Join(runErr, err)
	}
	return summary, runErr
}

func runStep(ctx context.Context, burn string, step periapsis.Step, dir string, log io.Writer, masker Masker, defaultTimeout time.Duration) (StepResult, error) {
	started := time.Now().UTC()
	result := StepResult{Burn: burn, Step: step.Name, Status: StepPassed, StartedAt: formatTime(started)}
	prefix := fmt.Sprintf("[%s/%s] ", burn, step.Name)
	writeEvent(log, masker, prefix, "$ "+formatCommand(step.Command, step.Args))

	timeout := step.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	stepContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	environment := mergedEnvironment(step.Env)
	executable, err := resolveStepExecutable(step.Command, environment)
	if err != nil {
		result.Status = StepFailed
		result.ExitCode = -1
		result.Error = masker.Mask(err.Error())
		result.FinishedAt = formatTime(time.Now().UTC())
		writeEvent(log, masker, prefix, "failed to resolve command")
		return result, &StepError{Burn: burn, Step: step.Name, ExitCode: -1, Err: errors.New(result.Error)}
	}
	// #nosec G204 -- repository-authored steps execute only inside the isolated, credential-scoped Job.
	//nolint:noctx // the explicit timeout path kills and joins the entire process group, not only its leader.
	command := exec.Command(executable, step.Args...)
	command.Args[0] = step.Command
	command.Dir = dir
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = processWaitDelay
	output := newPrefixedWriter(log, prefix, masker)
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		result.Status = StepFailed
		result.ExitCode = -1
		result.Error = masker.Mask(err.Error())
		result.FinishedAt = formatTime(time.Now().UTC())
		writeEvent(log, masker, prefix, "failed to start")
		return result, &StepError{Burn: burn, Step: step.Name, ExitCode: -1, Err: errors.New(result.Error)}
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	var runErr error
	var contextErr error
	select {
	case runErr = <-waited:
	case <-stepContext.Done():
		contextErr = stepContext.Err()
		killErr := killProcessGroup(command.Process)
		runErr = <-waited
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) && !errors.Is(killErr, syscall.ESRCH) {
			runErr = errors.Join(runErr, fmt.Errorf("kill process group: %w", killErr))
		}
	}
	// A shell can exit while a background child keeps mutating state. Always
	// retire the exact step group after capturing the leader's status; this
	// preserves its exit code while preventing descendants from crossing steps.
	if killErr := killProcessGroup(command.Process); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) && !errors.Is(killErr, syscall.ESRCH) {
		runErr = errors.Join(runErr, fmt.Errorf("retire process group: %w", killErr))
	}
	if flushErr := output.Flush(); flushErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("capture step output: %w", flushErr))
	}

	result.FinishedAt = formatTime(time.Now().UTC())
	if runErr == nil && contextErr == nil {
		writeEvent(log, masker, prefix, "passed")
		return result, nil
	}
	result.ExitCode = exitCode(runErr)
	result.Error = masker.Mask(errorText(runErr, contextErr))
	stepError := &StepError{Burn: burn, Step: step.Name, ExitCode: result.ExitCode, Err: errors.New(result.Error)}
	switch {
	case errors.Is(contextErr, context.DeadlineExceeded):
		result.Status = StepTimedOut
		stepError.TimedOut = true
		writeEvent(log, masker, prefix, fmt.Sprintf("timed out after %s", timeout))
	case contextErr != nil || ctx.Err() != nil:
		// Interruption remains an in-process run outcome, but the binding step
		// status vocabulary deliberately has no "interrupted" value.
		result.Status = StepFailed
		stepError.Interrupted = true
		writeEvent(log, masker, prefix, "interrupted")
	default:
		result.Status = StepFailed
		writeEvent(log, masker, prefix, fmt.Sprintf("failed with exit code %d", result.ExitCode))
	}
	return result, stepError
}

func resolveStepExecutable(command string, environment []string) (string, error) {
	if strings.ContainsRune(command, os.PathSeparator) {
		return command, nil
	}
	path, _ := environmentValue(environment, "PATH")
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, command)
		if err := executableFile(candidate); err != nil {
			continue
		}
		if !filepath.IsAbs(candidate) {
			return "", fmt.Errorf("resolve command %q from step PATH: %w", command, exec.ErrDot)
		}
		return candidate, nil
	}
	return "", fmt.Errorf("resolve command %q from step PATH: %w", command, exec.ErrNotFound)
}

func executableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return syscall.EISDIR
	}
	if err := syscall.Access(path, 1); err == nil {
		return nil
	} else if !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EPERM) {
		return err
	}
	if info.Mode()&0o111 == 0 {
		return os.ErrPermission
	}
	return nil
}

func environmentValue(environment []string, name string) (string, bool) {
	prefix := name + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix), true
		}
	}
	return "", false
}

func killProcessGroup(process *os.Process) error {
	if process == nil {
		return os.ErrProcessDone
	}
	return syscall.Kill(-process.Pid, syscall.SIGKILL)
}

func errorText(runErr, contextErr error) string {
	switch {
	case contextErr != nil && runErr != nil:
		return errors.Join(contextErr, runErr).Error()
	case contextErr != nil:
		return contextErr.Error()
	case runErr != nil:
		return runErr.Error()
	default:
		return "step failed"
	}
}

func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func formatCommand(command string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, strconv.Quote(command))
	for _, argument := range args {
		parts = append(parts, strconv.Quote(argument))
	}
	return strings.Join(parts, " ")
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

// setXPatterns detect shell tracing directives that can expose transformed
// secret values through the streaming redactor.
var setXPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bset\s+-[a-z]*x`),
	regexp.MustCompile(`\bset\s+-o\s+xtrace\b`),
	regexp.MustCompile(`\b(?:ba)?sh\s+-[a-z]*x\b`),
}

func stepContainsSetX(step periapsis.Step) bool {
	for _, pattern := range setXPatterns {
		if pattern.MatchString(step.Command) {
			return true
		}
		for _, arg := range step.Args {
			if pattern.MatchString(arg) {
				return true
			}
		}
	}
	return false
}

func writeEvent(log io.Writer, masker Masker, prefix, message string) {
	_, _ = fmt.Fprintf(log, "%s%s\n", prefix, masker.Mask(message))
}

func formatTime(value time.Time) string {
	return value.Format(time.RFC3339Nano)
}
