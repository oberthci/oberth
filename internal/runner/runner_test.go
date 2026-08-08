package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestRunnerOrdersStepsPrefixesOutputAndEmitsSummary(t *testing.T) {
	dir := t.TempDir()
	pipeline := periapsis.Pipeline{Burns: []periapsis.Burn{
		{Name: "test", Type: periapsis.Retrograde, DependsOn: []string{"lint"}, Steps: []periapsis.Step{{Name: "test", Command: "sh", Args: []string{"-c", "printf test >> order; printf 'test output\\n'"}}}},
		{Name: "lint", Type: periapsis.Retrograde, Steps: []periapsis.Step{{Name: "lint", Command: "sh", Args: []string{"-c", "printf lint- >> order; printf 'lint output\\n'"}}}},
	}}
	var log, termination bytes.Buffer
	summary, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Second, SummaryWriter: &termination}).Run(context.Background(), pipeline, dir, &log)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "order"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "lint-test" {
		t.Fatalf("execution order = %q", raw)
	}
	for _, line := range strings.Split(strings.TrimSpace(log.String()), "\n") {
		if !strings.HasPrefix(line, "[lint/lint]") && !strings.HasPrefix(line, "[test/test]") {
			t.Fatalf("unprefixed log line %q in:\n%s", line, log.String())
		}
	}
	if summary.Status != StatusPassed || len(summary.Steps) != 2 {
		t.Fatalf("summary = %#v", summary)
	}
	var emitted []StepResult
	if err := json.Unmarshal(termination.Bytes(), &emitted); err != nil {
		t.Fatalf("termination summary is invalid JSON: %v: %s", err, termination.String())
	}
	if len(emitted) != 2 || emitted[0].Burn != "lint" || emitted[1].Burn != "test" {
		t.Fatalf("emitted summary = %#v", emitted)
	}
}

func TestRunnerEmitsCarriageReturnProgressBeforeStepExit(t *testing.T) {
	pipeline := singleStepPipeline(periapsis.Step{
		Name: "fetch", Command: "sh", Args: []string{"-c", "printf 'download 10%%\\r'; sleep 30"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := newNotifyingLog()
	finished := make(chan error, 1)
	go func() {
		_, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Minute}).Run(ctx, pipeline, t.TempDir(), log)
		finished <- err
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for !strings.Contains(log.String(), "[test/fetch] download 10%\n") {
		select {
		case <-log.changed:
		case err := <-finished:
			t.Fatalf("step exited before progress was observable: %v; log=%q", err, log.String())
		case <-deadline.C:
			cancel()
			<-finished
			t.Fatalf("carriage-return progress was not observable before step exit: %q", log.String())
		}
	}
	select {
	case err := <-finished:
		t.Fatalf("step exited before cancellation: %v", err)
	default:
	}
	cancel()
	if err := <-finished; err == nil {
		t.Fatal("canceled step returned no error")
	}
}

type notifyingLog struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	changed chan struct{}
}

func newNotifyingLog() *notifyingLog {
	return &notifyingLog{changed: make(chan struct{}, 1)}
}

func (log *notifyingLog) Write(value []byte) (int, error) {
	log.mu.Lock()
	written, err := log.buffer.Write(value)
	log.mu.Unlock()
	select {
	case log.changed <- struct{}{}:
	default:
	}
	return written, err
}

func (log *notifyingLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.buffer.String()
}

func TestRunnerResolvesBareCommandFromStepPATH(t *testing.T) {
	dir := t.TempDir()
	ambientBin := t.TempDir()
	repositoryBin := t.TempDir()
	const commandName = "oberth-path-contract"
	writeTestExecutable(t, filepath.Join(ambientBin, commandName), "#!/bin/sh\nprintf ambient > selected\n")
	writeTestExecutable(t, filepath.Join(repositoryBin, commandName), "#!/bin/sh\nprintf repository > selected\n")
	t.Setenv("PATH", ambientBin+string(os.PathListSeparator)+"/usr/bin:/bin")

	pipeline := singleStepPipeline(periapsis.Step{
		Name:    "path-contract",
		Command: commandName,
		Env:     map[string]string{"PATH": repositoryBin + string(os.PathListSeparator) + "/usr/bin:/bin"},
	})
	if _, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "selected"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "repository" {
		t.Fatalf("bare command resolved to %q executable, want repository step PATH", got)
	}
}

func TestRunnerFailsFast(t *testing.T) {
	dir := t.TempDir()
	pipeline := periapsis.Pipeline{Burns: []periapsis.Burn{{
		Name: "test", Type: periapsis.Retrograde,
		Steps: []periapsis.Step{
			{Name: "fails", Command: "sh", Args: []string{"-c", "exit 7"}},
			{Name: "must-not-run", Command: "sh", Args: []string{"-c", "touch marker"}},
		},
	}, {
		Name: "package", Type: periapsis.Prograde, DependsOn: []string{"test"},
		Steps: []periapsis.Step{{Name: "also-must-not-run", Command: "sh", Args: []string{"-c", "touch second-marker"}}},
	}}}
	summary, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{})
	var stepErr *StepError
	if !errors.As(err, &stepErr) || stepErr.ExitCode != 7 {
		t.Fatalf("error = %#v", err)
	}
	if len(summary.Steps) != 3 || summary.Steps[0].Status != StepFailed || summary.Steps[1].Status != StepSkipped || summary.Steps[2].Status != StepSkipped || summary.Status != StatusFailed {
		t.Fatalf("summary = %#v", summary)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later step ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "second-marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("later burn ran: %v", err)
	}
}

func TestRunnerTimeoutKillsDescendantProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pipeline := singleStepPipeline(periapsis.Step{
		Name: "slow", Command: "sh", Timeout: 80 * time.Millisecond,
		Args: []string{"-c", "sleep 30 & child=$!; printf '%s' \"$child\" > child.pid; wait"},
	})
	summary, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{})
	var stepErr *StepError
	if !errors.As(err, &stepErr) || !stepErr.TimedOut {
		t.Fatalf("error = %#v", err)
	}
	if summary.Steps[0].Status != StepTimedOut {
		t.Fatalf("step = %#v", summary.Steps[0])
	}
	pid := readPID(t, filepath.Join(dir, "child.pid"))
	waitProcessGone(t, pid)
}

func TestRunnerNonzeroExitRetiresBackgroundProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pipeline := singleStepPipeline(periapsis.Step{
		Name: "failed-publisher", Command: "sh",
		Args: []string{"-c", "sleep 30 & child=$!; printf '%s' \"$child\" > child.pid; exit 7"},
	})
	started := time.Now()
	summary, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: 5 * time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("failed step with background child returned after %s", elapsed)
	}
	var stepErr *StepError
	if !errors.As(err, &stepErr) || stepErr.ExitCode != 7 {
		t.Fatalf("error = %#v, want preserved exit 7", err)
	}
	if len(summary.Steps) != 1 || summary.Steps[0].Status != StepFailed || summary.Steps[0].ExitCode != 7 {
		t.Fatalf("summary = %#v", summary)
	}
	pid := readPID(t, filepath.Join(dir, "child.pid"))
	waitProcessGone(t, pid)
}

func TestRunnerTimeoutDoesNotHangOnEscapedStdoutGrandchild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Fatalf("setsid is required by the runner contract: %v", err)
	}
	dir := t.TempDir()
	pipeline := singleStepPipeline(periapsis.Step{
		Name: "escape", Command: "sh", Timeout: 80 * time.Millisecond,
		Args: []string{"-c", `setsid sh -c 'printf %s "$$" > escaped.pid; sleep 30' & while [ ! -s escaped.pid ]; do sleep 0.01; done; wait`},
	})
	started := time.Now()
	summary, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timed-out step returned after %s", elapsed)
	}
	var stepErr *StepError
	if !errors.As(err, &stepErr) || !stepErr.TimedOut || stepErr.Interrupted {
		t.Fatalf("error = %#v", err)
	}
	if len(summary.Steps) != 1 || summary.Steps[0].Status != StepTimedOut {
		t.Fatalf("summary = %#v", summary)
	}
	escapedPID := readPID(t, filepath.Join(dir, "escaped.pid"))
	_ = syscall.Kill(-escapedPID, syscall.SIGKILL)
	waitProcessGone(t, escapedPID)
}

