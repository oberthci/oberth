package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/testtimeout"
)

type heldPatternLogs struct {
	LogStore
	entered chan struct{}
	release <-chan struct{}
}

func (logs heldPatternLogs) ReadFiltered(id, burn, step string, filter runlog.Filter) ([]byte, runlog.Meta, error) {
	if filter.Pattern != "" {
		logs.entered <- struct{}{}
		<-logs.release
	}
	return logs.LogStore.ReadFiltered(id, burn, step, filter)
}

func TestRunLogPatternReserveDoesNotBlockCIIssueCreation(t *testing.T) {
	// Two actual API readers stop inside their log backend, keeping both
	// interactive slots occupied throughout the issue-creation operation.
	output := "[test/unit] Error: user-requested log\n"
	logs := writeIndexedRunLog(t, "run-pattern-reserve", output)
	entered, unblock := make(chan struct{}, 2), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	reader := &API{logs: heldPatternLogs{LogStore: logs, entered: entered, release: unblock}}
	run := model.Run{ID: "run-pattern-reserve", Status: model.RunFailed}
	done := make(chan error, 2)
	for index := range 2 {
		go func() {
			ctx := withRunLogCaller(t.Context(), api.Actor{TokenCredentialID: string(rune('a' + index))})
			_, _, err := reader.readStepLogFiltered(ctx, run, "test", "unit", runlog.Filter{Pattern: "Error:"})
			done <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(testtimeout.Start(3 * time.Second)):
			t.Fatal("interactive scans did not start")
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	_, _, err := reader.readStepLogFiltered(ctx, run, "test", "unit", runlog.Filter{Pattern: "Error:"})
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || len(entered) != 0 {
		t.Fatalf("third pattern read ignored request budget: %v", err)
	}
	plain, _, err := reader.readStepLogFiltered(t.Context(), run, "test", "unit", runlog.Filter{})
	if err != nil || string(plain) != output {
		t.Fatalf("plain read unnecessarily queued: %q %v", plain, err)
	}

	fixture := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "security", FailedBurn: "security", FailedStep: "security", Error: "step failed", Steps: []model.StepResult{{Burn: "security", Step: "security", Status: model.StepFailed, ExitCode: 2}}})
	fixture.jobs.transcripts = []string{siblingInterleavedTranscript()}
	queued, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "pattern-reserve-ci", Repository: fixture.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	processCtx, cancelProcess := context.WithTimeout(t.Context(), testtimeout.Start(10*time.Second))
	defer cancelProcess()
	processed := make(chan error, 1)
	go func() { processed <- fixture.scheduler.ProcessNext(processCtx) }()
	select {
	case err := <-processed:
		if err != nil {
			t.Fatal(err)
		}
	case <-processCtx.Done():
		release()
		<-processed
		t.Fatal("CI issue creation waited behind interactive pattern scans")
	}
	issues, err := fixture.store.ListIssues(t.Context(), model.IssueListFilter{RepoID: fixture.repo.ID})
	if err != nil || len(issues.Issues) != 1 {
		t.Fatalf("CI issue not created: %+v %v", issues, err)
	}
	body := issues.Issues[0].Body
	for _, want := range []string{"fatal error: fault", "exit status 2", queued.ID} {
		if !strings.Contains(body, want) {
			t.Fatalf("CI issue lost excerpt/pointer %q: %s", want, body)
		}
	}
	// Normal interactive reads remain blocked until their own backend releases.
	select {
	case err := <-done:
		t.Fatalf("held scan returned early: %v", err)
	default:
	}
	release()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	bodyBytes, _, err := reader.readStepLogFiltered(t.Context(), run, "test", "unit", runlog.Filter{Pattern: "Error:"})
	if err != nil || string(bodyBytes) != output {
		t.Fatalf("interactive slot leaked: %q %v", bodyBytes, err)
	}
}
