package service

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

// A fixture reports presence separately from the controller's nonterminal result.
type restartStateJobs struct {
	JobController
	found bool
}

func (restartStateJobs) WaitForWatchSync(ctx context.Context) error       { return ctx.Err() }
func (j restartStateJobs) Adopt(context.Context, model.Run) (bool, error) { return j.found, nil }
func (restartStateJobs) WorkflowNamespace() string                        { return "test-argo" }
func (j restartStateJobs) WaitResumed(ctx context.Context, name string, w io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	return j.Wait(ctx, name, w)
}

func TestRestartDuringAdoptionLeavesWorkflowForNextOwner(t *testing.T) {
	f := newRecoveryFixture(t)
	run := f.enqueueAndClaim(t, "feature/adopt-interrupted", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	f.setWorkflowName(t, run.ID, "original-workflow")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
	scheduler := f.scheduler(t, jobs)
	for attempt := range 2 {
		dispatch := func(model.Run) error { return nil }
		if attempt == 0 {
			dispatch = func(model.Run) error { return context.Canceled }
		}
		err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, dispatch)
		if (attempt == 0) != errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		current, err := f.store.Run(t.Context(), run.ID)
		if err != nil || current.Status != model.RunRunning || current.WorkflowName != "original-workflow" || current.ID != run.ID {
			t.Fatal(current, err)
		}
	}
	if jobs.created != 0 || len(jobs.deleted) != 0 || len(jobs.adopted) != 2 {
		t.Fatal("adoption replayed execution", jobs)
	}
}

func TestRestartAmbiguousCreateAdoptsLateWorkflowOrFailsConfirmedAbsence(t *testing.T) {
	for _, appears := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed-absence", true: "late-workflow"}[appears], func(t *testing.T) {
			f := newRecoveryFixture(t)
			run := recoveryWaitingRun(t, f, "feature/ambiguous", "original-workflow", "")
			jobs := &createRecoveryJobs{present: map[string]bool{}, adoptingJobs: adoptingJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitResult: JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}}}}
			scheduler := f.scheduler(t, jobs)
			now := time.Now()
			scheduler.createNow = func() time.Time { return now }
			if err := scheduler.recordWorkflowCreateBinding(run, run.WorkflowName); err != nil {
				t.Fatal(err)
			}
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = file.Close(); err != nil {
				t.Fatal(err)
			}
			if err = scheduler.logs.AppendStepProgress(run.ID, runprogress.Event{Version: runprogress.Version, Stage: "test", Step: "unit", Status: runprogress.StepRunning, StartedAt: &now}); err != nil {
				t.Fatal(err)
			}
			if err = scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, func(model.Run) error { t.Fatal("absent Workflow adopted"); return nil }); err != nil {
				t.Fatal(err)
			}
			scheduler.settleOneCreate(t.Context())
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil || current.Status != model.RunRunning || scheduler.pendingCreateCount() != 1 || jobs.settleCalls != 0 {
				t.Fatal("provisional absence was final", current, err)
			}
			jobs.present[run.ID] = appears
			now = now.Add(2 * createObservationWindow)
			scheduler.settleOneCreate(t.Context())
			if appears {
				select {
				case recovered := <-scheduler.recoveredWorkflows:
					if err := scheduler.resumeAdoptedRun(t.Context(), recovered.run, scheduler.requireMutation, recovered.reservation); err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("late Workflow was not adopted")
				}
			}
			current, err = f.store.Run(t.Context(), run.ID)
			want := model.RunFailed
			if appears {
				want = model.RunPassed
			}
			if err != nil || current.Status != want || current.ID != run.ID {
				t.Fatal(current, err)
			}
			if !appears && presentationRun(current).DisplayReason != model.ServerRestartReason {
				t.Fatal(current)
			}
			scheduler.submissions.mu.Lock()
			owners := len(scheduler.submissions.owners)
			scheduler.submissions.mu.Unlock()
			if owners != 0 {
				t.Fatal("completed recovery retained submission ownership", owners)
			}
			if jobs.settleCalls != 0 || jobs.created != 0 || len(jobs.deleted) != 0 || scheduler.pendingCreateCount() != 0 {
				t.Fatal("restart settled by execution/deletion", jobs)
			}
			if pending, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
				t.Fatal("adoption left a deletion obligation", pending, err)
			}
		})
	}
}