func TestRunnerCancellationKillsDescendants(t *testing.T) {
	dir := t.TempDir()
	pipeline := singleStepPipeline(periapsis.Step{
		Name: "cancel", Command: "sh",
		Args: []string{"-c", "sleep 30 & child=$!; printf '%s' \"$child\" > child.pid; touch started; wait"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	var termination bytes.Buffer
	result := make(chan struct {
		summary Summary
		err     error
	}, 1)
	go func() {
		summary, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Minute, SummaryWriter: &termination}).Run(ctx, pipeline, dir, &bytes.Buffer{})
		result <- struct {
			summary Summary
			err     error
		}{summary: summary, err: err}
	}()
	waitForFile(t, filepath.Join(dir, "started"))
	cancel()
	got := <-result
	var stepErr *StepError
	if !errors.As(got.err, &stepErr) || !stepErr.Interrupted {
		t.Fatalf("error = %#v", got.err)
	}
	if got.summary.Status != StatusInterrupted || got.summary.Steps[0].Status != StepFailed {
		t.Fatalf("summary = %#v", got.summary)
	}
	var emitted []StepResult
	if err := json.Unmarshal(termination.Bytes(), &emitted); err != nil {
		t.Fatalf("termination summary is invalid JSON: %v: %s", err, termination.String())
	}
	if len(emitted) != 1 || emitted[0].Status != StepFailed {
		t.Fatalf("termination steps = %#v", emitted)
	}
	pid := readPID(t, filepath.Join(dir, "child.pid"))
	waitProcessGone(t, pid)
}

func TestRunnerMasksSecretsInCommandsOutputErrorsAndSummary(t *testing.T) {
	const secret = "release-token-value"
	pipeline := singleStepPipeline(periapsis.Step{
		Name: "publish", Command: "sh",
		Args: []string{"-c", "printf '%s\\n' 'release-token-value'; printf '%s\\n' 'release-token-value' >&2; exit 9"},
	})
	var log, termination bytes.Buffer
	summary, err := (Runner{
		Trigger:       periapsis.TriggerCI,
		StepTimeout:   time.Second,
		Secrets:       []string{secret},
		SummaryWriter: &termination,
	}).Run(context.Background(), pipeline, t.TempDir(), &log)
	if err == nil {
		t.Fatal("expected failure")
	}
	for name, value := range map[string]string{
		"log": log.String(), "error": err.Error(), "summary": fmt.Sprintf("%#v", summary), "termination": termination.String(),
	} {
		if strings.Contains(value, secret) {
			t.Fatalf("%s leaked secret: %s", name, value)
		}
	}
	if !strings.Contains(log.String(), MaskReplacement) {
		t.Fatalf("log does not contain mask marker: %s", log.String())
	}
}

func TestRunnerSelectsCIOrReleaseBurns(t *testing.T) {
	pipeline := periapsis.New().
		Retrograde("ci", periapsis.Step{Name: "ci", Command: "sh", Args: []string{"-c", "printf ci >> selected"}}).
		Release("release", periapsis.Step{Name: "release", Command: "sh", Args: []string{"-c", "printf release >> selected"}}).DependsOn("ci").
		Build()
	dir := t.TempDir()
	if _, err := (Runner{Trigger: periapsis.TriggerCI, StepTimeout: time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{Trigger: periapsis.TriggerRelease, StepTimeout: time.Second}).Run(context.Background(), pipeline, dir, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "selected"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "cirelease" {
		t.Fatalf("selected output = %q", raw)
	}
}

func TestMarshalSummaryRetainsCompleteExactBudgetInventory(t *testing.T) {
	steps := terminationBudgetResults()
	summary := Summary{Version: SummaryVersion, Status: StatusFailed, Steps: steps}
	raw, err := MarshalSummary(summary)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != MaxTerminationSummaryBytes {
		t.Fatalf("summary length = %d, want %d", len(raw), MaxTerminationSummaryBytes)
	}
	if len(raw) == 0 || raw[0] != '[' {
		t.Fatalf("termination payload is not an array: %s", raw)
	}
	var emitted []StepResult
	if err := json.Unmarshal(raw, &emitted); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(emitted) != len(steps) {
		t.Fatalf("emitted %d of %d admitted results", len(emitted), len(steps))
	}
	for index := range steps {
		if emitted[index] != steps[index] {
			t.Fatalf("result %d changed: got %#v, want %#v", index, emitted[index], steps[index])
		}
	}
}

func TestMarshalSummaryRejectsOversizeWithoutDroppingResults(t *testing.T) {
	steps := terminationBudgetResults()
	steps[len(steps)-1].Step += "x"
	_, err := MarshalSummary(Summary{Version: SummaryVersion, Status: StatusFailed, Steps: steps})
	if err == nil || !strings.Contains(err.Error(), "requires 4097 bytes, maximum is 4096") {
		t.Fatalf("MarshalSummary() error = %v", err)
	}
}

func TestMarshalSummaryUsesExactStepResultArrayWireShape(t *testing.T) {
	summary := Summary{Version: SummaryVersion, Trigger: "ci", Status: StatusFailed, Error: "overall failure", Steps: []StepResult{{
		Burn: "test", Step: "go-test", Status: StepTimedOut, ExitCode: -1,
		Error: "internal diagnostic", StartedAt: "2026-08-04T10:00:00Z", FinishedAt: "2026-08-04T10:30:00Z",
	}}}
	raw, err := MarshalSummary(summary)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"burn":"test","step":"go-test","status":"timed_out","exit_code":-1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:30:00Z"}]`
	if string(raw) != want {
		t.Fatalf("termination payload = %s\nwant = %s", raw, want)
	}
}

func TestDecodeStepResultsAcceptsOnlyBindingStatuses(t *testing.T) {
	raw := `[{
		"burn":"test","step":"passed","status":"passed","exit_code":0,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"
	},{
		"burn":"test","step":"failed","status":"failed","exit_code":1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"
	},{
		"burn":"test","step":"skipped","status":"skipped","exit_code":-1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"
	},{
		"burn":"test","step":"timed-out","status":"timed_out","exit_code":-1,"started_at":"2026-08-04T10:00:00Z","finished_at":"2026-08-04T10:00:01Z"
	}]`
	results, err := DecodeStepResults([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 4 || results[0].Status != StepPassed || results[1].Status != StepFailed || results[2].Status != StepSkipped || results[3].Status != StepTimedOut {
		t.Fatalf("results = %#v", results)
	}
	if _, err := DecodeStepResults([]byte(raw + `{}`)); err == nil {
		t.Fatal("decoder accepted trailing JSON")
	}
}

func TestStepContainsSetXDetectsTracing(t *testing.T) {
	cases := []struct {
		name string
		step periapsis.Step
		want bool
	}{
		{"literal set -x in arg", periapsis.Step{Command: "sh", Args: []string{"-c", "set -x; make build"}}, true},
		{"set -ex", periapsis.Step{Command: "sh", Args: []string{"-c", "set -ex; go test ./..."}}, true},
		{"set -eux", periapsis.Step{Command: "sh", Args: []string{"-c", "set -eux; cargo build"}}, true},
		{"set -euxo pipefail", periapsis.Step{Command: "bash", Args: []string{"-c", "set -euxo pipefail; npm install"}}, true},
		{"set -o xtrace", periapsis.Step{Command: "sh", Args: []string{"-c", "set -o xtrace; ls"}}, true},
		{"bash -x", periapsis.Step{Command: "sh", Args: []string{"-c", "bash -x build.sh"}}, true},
		{"sh -x", periapsis.Step{Command: "sh", Args: []string{"-c", "sh -x deploy.sh"}}, true},
		{"set -x in command field", periapsis.Step{Command: "set -x && echo hi"}, true},
		{"no tracing set -e only", periapsis.Step{Command: "sh", Args: []string{"-c", "set -e; make"}}, false},
		{"no tracing set -eu", periapsis.Step{Command: "sh", Args: []string{"-c", "set -eu; go build"}}, false},
		{"no tracing clean script", periapsis.Step{Command: "go", Args: []string{"test", "./..."}}, false},
		{"no tracing reset word boundary", periapsis.Step{Command: "sh", Args: []string{"-c", "reset -x something"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepContainsSetX(tc.step); got != tc.want {
				t.Fatalf("stepContainsSetX() = %v, want %v", got, tc.want)
			}
		})
	}
}

func singleStepPipeline(step periapsis.Step) periapsis.Pipeline {
	return periapsis.Pipeline{Burns: []periapsis.Burn{{Name: "test", Type: periapsis.Retrograde, Steps: []periapsis.Step{step}}}}
}

func terminationBudgetResults() []StepResult {
	const timestamp = "9999-12-31T23:59:59.999999999Z"
	results := make([]StepResult, 20)
	for index := range results {
		nameBytes := 52
		if index == len(results)-1 {
			nameBytes = 47
		}
		suffix := fmt.Sprintf("%02d", index)
		results[index] = StepResult{
			Burn:       "b",
			Step:       strings.Repeat("s", nameBytes-len(suffix)) + suffix,
			Status:     StepTimedOut,
			ExitCode:   255,
			StartedAt:  timestamp,
			FinishedAt: timestamp,
		}
	}
	return results
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}
	return pid
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("file %s was not created", path)
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) || processIsZombie(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived process-group termination", pid)
}

func processIsZombie(pid int) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(raw))
	return len(fields) > 2 && fields[2] == "Z"
}
