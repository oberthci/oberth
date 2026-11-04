package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

// Model the watchdog's progress -> Cancel -> observation failure -> Delete
// path: no failed step or terminal step result is emitted for the hung suite.
type hungSuiteJobs struct {
	recoveryJobs
	started   bool
	cancelled bool
}

func (j *hungSuiteJobs) Wait(_ context.Context, _ string, out io.Writer) (JobResult, error) {
	now := time.Now()
	event := runprogress.Event{Version: 1, Stage: "ci", Step: "unit", DeclaredSize: "M", Status: runprogress.StepQueued}
	if j.started {
		event.Status, event.StartedAt = runprogress.StepRunning, &now
	}
	marker, err := runprogress.MarshalMarker(event)
	if err != nil {
		return JobResult{}, err
	}
	if _, err = out.Write(marker); err != nil {
		return JobResult{}, err
	}
	j.cancelled = true
	return JobResult{}, fmt.Errorf("%s Server watchdog expired: %w", model.ExecutionDeadlineMessage, context.DeadlineExceeded)
}
func (j *hungSuiteJobs) Delete(ctx context.Context, name, runID string) error {
	if !j.cancelled {
		return errors.New("Delete preceded watchdog Cancel")
	}
	return j.recoveryJobs.Delete(ctx, name, runID)
}

func TestInfrastructureRetryWatchdogDoesNotReplayHungSuite(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			f := newRecoveryFixture(t)
			source := f.enqueueAndClaim(t, "feature/watchdog", strings.Repeat("a", 40))
			jobs := &hungSuiteJobs{started: started, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
			scheduler := f.scheduler(t, jobs)
			if err := scheduler.executeAndCleanup(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			failed, err := f.store.Run(t.Context(), source.ID)
			if err != nil || failed.Status != model.RunFailed || failed.Phase != "job" || failed.FailedStep != "" || !jobs.cancelled || len(jobs.deleted) != 1 {
				t.Fatalf("watchdog did not follow cleanup path: %+v cancelled=%v deleted=%v err=%v", failed, jobs.cancelled, jobs.deleted, err)
			}
			if (failed.AutoRetriedBy != "") == started {
				t.Fatalf("watchdog retry: started=%v failed=%+v", started, failed)
			}
			runs, err := f.store.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: f.repo.ID, Limit: 10})
			want := 2
			if started {
				want = 1
			}
			if err != nil || len(runs) != want {
				t.Fatalf("watchdog attempts=%d want=%d err=%v", len(runs), want, err)
			}
		})
	}
}

func TestInfrastructureRetryExecutionDeadlineUsesEveryStartEvidence(t *testing.T) {
	for _, evidence := range []string{"empty", "queued", "history-running", "history-passed", "history-start-then-queued", "result-started", "result-logs", "result-running", "result-passed", "observed-started", "observed-running", "observed-passed", "earlier-attempt"} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/deadline=%v", evidence, deadline), func(t *testing.T) {
				logs, err := runlog.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				run := model.Run{ID: strings.Repeat("b", 32), RefKind: model.RefBranch, Trigger: "branch"}
				if err = logs.InitializeStepProgress(run.ID); err != nil {
					t.Fatal(err)
				}
				result := JobResult{Status: model.RunFailed, Phase: "job", Error: "workflow controller unavailable"}
				if deadline {
					result.Error = model.ExecutionDeadlineMessage
				}
				now := time.Now()
				switch evidence {
				case "queued", "history-running", "history-passed", "history-start-then-queued":
					event := runprogress.Event{Version: 1, Stage: "ci", Step: "unit", DeclaredSize: "M", Status: runprogress.StepQueued}
					if evidence != "queued" {
						event.Status, event.StartedAt = runprogress.StepRunning, &now
					}
					if evidence == "history-passed" {
						event.Status, event.FinishedAt = runprogress.StepPassed, &now
					}
					if err = logs.AppendStepProgress(run.ID, event); err != nil {
						t.Fatal(err)
					}
					if evidence == "history-start-then-queued" {
						event.Status, event.StartedAt = runprogress.StepQueued, nil
						if err = logs.AppendStepProgress(run.ID, event); err != nil {
							t.Fatal(err)
						}
					}
				case "result-started":
					result.Steps = []model.StepResult{{Status: model.StepSkipped, StartedAt: &now}}
				case "result-logs":
					result.Steps = []model.StepResult{{Status: model.StepSkipped, LogEnd: 42}}
				case "result-running":
					result.Steps = []model.StepResult{{Status: model.StepRunning}}
				case "result-passed":
					result.Steps = []model.StepResult{{Status: model.StepPassed}}
				case "observed-started":
					result.TerminalObservation = &model.RunTerminalObservation{Steps: []model.RunObservedStep{{Started: true}}}
				case "observed-running":
					result.TerminalObservation = &model.RunTerminalObservation{Steps: []model.RunObservedStep{{Status: model.StepRunning}}}
				case "observed-passed":
					result.TerminalObservation = &model.RunTerminalObservation{Steps: []model.RunObservedStep{{Status: model.StepPassed}}}
				case "earlier-attempt":
					result.ObservedStepStart = true
				}
				want := !deadline || evidence == "empty" || evidence == "queued"
				if got := (&Scheduler{logs: logs}).infrastructureAutoRetry(run, result); got != want {
					t.Fatalf("retry=%v want=%v", got, want)
				}
			})
		}
	}
}

func TestManualRetryOfAutomaticAttemptHasManualPresentation(t *testing.T) {
	f := newRecoveryFixture(t)
	source := f.enqueueAndClaim(t, "feature/manual-after-auto", strings.Repeat("c", 40))
	failed, err := f.store.FinishRun(t.Context(), source.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "executor unavailable", AutoRetryInfrastructure: true})
	if err != nil || failed.AutoRetriedBy == "" {
		t.Fatal(failed, err)
	}
	if _, err = f.store.FinishRun(t.Context(), failed.AutoRetriedBy, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: "executor unavailable"}); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.RetryRun(t.Context(), model.RunRetryRequest{RunID: failed.AutoRetriedBy, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.store.Run(t.Context(), next.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []model.Run{next.Run, saved} {
		view := presentationRun(run)
		if run.AutoRetryOf != "" || run.AutoRetriedBy != "" || strings.Contains(run.Reason, "automatic retry") || strings.Contains(view.DisplayReason, "automatic retry") || run.Actor != "owner" {
			t.Fatalf("manual retry presented as automatic: %+v", view)
		}
	}
	audit, err := f.store.LatestAuditActionByType(t.Context(), "run.retry", "run")
	if err != nil || audit.Actor != "owner" || audit.ResourceID != next.ID || !strings.Contains(audit.Details, failed.AutoRetriedBy) {
		t.Fatal("manual retry lost audit", audit, err)
	}
}
