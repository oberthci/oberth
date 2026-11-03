package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRestartCutAdoptionPersistsRetryableWithoutTestIssue(t *testing.T) {
	for _, mode := range []string{"lost-pod", "lost-exit", "real-nonzero"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, fixture.repo.DefaultBranch, strings.Repeat("a", 40))
			fixture.setJobName(t, run.ID, "adopted-workflow")
			if _, err := fixture.store.MarkRunSubmitted(t.Context(), run.ID); err != nil {
				t.Fatal(err)
			}
			result := JobResult{Status: model.RunInterrupted, Phase: "interrupted", Error: "server restart interrupted build/test: no recorded process exit code", Steps: []model.StepResult{stepResult(model.StepFailed)}}
			if mode == "real-nonzero" {
				result.Status, result.Phase, result.Error = model.RunFailed, "test", "real test failed"
				result.FailedBurn, result.FailedStep = "test", "unit"
			}
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{waitResult: result, terminalErr: ErrJobNotTerminal}}
			scheduler := fixture.scheduler(t, jobs)
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("[test/unit] before server restart\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scheduler.Run(ctx); close(done) }()
			deadline := time.NewTimer(testStartTimeout(15 * time.Second))
			defer deadline.Stop()
			var current model.Run
		observe:
			for {
				select {
				case err := <-done:
					t.Fatalf("adoption stopped before finalization: %v", err)
				case <-deadline.C:
					t.Fatal("adopted run did not settle")
				case <-time.After(2 * time.Millisecond):
					current, err = fixture.store.Run(t.Context(), run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if current.Status.Terminal() {
						break observe
					}
				}
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(testStartTimeout(15 * time.Second)):
				t.Fatal("owner did not stop")
			}
			jobs.mu.Lock()
			created, releaseCreated, adopted := jobs.created, jobs.releaseCreated, len(jobs.adopted)
			jobs.mu.Unlock()
			if created != 0 || releaseCreated != 0 || adopted != 1 {
				t.Fatal("restart replayed execution", created, releaseCreated, adopted)
			}
			issue, issueErr := fixture.store.OpenCIIssue(t.Context(), fixture.repo.ID, run.Ref)
			retry, retryErr := fixture.store.RetryRun(t.Context(), model.RunRetryRequest{RunID: run.ID, Actor: "agent@host"})
			if mode == "real-nonzero" {
				if current.Status != model.RunFailed || current.FailedStep != "unit" || issueErr != nil || issue.ID == 0 || !errors.Is(retryErr, store.ErrInvalidState) {
					t.Fatalf("real failure became retryable: run=%+v issue=%+v/%v retry=%+v/%v", current, issue, issueErr, retry, retryErr)
				}
			} else {
				if current.Status != model.RunInterrupted || current.FailedStep != "" || !errors.Is(issueErr, store.ErrNotFound) || retryErr != nil || retry.ID == run.ID || retry.Status != model.RunQueued {
					t.Fatalf("restart remained CI red or unretryable: run=%+v issue=%+v/%v retry=%+v/%v", current, issue, issueErr, retry, retryErr)
				}
				shown := presentationRun(current)
				if shown.DisplayStatus != "cancelled" || shown.DisplayReason != "server restart, retry needed" {
					t.Fatal("restart lost amber presentation", shown)
				}
			}
		})
	}
}

type restartIndexFailure struct {
	LogStore
	tails atomic.Int64
}

func (l *restartIndexFailure) BuildIndex(string) (runlog.Index, error) {
	return runlog.Index{}, errors.New("index unreadable after restart")
}
func (l *restartIndexFailure) Tail(string, int64) ([]byte, error) {
	l.tails.Add(1)
	return nil, errors.New("historical tail unavailable")
}
func TestRestartCutLocalLogFailureUsesPinnedReobserve(t *testing.T) {
	for _, lateFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cut", true: "late-real-failure"}[lateFailure], func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, fixture.repo.DefaultBranch, strings.Repeat("a", 40))
			fixture.setJobName(t, run.ID, "adopted-workflow")
			if _, err := fixture.store.MarkRunSubmitted(t.Context(), run.ID); err != nil {
				t.Fatal(err)
			}
			run, err := fixture.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			cut := JobResult{Status: model.RunInterrupted, Phase: "interrupted", Error: "server restart cut"}
			var reads atomic.Int64
			cut.Reobserve = func(context.Context) (JobResult, error) {
				reads.Add(1)
				if lateFailure {
					return JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "late real failure"}, nil
				}
				return JobResult{Status: model.RunInterrupted, Phase: "interrupted", Error: "server restart cut"}, nil
			}
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{waitResult: cut, terminalResult: JobResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "unbound synthetic failure"}}}
			scheduler := fixture.scheduler(t, jobs)
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			logs := &restartIndexFailure{LogStore: scheduler.logs}
			scheduler.logs = logs
			// A direct resumed owner isolates the post-Wait local-index failure from
			// startup and proves that the capability is used even when Wait returned nil.
			reservation, err := scheduler.admission.adopt(run, "M", argoworkflow.VMIDemand{})
			if err != nil {
				t.Fatal(err)
			}
			if err := scheduler.resumeAdoptedRun(t.Context(), run, scheduler.requireMutation, reservation); err != nil {
				t.Fatal(err)
			}
			current, err := fixture.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 1 {
				t.Fatal("pinned re-observation bypassed", reads.Load())
			}
			if lateFailure {
				if current.Status != model.RunFailed || current.Error != "late real failure" || current.FailedStep != "unit" {
					t.Fatal("late real failure masked", current)
				}
			} else {
				if current.Status != model.RunInterrupted || current.Error != "server restart cut" || logs.tails.Load() != 0 {
					t.Fatal("log failure replaced cut verdict", current, logs.tails.Load())
				}
			}
		})
	}
}
