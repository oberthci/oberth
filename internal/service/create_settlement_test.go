package service

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
)

type pendingCreateJobs struct {
	*fakeJobs
	createHook func(context.Context, WorkflowRequest) error
	settle     func(context.Context, string, string, time.Time) (bool, error)
	adoptCalls atomic.Int32
	adoptHook  func(context.Context, model.Run) (bool, error)
}

func (j *pendingCreateJobs) CreateWorkflow(ctx context.Context, req WorkflowRequest) error {
	if j.createHook != nil {
		return j.createHook(ctx, req)
	}
	if req.Run.Ref == "feature/pending" {
		return &uncertainWorkflowCreateError{}
	}
	return j.fakeJobs.CreateWorkflow(ctx, req)
}
func (j *pendingCreateJobs) Adopt(ctx context.Context, run model.Run) (bool, error) {
	j.adoptCalls.Add(1)
	if j.adoptHook != nil {
		return j.adoptHook(ctx, run)
	}
	return false, nil
}
func (*pendingCreateJobs) WorkflowNamespace() string { return "test-argo" }
func (j *pendingCreateJobs) WaitResumed(ctx context.Context, name string, out io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	return j.Wait(ctx, name, out)
}

var _ JobAdopter = (*pendingCreateJobs)(nil)
var _ JobCreateSettler = (*pendingCreateJobs)(nil)

func (j *pendingCreateJobs) SettleCreate(ctx context.Context, name, id string, boundary time.Time) (bool, error) {
	if j.settle == nil {
		return j.fakeJobs.SettleCreate(ctx, name, id, boundary)
	}
	return j.settle(ctx, name, id, boundary)
}

func awaitCreateState(t *testing.T, s *Scheduler, id string, wantAttempts int, wantGone bool) {
	t.Helper()
	timer := time.NewTimer(testStartTimeout(5 * time.Second))
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s.createMu.Lock()
		pending, exists := s.createPending[id]
		attempts := 0
		if exists {
			attempts = pending.attempts
		}
		s.createMu.Unlock()
		if wantGone && !exists {
			return
		}
		if !wantGone && exists && attempts >= wantAttempts {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("cleanup state not reached: exists=%t attempts=%d wanted=%d gone=%t", exists, attempts, wantAttempts, wantGone)
		case <-tick.C:
		}
	}
}
func startCreateScheduler(t *testing.T, s *Scheduler) (chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.runAcceptedFixtures(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("scheduler exited: %v", err)
			}
		case <-time.After(testStartTimeout(5 * time.Second)):
			t.Error("scheduler did not stop")
		}
	})
	return done, cancel
}

func TestWorkflowCreateRestartRestoresGrantsWithoutWaitingForSettlement(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked=%t", marked), func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "infrastructure"})
			jobs := &pendingCreateJobs{fakeJobs: f.jobs}
			f.scheduler.jobs = jobs
			f.scheduler.submissions = newSubmissionRegistry()
			var runs []model.Run
			for i := range 2 {
				queued, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: fmt.Sprintf("restart-%d", i), Repository: f.repo, Branch: fmt.Sprintf("feature/restart-%d", i), SHA: strings.Repeat("c", 40), Actor: "pusher"})
				if err != nil {
					t.Fatal(err)
				}
				run, err := f.store.StartRun(t.Context(), queued.ID)
				if err != nil || run.ID != queued.ID {
					t.Fatal(err)
				}
				run, err = f.store.SetRunWorkflowName(t.Context(), run.ID, fmt.Sprintf("wf-%d", i))
				if err != nil {
					t.Fatal(err)
				}
				// A possible Workflow POST always follows durable admission.
				run, err = f.store.MarkRunSubmissionStarted(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.scheduler.recordWorkflowCreateBinding(run, run.WorkflowName); err != nil {
					t.Fatal(err)
				}
				if marked {
					if err := f.store.MarkRunCreatePending(t.Context(), run.ID, run.WorkflowName); err != nil {
						t.Fatal(err)
					}
				}
				runs = append(runs, run)
			}
			if err := f.store.RecoverOwnerState(t.Context()); err != nil {
				t.Fatal(err)
			}
			// A blocking API call must not delay startup or dispatch of unrelated work.
			apiEntered := make(chan struct{}, 1)
			jobs.adoptHook = func(ctx context.Context, _ model.Run) (bool, error) {
				if jobs.adoptCalls.Load() <= 2 {
					return false, nil
				}
				select {
				case apiEntered <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return false, ctx.Err()
			}

			startup := make(chan struct{})
			f.scheduler.onStartupRecovered = func() { close(startup) }
			done, _ := startCreateScheduler(t, f.scheduler)
			select {
			case <-startup:
			case err := <-done:
				done <- err
				t.Fatalf("startup failed: %v", err)
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("startup waited for pending creates")
			}
			select {
			case <-apiEntered:
			case <-time.After(testStartTimeout(time.Second)):
				t.Fatal("pending worker never started")
			}
			if jobs.adoptCalls.Load() < 3 {
				t.Fatal("background recovery did not observe the Workflow")
			}

			state := f.scheduler.submissionStateForTest()
			if state.Runs != 2 || f.scheduler.pendingCreateCount() != 2 {
				t.Fatalf("restart did not restore exact pending contracts: %+v", state)
			}
			other, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "after-restart", Repository: f.repo, Branch: "feature/available", SHA: strings.Repeat("d", 40), Actor: "pusher"})
			if err != nil {
				t.Fatal(err)
			}
			waitForFixtureRunStatus(t, f, other.ID, model.RunFailed)
			for _, run := range runs {
				current, err := f.store.Run(t.Context(), run.ID)
				if err != nil || current.Status != model.RunRunning {
					t.Fatalf("startup discarded pending run: %+v %v", current, err)
				}
			}
		})
	}
}

func TestWorkflowCreateSettlementBackoffIsCapped(t *testing.T) {
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300}
	for i, seconds := range want {
		if got := createSettlementBackoff(i + 1); got != seconds*time.Second {
			t.Fatalf("attempt %d backoff=%s", i+1, got)
		}
	}
}

func TestTerminalCancellationHandoffDoesNotRearmStop(t *testing.T) {
	f := newControlFixture(t)
	enqueued, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "terminal-handoff", Repository: f.repo, Branch: "feature/terminal", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.StartRun(t.Context(), enqueued.ID)
	if err != nil || run.ID != enqueued.ID {
		t.Fatal(run, err)
	}
	if _, err := f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "native terminal verdict"}); err != nil {
		t.Fatal(err)
	}
	f.scheduler.createPending = map[string]*pendingCreate{run.ID: {runID: run.ID}}
	f.scheduler.settleOneCreate(t.Context())
	pending, err := f.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 0 || f.scheduler.pendingCreateCount() != 0 {
		t.Fatalf("terminal handoff rearmed custody: pending=%+v error=%v", pending, err)
	}
}