type restartLookupJobs struct {
	adoptingJobs
	lookup func(context.Context, model.Run) (bool, error)
}

func (j *restartLookupJobs) Adopt(ctx context.Context, run model.Run) (bool, error) {
	return j.lookup(ctx, run)
}

func prepareRestartJournal(t *testing.T, scheduler *Scheduler, run model.Run) {
	t.Helper()
	if err := scheduler.recordWorkflowCreateBinding(run, run.WorkflowName); err != nil {
		t.Fatal(err)
	}
	file, err := scheduler.logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := scheduler.logs.AppendStepProgress(run.ID, runprogress.Event{Version: runprogress.Version, Stage: "test", Step: "unit", Status: runprogress.StepRunning, StartedAt: &now}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartLateAdoptionSurvivesAnotherOwnerShutdown(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/late-adoption", "original-workflow", "")
	lookups := 0 // The startup read precedes the single create-observation worker.
	jobs := &restartLookupJobs{adoptingJobs: adoptingJobs{recoveryJobs: recoveryJobs{
		terminalErr: ErrJobNotTerminal, waitEntered: make(chan struct{}), waitBlocks: make(chan struct{}),
	}}, lookup: func(context.Context, model.Run) (bool, error) {
		lookups++
		return lookups > 1, nil
	}}
	scheduler := f.scheduler(t, jobs)
	prepareRestartJournal(t, scheduler, run)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("owner exited before late adoption: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("late Workflow did not reach the observer")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || current.WorkflowName != run.WorkflowName || len(jobs.deleted) != 0 || jobs.created != 0 {
		t.Fatal("late adoption was stopped or resubmitted", current, err)
	}
	// A second owner resumes the very same Workflow and keeps its real result.
	nextJobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{waitResult: JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}}}
	next := f.scheduler(t, nextJobs)
	if err := next.reconcileStrandedRunsWithGate(t.Context(), next.requireMutation, func(run model.Run) error {
		grant, err := next.submissions.adopt(run)
		if err != nil {
			return err
		}
		return next.resumeAdoptedRun(t.Context(), run, next.requireMutation, grant)
	}); err != nil {
		t.Fatal(err)
	}
	current, err = f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunPassed || nextJobs.created != 0 || len(nextJobs.deleted) != 0 {
		t.Fatal(current, err)
	}
	pending, err := f.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("adopted Workflow still has a deletion obligation", pending, err)
	}
}

func TestRestartCreateReadErrorBeyondWindowRetainsRun(t *testing.T) {
	f := newRecoveryFixture(t)
	run := recoveryWaitingRun(t, f, "feature/read-outage", "original-workflow", "")
	readErr := error(nil)
	jobs := &restartLookupJobs{lookup: func(context.Context, model.Run) (bool, error) { return false, readErr }}
	scheduler := f.scheduler(t, jobs)
	now := time.Now()
	scheduler.createNow = func() time.Time { return now }
	prepareRestartJournal(t, scheduler, run)
	if err := scheduler.reconcileStrandedRunsWithGate(t.Context(), scheduler.requireMutation, nil); err != nil {
		t.Fatal(err)
	}
	scheduler.settleOneCreate(t.Context())
	now = now.Add(2 * createObservationWindow)
	readErr = errors.New("cluster API unreachable")
	scheduler.settleOneCreate(t.Context())
	current, err := f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning || scheduler.pendingCreateCount() != 1 || len(jobs.deleted) != 0 || jobs.created != 0 {
		t.Fatal("outage was mistaken for absence", current, err)
	}
	readErr = nil
	now = now.Add(createObservationWindow)
	scheduler.settleOneCreate(t.Context())
	current, err = f.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunFailed || current.Error != model.ServerRestartReason || scheduler.pendingCreateCount() != 0 {
		t.Fatal("confirmed absence was not finalized", current, err)
	}
}
