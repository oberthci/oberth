package service

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
)

type replayAdoptingJobs struct {
	JobController
	adoptions atomic.Int32
}

func (j *replayAdoptingJobs) Adopt(context.Context, model.Run) (bool, error) {
	j.adoptions.Add(1)
	return true, nil
}
func (*replayAdoptingJobs) WorkflowNamespace() string { return "pipeline" }
func (j *replayAdoptingJobs) WaitResumed(ctx context.Context, name string, out io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	return j.Wait(ctx, name, out)
}

func TestDirectCreateReplayBeforeStartupKeepsItsLiveObserver(t *testing.T) {
	f := newGroupFixture(t, 0)
	jobs := &replayAdoptingJobs{JobController: f.scheduler.jobs}
	f.scheduler.jobs = jobs
	// Activation replays a pending receive before starting Scheduler.Run.
	accepted, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "activation-replay", Repository: f.terraform, Branch: "feature/replay", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	f.awaitCreated(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.scheduler.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	awaitSchedulerRecovered(t, f.scheduler)
	run, err := f.store.Run(t.Context(), accepted.ID)
	if err != nil || run.Status != model.RunRunning || jobs.adoptions.Load() != 0 {
		t.Fatalf("replay lost live ownership: %+v adoptions=%d err=%v", run, jobs.adoptions.Load(), err)
	}
	f.engine.finish(t, accepted.ID)
	if run := f.awaitTerminal(t, accepted.ID); run.Status != model.RunFailed || run.FailedStage != "test" {
		t.Fatalf("replay lost Argo verdict: %+v", run)
	}
}

type cancelBeforeStartStore struct{ *store.Store }

func (s *cancelBeforeStartStore) StartRun(ctx context.Context, id string) (model.Run, error) {
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: id, Actor: "owner"}); err != nil {
		return model.Run{}, err
	}
	return s.Store.StartRun(ctx, id)
}
func TestDirectCreateCancelBetweenInsertAndStartIsAcceptedAndEnded(t *testing.T) {
	f := newControlFixture(t)
	f.scheduler.store = &cancelBeforeStartStore{f.store}
	accepted, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "cancel-before-start", Repository: f.repo, Branch: "feature/cancel", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.Run(t.Context(), accepted.ID)
	if err != nil || run.Status != model.RunInterrupted || run.WorkflowName != "" {
		t.Fatalf("cancelled acceptance=%+v %v", run, err)
	}
}

type failedCreateWatchJobs struct {
	*fakeJobs
	watched   chan WorkflowRequest
	deadlines chan time.Time
}

func (j *failedCreateWatchJobs) CreateWorkflow(context.Context, WorkflowRequest) error {
	return &uncertainWorkflowCreateError{}
}
func (j *failedCreateWatchJobs) WatchFailedCreate(ctx context.Context, req WorkflowRequest) error {
	deadline, _ := ctx.Deadline()
	j.deadlines <- deadline
	j.watched <- req
	<-ctx.Done()
	return ctx.Err()
}
func TestDirectCreateFailedObservationIsBoundedAndRearmedAtStartup(t *testing.T) {
	f := newControlFixture(t)
	jobs := &failedCreateWatchJobs{fakeJobs: f.jobs, watched: make(chan WorkflowRequest, 2), deadlines: make(chan time.Time, 2)}
	f.scheduler.jobs = jobs
	accepted, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "late-create-restart", Repository: f.repo, Branch: "feature/late", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	for round := range 2 {
		select {
		case req := <-jobs.watched:
			if req.Run.ID != accepted.ID || req.WorkflowName == "" {
				t.Fatalf("lost failed create identity: %+v", req)
			}
			deadline := <-jobs.deadlines
			if deadline.IsZero() || time.Until(deadline) > createObservationWindow {
				t.Fatal("unbounded failed-create watch", deadline)
			}
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Fatalf("watch round %d not armed", round)
		}
		if round == 0 {
			f.scheduler.closeObservations()
			scheduler, err := NewScheduler(SchedulerConfig{Store: f.store, Git: f.git, Logs: f.logs, Jobs: jobs, Auditor: f.store, WorkspaceRoot: f.scheduler.workspaceRoot})
			if err != nil {
				t.Fatal(err)
			}
			defer scheduler.closeObservations()
			if err := scheduler.rearmFailedCreates(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	run, err := f.store.Run(t.Context(), accepted.ID)
	if err != nil || run.Status != model.RunFailed {
		t.Fatal(run, err)
	}
}

func TestDirectCreateQueuedDisplayAndFilterComeOnlyFromArgo(t *testing.T) {
	f := newControlFixture(t)
	preparing, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "display-preparing", Repository: f.repo, Branch: "feature/preparing", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "display-pending", Repository: f.repo, Branch: "feature/pending", SHA: strings.Repeat("b", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetRunWorkflowName(t.Context(), pending.ID, "pending-workflow"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.MarkRunSubmitted(t.Context(), pending.ID); err != nil {
		t.Fatal(err)
	}
	control := f.api(t)
	control.schedulingObserver = schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
		return &model.ExecutionObservation{Phase: "Pending", State: "observed", Message: "waiting for Argo"}, nil
	})
	for status, want := range map[string]string{model.DisplayQueued: pending.ID, model.DisplayRunning: preparing.ID} {
		result, err := control.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{Status: model.RunStatusFilter{status}})
		if err != nil {
			t.Fatal(err)
		}
		rows := result.([]model.Run)
		if len(rows) != 1 || rows[0].ID != want || rows[0].DisplayStatus != status {
			t.Fatalf("%s filter=%+v", status, rows)
		}
	}
}

type disconnectAfterInsertStore struct {
	*store.Store
	disconnect context.CancelFunc
}

func (s *disconnectAfterInsertStore) EnqueueReceiveEvent(ctx context.Context, event model.ReceiveEventSpec, spec model.RunSpec) (model.EnqueueRunResult, error) {
	result, err := s.Store.EnqueueReceiveEvent(ctx, event, spec)
	if err == nil {
		s.disconnect()
	}
	return result, err
}
func TestDirectCreateDisconnectImmediatelyAfterInsertKeepsAcceptance(t *testing.T) {
	f := newGroupFixture(t, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.scheduler.store = &disconnectAfterInsertStore{Store: f.store, disconnect: cancel}
	accepted, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "disconnect-after-insert", Repository: f.terraform, Branch: "feature/cut", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil || accepted.ID == "" {
		t.Fatalf("lost committed acceptance: %+v %v", accepted, err)
	}
	f.awaitCreated(t, 1)
	f.engine.finish(t, accepted.ID)
	f.awaitTerminal(t, accepted.ID)
}
