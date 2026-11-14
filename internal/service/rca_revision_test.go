package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type rcaPreparedFailure struct{ infra bool }

func (e *rcaPreparedFailure) Error() string               { return "source preparation unavailable" }
func (e *rcaPreparedFailure) InfrastructureFailure() bool { return e.infra }
func (*rcaPreparedFailure) BeforeWorkflowCreate() bool    { return true }

type rcaRevisionStore struct {
	*store.Store
	submitFailure, repoFailure bool
	submitRef                  string
	submitError                error
}

func (s *rcaRevisionStore) MarkRunSubmitted(ctx context.Context, id string) (model.Run, error) {
	matches := s.submitRef == ""
	if s.submitFailure && !matches {
		run, err := s.Run(ctx, id)
		matches = err == nil && run.Ref == s.submitRef
	}
	if s.submitFailure && matches {
		if s.submitError != nil {
			return model.Run{}, s.submitError
		}
		return model.Run{}, context.DeadlineExceeded
	}
	return s.Store.MarkRunSubmitted(ctx, id)
}
func (s *rcaRevisionStore) Repository(ctx context.Context, id int64) (model.Repository, error) {
	if s.repoFailure {
		return model.Repository{}, context.DeadlineExceeded
	}
	return s.Store.Repository(ctx, id)
}

func TestRCARevisionDefiniteCreateFailureRetriesWithoutDeletingName(t *testing.T) {
	for _, kind := range []string{"known-source-outage", "hard-policy", "unknown-create", "infra-without-absence-proof"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/"+kind, strings.Repeat("a", 40))
			var failure error = &rcaPreparedFailure{infra: kind == "known-source-outage"}
			if kind == "infra-without-absence-proof" {
				failure = &identityObservationFailure{cause: context.DeadlineExceeded}
			}
			if kind == "unknown-create" {
				failure = errors.New("unknown outcome without proof")
			}
			jobs := &rcaCreateFailure{createErr: failure}
			jobs.deleteErr = errors.New("must not delete this name")
			scheduler := fixture.scheduler(t, jobs)
			if err := scheduler.executeAndCleanup(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			current, err := fixture.store.Run(t.Context(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != model.RunFailed || len(jobs.deleted) != 0 {
				t.Fatal("definite failure changed custody", current, jobs.deleted)
			}
			counts, err := fixture.store.CountRuns(t.Context())
			if err != nil || counts.Total != 1 {
				t.Fatal("automatic attempt created", counts, err)
			}

		})
	}
}

func TestRCARevisionRetryRequiresSettledWorkAndNoTestFailure(t *testing.T) {
	for _, kind := range []string{"submitted-write-failed", "completed-while-down", "create-settlement"} {
		t.Run(kind, func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/"+kind, strings.Repeat("a", 40))
			jobs := &rcaCreateFailure{}
			scheduler := fixture.scheduler(t, jobs)
			// Explicit empty history permits settled infrastructure recovery; an
			// observed test failure still cannot authorize replay.
			switch kind {
			case "submitted-write-failed":
				scheduler.store = &rcaRevisionStore{Store: fixture.store, submitFailure: true}
				if err := scheduler.executeAndCleanup(t.Context(), run); err != nil {
					t.Fatal(err)
				}
				scheduler.settleOneCreate(t.Context())
				if len(jobs.deleted) != 1 || jobs.createCalls != 1 {
					t.Fatal("did not exercise create then delete", jobs.createCalls, jobs.deleted)
				}
			case "completed-while-down":
				fixture.setWorkflowName(t, run.ID, "workflow")
				scheduler.store = &rcaRevisionStore{Store: fixture.store, repoFailure: true}
				if err := scheduler.finalizeReconciledRun(t.Context(), run, JobResult{Status: model.RunFailed, Phase: "test", Steps: []model.StepResult{{NodeID: "ci.ran", Stage: "ci", Step: "ran", Status: model.StepFailed}}}, scheduler.requireMutation); err != nil {
					t.Fatal(err)
				}
			case "create-settlement":
				fixture.setWorkflowName(t, run.ID, "workflow")
				if err := fixture.store.MarkRunCreatePending(t.Context(), run.ID, "workflow"); err != nil {
					t.Fatal(err)
				}
				cancellations, err := fixture.store.PendingRunCancellations(t.Context())
				if err != nil || len(cancellations) != 1 {
					t.Fatal(cancellations, err)
				}
				if err := fixture.store.CompleteObservedRunCancellation(t.Context(), cancellations[0]); err != nil {
					t.Fatal(err)
				}
				done, err := scheduler.settleCreateAttempt(t.Context(), t.Context(), &pendingCreate{runID: run.ID, persisted: true, confirmed: true})
				if err != nil || !done {
					t.Fatal(done, err)
				}
			}
			current, err := fixture.store.Run(t.Context(), run.ID)
			wantRetry := false
			wantStatus := model.RunFailed
			if kind == "submitted-write-failed" {
				wantStatus = model.RunInterrupted
			}
			if err != nil || current.Status != wantStatus {
				t.Fatal("settled infrastructure/test failure classification", current, err)
			}
			counts, err := fixture.store.CountRuns(t.Context())
			wantCount := int64(1)
			if wantRetry {
				wantCount = 2
			}
			if err != nil || int64(counts.Total) != wantCount {
				t.Fatal("unexpected replacement", counts, err)
			}
		})
	}
}

