package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

// siblingInterleavedTranscript reproduces #656's shape: the `security` DAG leaf
// crashes early (a Trivy "fatal error: fault" followed by a long goroutine
// dump), while the `test` and `build-amd64` leaves keep logging for far longer
// than the CI-issue budget afterwards. The combined-log tail therefore holds no
// security line at all.
func siblingInterleavedTranscript() string {
	var log strings.Builder
	log.WriteString("[setup/setup] tools ready\n")
	log.WriteString("[security/security] 2026-09-28T19:00:00Z INFO scanning go.mod\n")
	for index := range 3 {
		fmt.Fprintf(&log, "[test/test] === RUN TestEarly%d\n", index)
	}
	log.WriteString("[security/security] fatal error: fault\n")
	log.WriteString("[security/security] [signal SIGSEGV: segmentation violation code=0x1 addr=0x28 pc=0x1a2b3c]\n")
	for index := range 600 {
		fmt.Fprintf(&log, "[security/security] go.etcd.io/bbolt.(*Cursor).search(0xc000%06d, ...)\n", index)
		if index%50 == 0 {
			fmt.Fprintf(&log, "[test/test] --- PASS: TestInterleaved%d (0.01s)\n", index)
		}
	}
	log.WriteString("[security/security] exit status 2\n")
	for index := range 1000 {
		fmt.Fprintf(&log, "[test/test] === RUN TestLate%04d\n", index)
		fmt.Fprintf(&log, "[build-amd64/build-amd64] compiling package %04d\n", index)
	}
	log.WriteString("[test/test] ok  \tgithub.com/oberthci/oberth/internal/store\t12.3s\n")
	return log.String()
}

// TestCIIssueExcerptIsTheFailedStepSliceNotSiblingTail is the #656
// acceptance: a DAG leaf fails while its siblings continue logging, and the CI
// issue excerpt contains only the failed step's own prefixed lines, including
// its final error lines and the earlier root-cause line that scrolled out of
// the tail window.
func TestCIIssueExcerptIsTheFailedStepSliceNotSiblingTail(t *testing.T) {
	fixture := newControlFixture(t, JobResult{
		Status: model.RunFailed, Phase: "security", FailedBurn: "security", FailedStep: "security",
		Error: "step security/security failed with exit code 2",
		Steps: []model.StepResult{
			{Burn: "security", Step: "security", Status: model.StepFailed, ExitCode: 2},
			{Burn: "test", Step: "test", Status: model.StepPassed},
			{Burn: "build-amd64", Step: "build-amd64", Status: model.StepPassed},
		},
	})
	transcript := siblingInterleavedTranscript()
	fixture.jobs.transcripts = []string{transcript}
	// The pre-fix behaviour, for contrast: the combined tail has no security line.
	if combined := transcript[len(transcript)-maximumCIIssueTailBytes:]; strings.Contains(combined, "[security/security]") {
		t.Fatal("fixture does not reproduce #656: the combined tail already shows the failed step")
	}

	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "receive-656", Repository: fixture.repo, Branch: "campaign-0928-publishers",
		SHA: "7c496a5d4043785ffe2593d25eff3d434e8fd041", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := fixture.store.ListIssues(ctx, model.IssueListFilter{RepoID: fixture.repo.ID})
	if err != nil || len(page.Issues) != 1 {
		t.Fatalf("issues = %#v, %v", page, err)
	}
	body := page.Issues[0].Body
	if !strings.Contains(body, "failed: security / security") ||
		!strings.Contains(body, "full step log: run_logs "+enqueued.ID+" security security") {
		t.Fatalf("CI issue lost its failed-step header or full-log pointer:\n%s", body)
	}
	securityLines := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if !strings.HasPrefix(line, "[security/security] ") {
			t.Fatalf("CI issue excerpt contains a line from another step: %q", line)
		}
		securityLines++
	}
	if securityLines == 0 {
		t.Fatalf("CI issue excerpt contains no line of the failed step:\n%s", body)
	}
	for _, want := range []string{
		"[security/security] exit status 2\n",
		"[security/security] fatal error: fault\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("CI issue excerpt misses %q:\n%s", strings.TrimSpace(want), body)
		}
	}
	if strings.Index(body, "fatal error: fault") > strings.Index(body, "exit status 2") {
		t.Fatal("earlier diagnostic line must precede the step's tail")
	}
}

func writeIndexedRunLog(t *testing.T, runID, content string) *runlog.Store {
	t.Helper()
	logs, err := runlog.Open(filepath.Join(t.TempDir(), "logs"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := logs.Create(runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.BuildIndex(runID); err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestStepFailureExcerptBoundsDiagnosticsAndTail(t *testing.T) {
	t.Parallel()
	logs := writeIndexedRunLog(t, "run-656-budget", siblingInterleavedTranscript())
	for _, budget := range []int64{512, 4 << 10, maximumCIIssueTailBytes} {
		excerpt := stepFailureExcerpt(logs, "run-656-budget", "security", "security", budget)
		if int64(len(excerpt)) > budget {
			t.Fatalf("budget %d: excerpt is %d bytes", budget, len(excerpt))
		}
		text := string(excerpt)
		if !strings.HasSuffix(text, "[security/security] exit status 2\n") {
			t.Fatalf("budget %d: excerpt does not end with the step's final line:\n%s", budget, text)
		}
		if !strings.Contains(text, "[security/security] fatal error: fault\n") {
			t.Fatalf("budget %d: excerpt dropped the earlier root-cause line:\n%s", budget, text)
		}
		diagnostics, tail, found := strings.Cut(text, "\n\n")
		if !found || strings.Contains(tail, "fatal error: fault") {
			t.Fatalf("budget %d: earlier diagnostics are not separated from the tail:\n%s", budget, text)
		}
		for _, line := range strings.Split(strings.TrimSpace(diagnostics+"\n"+tail), "\n") {
			if !strings.HasPrefix(line, "[security/security] ") {
				t.Fatalf("budget %d: foreign line %q", budget, line)
			}
		}
	}
}

func TestStepFailureExcerptReturnsWholeSmallSliceVerbatim(t *testing.T) {
	t.Parallel()
	content := "[lint/vet] ok\n[test/unit] --- FAIL: TestSomething\n[lint/vet] still going\n[test/unit] Error: expected 1, got 2\n[lint/vet] done\n"
	logs := writeIndexedRunLog(t, "run-656-small", content)
	got := string(stepFailureExcerpt(logs, "run-656-small", "test", "unit", maximumCIIssueTailBytes))
	want := "[test/unit] --- FAIL: TestSomething\n[test/unit] Error: expected 1, got 2\n"
	if got != want {
		t.Fatalf("excerpt = %q, want %q", got, want)
	}
}

func TestFailureExcerptNeverSubstitutesAnotherStepsOutput(t *testing.T) {
	t.Parallel()
	content := "[build-amd64/build-amd64] compiled\n[test/unit] PASS\n"
	logs := writeIndexedRunLog(t, "run-656-silent", content)
	scheduler := &Scheduler{logs: logs}

	// The failed step never wrote a line: no excerpt, rather than sibling output.
	excerpt, err := scheduler.failureExcerpt("run-656-silent", "security", "security")
	if err != nil || len(excerpt) != 0 {
		t.Fatalf("silent failed step excerpt = %q, %v; want empty", excerpt, err)
	}
	// A Job-level failure without a failed step keeps the combined tail.
	excerpt, err = scheduler.failureExcerpt("run-656-silent", "", "")
	if err != nil || string(excerpt) != content {
		t.Fatalf("job-level failure excerpt = %q, %v; want the combined tail", excerpt, err)
	}
}