type rcaPendingJobs struct {
	recoveryJobs
	settled     chan struct{}
	knownCreate bool
	calls       atomic.Int64
}

func (j *rcaPendingJobs) CreateWorkflow(ctx context.Context, req WorkflowRequest) error {
	if req.Run.Ref == "uncertain" && !j.knownCreate {
		return &uncertainWorkflowCreateError{}
	}
	return j.recoveryJobs.CreateWorkflow(ctx, req)
}
func (j *rcaPendingJobs) SettleCreate(context.Context, string, string, time.Time) (bool, error) {
	j.calls.Add(1)
	select {
	case j.settled <- struct{}{}:
	default:
	}
	return false, context.DeadlineExceeded
}
func TestRCARevisionCreateCleanupOutageDoesNotStopRunLoop(t *testing.T) {
	testRCACreateCleanupOutage(t, true)
}
func testRCACreateCleanupOutage(t *testing.T, knownCreate bool) {
	fixture := newRecoveryFixture(t)
	var runs []model.Run
	for _, ref := range []string{"uncertain", "independent"} {
		r, err := storetest.EnqueueRun(fixture.store, t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "agent", Trigger: "branch"})
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, r.Run)
	}
	jobs := &rcaPendingJobs{knownCreate: knownCreate, settled: make(chan struct{}, 1), recoveryJobs: recoveryJobs{deleteErr: errors.New("API unavailable"), waitResult: JobResult{Status: model.RunFailed, Phase: "test", FailedStep: "unit", Error: "real failure"}}}
	logs, err := runlog.Open(filepath.Join(fixture.root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	var database SchedulerStore = fixture.store
	if knownCreate {
		database = &rcaRevisionStore{Store: fixture.store, submitFailure: true, submitRef: "uncertain", submitError: errors.New("submission record rejected")}
	}
	scheduler, err := NewScheduler(SchedulerConfig{Store: database, Git: &controlGit{}, Logs: logs, Jobs: jobs, Auditor: fixture.store, Signals: NewSignals(), WorkspaceRoot: filepath.Join(fixture.root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	scheduler.createTicks = ticks
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- scheduler.runAcceptedFixtures(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("scheduler stopped", err)
			}
		case <-time.After(testStartTimeout(15 * time.Second)):
			t.Error("scheduler failed to join")
		}
	})
	timer := time.NewTimer(testStartTimeout(20 * time.Second))
	defer timer.Stop()
	for {
		current, err := fixture.store.Run(t.Context(), runs[1].ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() && scheduler.ownsCreateCleanup(runs[0].ID) {
			break
		}
		select {
		case err := <-done:
			done <- err
			t.Fatal("create cleanup killed scheduler", err)
		case <-timer.C:
			t.Fatal("independent run did not progress")
		case <-time.After(time.Millisecond):
		}
	}
	ticks <- time.Now()
	select {
	case <-jobs.settled:
	case <-timer.C:
		t.Fatal("settlement worker not reached")
	}
	select {
	case err := <-done:
		done <- err
		t.Fatal("settler error killed scheduler", err)
	default:
	}
	pending, err := fixture.store.Run(t.Context(), runs[0].ID)
	if err != nil || !isPendingCreate(pending) {
		t.Fatal("uncertain run not retained", pending, err)
	}
	if state := scheduler.submissionStateForTest(); state.Runs == 0 {
		t.Fatal("uncertain grant released")
	}
}

func TestRCARevisionExistingIntentDoesNotManufactureMissingJournal(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "existing-intent", strings.Repeat("a", 40))
	name, err := deterministicWorkflowName(fixture.repo.Name, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.setWorkflowName(t, run.ID, name)
	run, err = fixture.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	scheduler := fixture.scheduler(t, &rcaCreateFailure{createErr: &rcaPreparedFailure{infra: true}})
	if err := scheduler.executeAndCleanup(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	finished, err := fixture.store.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal("unknown old intent acquired empty evidence", finished, err)
	}
}
