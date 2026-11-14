package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type adoptingJobs struct {
	recoveryJobs
	mu             sync.Mutex
	found          bool
	adoptErr       error
	adoptHook      func(context.Context, model.Run) (bool, error)
	adopted        []string
	created        int
	releaseCreated int
	resumed        []runprogress.Event
	retained       int64
}

func (jobs *adoptingJobs) Adopt(ctx context.Context, run model.Run) (bool, error) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.adopted = append(jobs.adopted, run.ID)
	if jobs.adoptHook != nil {
		return jobs.adoptHook(ctx, run)
	}
	return jobs.found, jobs.adoptErr
}

func (*adoptingJobs) WorkflowNamespace() string { return "oberth-argo" }

var _ JobAdopter = (*adoptingJobs)(nil)

func (jobs *adoptingJobs) CreateWorkflow(context.Context, WorkflowRequest) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.created++
	jobs.terminalResult = JobResult{}
	jobs.terminalErr = ErrJobNotTerminal
	return nil
}

func (jobs *adoptingJobs) CreateRelease(context.Context, WorkflowRequest) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.releaseCreated++
	return nil
}

func (jobs *adoptingJobs) WaitResumed(ctx context.Context, name string, output io.Writer, events []runprogress.Event, retained int64) (JobResult, error) {
	jobs.mu.Lock()
	jobs.resumed, jobs.retained = events, retained
	jobs.mu.Unlock()
	return jobs.Wait(ctx, name, output)
}

func waitAdoptedStatus(t *testing.T, database *store.Store, id string, status model.RunStatus) model.Run {
	t.Helper()
	deadline := time.After(testStartTimeout(5 * time.Second))
	ticker := time.NewTicker(testTimeout(time.Millisecond))
	defer ticker.Stop()
	for {
		run, err := database.Run(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == status {
			return run
		}
		select {
		case <-deadline:
			t.Fatalf("run = %#v, want %s", run, status)
		case <-ticker.C:
		}
	}
}

func TestStartupAdoptsBranchWorkflowAndFinalizesSameRun(t *testing.T) {
	for _, finishedWhileDown := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "finished-while-down"}[finishedWhileDown], func(t *testing.T) {
			fixture := newRecoveryFixture(t)
			previous := fixture.enqueueAndClaim(t, "feature/adopt", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			run := previous
			fixture.setWorkflowName(t, run.ID, "oberth-adopt")
			if _, err := fixture.store.MarkRunSubmitted(context.Background(), run.ID); err != nil {
				t.Fatal(err)
			}
			jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{
				terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}),
				waitResult: JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}},
			}}
			if finishedWhileDown {
				close(jobs.waitBlocks)
			}
			scheduler := fixture.scheduler(t, jobs)
			file, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("[test/unit] before restart\n"); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			event := runprogress.Event{Version: runprogress.Version, Stage: "test", Step: "unit", Status: runprogress.StepPassed, StartedAt: &now, FinishedAt: &now}
			// v0.17.44 retained DAG-attempt burns beside the planned key.
			// A colliding orphan must not stop scheduler startup/adoption.
			if err := scheduler.logs.(*runlog.Store).WriteStepPlan(run.ID, []runprogress.PlannedStep{{Stage: "test", Step: "unit", Ordinal: 0}}); err != nil {
				t.Fatal(err)
			}
			orphan := event
			orphan.Stage = "test(0)"
			if err := scheduler.logs.(*runlog.Store).AppendStepProgress(run.ID, orphan); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.logs.(*runlog.Store).AppendStepProgress(run.ID, event); err != nil {
				t.Fatal(err)
			}
			// Reopen as a new owner, exercising the durable recovery transition.
			if err := fixture.store.Close(); err != nil {
				t.Fatal(err)
			}
			fixture.store, err = storetest.Open(context.Background(), filepath.Join(fixture.root, "oberth.sqlite"), store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = fixture.store.Close() })
			scheduler = fixture.scheduler(t, jobs)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- scheduler.runAcceptedFixtures(ctx) }()
			select {
			case <-jobs.waitEntered:
			case err := <-done:
				t.Fatalf("scheduler stopped: %v", err)
			case <-time.After(testStartTimeout(5 * time.Second)):
				t.Fatal("adopted Wait not entered")
			}
			if !finishedWhileDown {
				current, err := fixture.store.Run(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if current.Status != model.RunRunning || current.QueueSequence != run.QueueSequence || current.AdmissionSequence != run.AdmissionSequence {
					t.Fatalf("adopted run = %#v", current)
				}
				scheduler.submissions.mu.Lock()
				runs := len(scheduler.submissions.owners)
				order := scheduler.submissions.owners[run.QueueSequence].order
				scheduler.submissions.mu.Unlock()
				if order != run.AdmissionSequence {
					t.Fatalf("adopted admission order = %d, want persisted %d", order, run.AdmissionSequence)
				}
				if runs != 1 {
					t.Fatalf("admission runs = %d", runs)
				}
				close(jobs.waitBlocks)
			}
			current := waitAdoptedStatus(t, fixture.store, run.ID, model.RunPassed)
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if current.WorkflowName != "oberth-adopt" || current.QueueSequence != run.QueueSequence || current.AdmissionSequence != run.AdmissionSequence {
				t.Fatalf("finalized run = %#v", current)
			}
			if counts, err := fixture.store.CountRuns(context.Background()); err != nil || counts.Total != 1 {
				t.Fatalf("adoption enqueued another run: counts=%#v err=%v", counts, err)
			}
			if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
				t.Fatalf("cancellations = %#v, %v", pending, err)
			}
			steps, err := fixture.store.StepResults(context.Background(), run.ID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("steps = %#v, %v", steps, err)
			}
			if jobs.created != 0 || len(jobs.deleted) != 0 || len(jobs.adopted) != 1 || jobs.retained != int64(len("[test/unit] before restart\n")) || len(jobs.resumed) != 1 {
				t.Fatalf("adoption did not preserve intent/logs: %#v", jobs)
			}
			scheduler.submissions.mu.Lock()
			runs := len(scheduler.submissions.owners)
			scheduler.submissions.mu.Unlock()
			if runs != 0 {
				t.Fatalf("admission leaked: %d", runs)
			}
		})
	}
}

func TestAdoptionMissingWorkflowFailsButReadFailureDoesNot(t *testing.T) {
	for _, readErr := range []error{nil, errors.New("API unavailable")} {
		fixture := newRecoveryFixture(t)
		run := fixture.enqueueAndClaim(t, "feature/missing", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		fixture.setWorkflowName(t, run.ID, "oberth-missing")
		jobs := &adoptingJobs{adoptErr: readErr, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if readErr != nil {
			jobs.adoptHook = func(context.Context, model.Run) (bool, error) { cancel(); return false, readErr }
		}
		scheduler := fixture.scheduler(t, jobs)
		err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, func(model.Run) error { return errors.New("absent Workflow adopted") })
		current, lookupErr := fixture.store.Run(context.Background(), run.ID)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		if readErr == nil {
			if err != nil || current.Status != model.RunFailed || current.Error != model.ServerRestartReason {
				t.Fatalf("missing recovery: %#v, %v", current, err)
			}
		} else if err == nil || current.Status != model.RunRunning {
			t.Fatalf("failed observation mutated run: %#v, %v", current, err)
		}
	}
}

func TestAdoptedWorkflowSurvivesShutdownWithWorkspace(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/shutdown", "cccccccccccccccccccccccccccccccccccccccc")
	fixture.setWorkflowName(t, run.ID, "oberth-shutdown")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
	scheduler := fixture.scheduler(t, jobs)
	source := filepath.Join(fixture.root, "work", run.ID, "src")
	if err := os.MkdirAll(source, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.runAcceptedFixtures(ctx) }()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("scheduler stopped: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("Wait not entered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunRunning || current.WorkflowName != "oberth-shutdown" || len(jobs.deleted) != 0 {
		t.Fatalf("shutdown disrupted Workflow: %#v; deleted %v", current, jobs.deleted)
	}
	if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("shutdown cancellations: %#v, %v", pending, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("active workspace removed: %v", err)
	}
}

// Adoption is idempotent and counts every adopted run; a new run is
// granted beside adopted ones (Argo parallelism bounds the Workflows).
func TestAdoptedAdmissionIsIdempotent(t *testing.T) {
	gate := newSubmissionRegistry()
	run := model.Run{ID: "active", QueueSequence: 11, AdmissionSequence: 2, RepoID: 1}
	first, err := gate.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	if first.sequence != run.QueueSequence || first.order != run.AdmissionSequence {
		t.Fatalf("adoption changed admission identity: key=%d order=%d", first.sequence, first.order)
	}
	again, err := gate.adopt(run)
	if err != nil || again != first {
		t.Fatalf("second adoption = %p, %v", again, err)
	}
	if _, err := gate.adopt(model.Run{ID: "conflict", QueueSequence: 11, AdmissionSequence: 2, RepoID: 1}); !errors.Is(err, ErrAdoptionRefused) {
		t.Fatalf("conflicting adoption = %v", err)
	}
	other, err := gate.adopt(model.Run{ID: "also-active", QueueSequence: 2, RepoID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(gate.owners) != 2 {
		t.Fatalf("adopted runs = %d", len(gate.owners))
	}
	waiting := gate.trackRun(model.Run{ID: "next", QueueSequence: 3, RepoID: 1})
	waiting.cancel()
	other.cancel()
	first.cancel()
	if len(gate.owners) != 0 {
		t.Fatalf("admission leaked: %d", len(gate.owners))
	}
}

func TestAdoptedRunStillHonorsCancel(t *testing.T) {

	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/preempted", "dddddddddddddddddddddddddddddddddddddddd")
	fixture.setWorkflowName(t, run.ID, "oberth-preempted")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitResult: JobResult{Status: model.RunFailed, Phase: model.PhaseCancelled}, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
	scheduler := fixture.scheduler(t, jobs)
	reservation, err := scheduler.submissions.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.resumeAdoptedRun(ctx, run, scheduler.requireMutation, reservation) }()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("resume stopped: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("Wait not entered")
	}
	if _, err := fixture.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}

	if err := scheduler.completePendingCancellations(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(jobs.waitBlocks)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Cancelled() || len(jobs.deleted) != 1 || jobs.deleted[0] != current.WorkflowName {
		t.Fatalf("adopted cancellation = %#v deleted=%v", current, jobs.deleted)
	}
	if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("pending cancellation = %#v %v", pending, err)
	}
	scheduler.submissions.mu.Lock()
	runs := len(scheduler.submissions.owners)
	scheduler.submissions.mu.Unlock()
	if runs != 0 {
		t.Fatalf("cancelled adoption retained admission: %d", runs)
	}
}

func TestNewBranchWorkflowSurvivesShutdown(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/new-shutdown", "ffffffffffffffffffffffffffffffffffffffff")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitBlocks: make(chan struct{}), waitEntered: make(chan struct{})}}
	scheduler := fixture.scheduler(t, jobs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.executeAndCleanup(ctx, run) }()
	select {
	case <-jobs.waitEntered:
	case err := <-done:
		t.Fatalf("execute stopped: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("Wait not entered")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunRunning || current.WorkflowName == "" || jobs.created != 1 || len(jobs.deleted) != 0 {
		t.Fatalf("new Workflow shutdown = %#v created=%d deleted=%v", current, jobs.created, jobs.deleted)
	}
	if pending, err := fixture.store.PendingRunCancellations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("shutdown cancellation = %#v %v", pending, err)
	}
}

func TestUnreadableNonAdoptingWorkflowKeepsRunForObservation(t *testing.T) {
	for _, release := range []bool{false, true} {
		fixture := newRecoveryFixture(t)
		spec := model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/promotion", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host", Trigger: "promotion"}
		if release {
			spec.RefKind = model.RefTag
			spec.Ref = "v1"
			spec.Trigger = "tag"
			spec.Release = true
		}
		var enqueued model.EnqueueRunResult
		var err error
		if release {
			enqueued, err = storetest.EnqueueRun(fixture.store, context.Background(), spec)
		} else {
			spec.TestedSHA, spec.BaseSHA = spec.SHA, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			enqueued, _, err = storetest.EnqueuePromotionRun(fixture.store, context.Background(), spec, model.PromotionSpec{
				RepoID: fixture.repo.ID, SourceBranch: spec.Ref, SourceSHA: spec.SHA, TargetRef: "main", Actor: "agent@host",
				ResultSHA: spec.SHA, PreviousSHA: spec.BaseSHA,
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		run := enqueued.Run
		if _, err := fixture.store.StartRun(context.Background(), run.ID); err != nil {
			t.Fatal(err)
		}
		fixture.setWorkflowName(t, run.ID, "oberth-legacy")
		jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
		// Without adoption or a terminal read, recovery retains this run.
		scheduler := fixture.scheduler(t, struct{ JobController }{jobs})
		if err := scheduler.reconcileStrandedRunsWithGate(context.Background(), scheduler.requireMutation, func(model.Run) error { return errors.New("out-of-scope adoption") }); err == nil {
			t.Fatal("unreadable Workflow was treated as absent")
		}
		current, err := fixture.store.Run(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != model.RunRunning || len(jobs.deleted) != 0 || len(jobs.adopted) != 0 {
			t.Fatal("unknown Workflow was mutated", current, jobs.deleted)
		}

	}
}

func TestAdoptionPreemptedBeforeWaitRetainsAdmissionOnDeleteFailure(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/delete-failure", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fixture.setWorkflowName(t, run.ID, "oberth-delete-failure")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{deleteErr: errors.New("API unavailable")}}
	scheduler := fixture.scheduler(t, jobs)
	reservation, err := scheduler.submissions.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}
	// A refused deletion no longer ends the scheduler: the worker returns
	// nil and the obligation stays pending for the dispatch-loop pass.
	if err := scheduler.resumeAdoptedRun(context.Background(), run, scheduler.requireMutation, reservation); err != nil {
		t.Fatalf("refused deletion ended the worker with an error: %v", err)
	}
	pending, err := fixture.store.PendingRunCancellations(context.Background())
	if err != nil || len(pending) != 1 || pending[0].RunID != run.ID {
		t.Fatalf("refused deletion lost its obligation: %+v %v", pending, err)
	}
	// The retry pass that completes the obligation releases the slot once.
	jobs.deleteErr = nil
	scheduler.settleOneCreate(t.Context())
	if err := scheduler.completePendingCancellations(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptedObservationFailureFailsOnlyThatRun(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/blip", "abababababababababababababababababababab")
	fixture.setWorkflowName(t, run.ID, "oberth-blip")
	// Wait returns only after the controller's bounded read retry gave up.
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal, waitErr: errors.New("argojob: read Workflow oberth-blip: connection refused")}}
	scheduler := fixture.scheduler(t, jobs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.runAcceptedFixtures(ctx) }()
	current := waitAdoptedStatus(t, fixture.store, run.ID, model.RunInterrupted)
	if current.Phase != model.PhaseCancelled || !strings.Contains(current.Error, "Stopped with strategy") {
		t.Fatalf("adopted observation failure = %#v", current)
	}
	select {
	case err := <-done:
		t.Fatalf("one adopted run's observation failure stopped the scheduler: %v", err)
	case <-time.After(testTimeout(50 * time.Millisecond)):
	}
	// A new accepting request still submits after the failed adoption.
	next, err := storetest.EnqueueRun(fixture.store, context.Background(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "feature/next", SHA: "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", Actor: "agent@host", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.AcceptEnqueue(ctx, next); err != nil {
		t.Fatal(err)
	}
	waitAdoptedStatus(t, fixture.store, next.ID, model.RunInterrupted)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(jobs.deleted) != 2 || jobs.deleted[0] != "oberth-blip" {
		t.Fatalf("unobservable Workflows were not deleted before failing: %v", jobs.deleted)
	}
	scheduler.submissions.mu.Lock()
	runs := len(scheduler.submissions.owners)
	scheduler.submissions.mu.Unlock()
	if runs != 0 {
		t.Fatalf("failed adoption retained admission: %d", runs)
	}
}

func TestAdoptedTerminalObservationErrorKeepsRealResult(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/budget", "efefefefefefefefefefefefefefefefefefefef")
	fixture.setWorkflowName(t, run.ID, "oberth-budget")
	passed := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{
		terminalResult: passed, waitResult: passed,
		waitErr: errors.New("step test/unit breached the aggregate run log budget"),
	}}
	scheduler := fixture.scheduler(t, jobs)
	reservation, err := scheduler.submissions.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.resumeAdoptedRun(context.Background(), run, scheduler.requireMutation, reservation); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunPassed || len(jobs.deleted) != 0 {
		t.Fatalf("terminal adopted Workflow lost its real result: %#v deleted=%v", current, jobs.deleted)
	}
	if steps, err := fixture.store.StepResults(context.Background(), run.ID); err != nil || len(steps) != 1 {
		t.Fatalf("steps = %#v, %v", steps, err)
	}
}

func TestAdoptedCompletedWorkflowWaitErrorUsesStopFallback(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/budget", "efefefefefefefefefefefefefefefefefefefef")
	fixture.setWorkflowName(t, run.ID, "oberth-budget")
	passed := JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}}
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{
		terminalErr: errors.New("completion read unavailable"), waitResult: passed,
		waitErr: errors.New("step test/unit breached the aggregate run log budget"),
	}}
	scheduler := fixture.scheduler(t, jobs)
	reservation, err := scheduler.submissions.adopt(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.resumeAdoptedRun(context.Background(), run, scheduler.requireMutation, reservation); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != model.RunPassed || len(jobs.deleted) != 1 || jobs.deleted[0] != "oberth-budget" {
		t.Fatalf("adopted completion Stop fallback lost its real result: %#v deleted=%v", current, jobs.deleted)
	}
	if steps, err := fixture.store.StepResults(context.Background(), run.ID); err != nil || len(steps) != 1 {
		t.Fatalf("steps = %#v, %v", steps, err)
	}
}

func TestAdoptionFailureNamesWorkflowAndIsolatesRefusal(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, test := range []struct {
		name      string
		adoptErr  error
		holdOther bool
		refused   bool
	}{
		{"binding-mismatch", fmt.Errorf("%w: argojob: Workflow oberth-refused adoption binding mismatch", ErrAdoptionRefused), false, true},
		{"admission-conflict", nil, true, true},
		{"read-failure", errors.New("argojob: read Workflow for adoption: connection refused"), false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			logged.Reset()
			fixture := newRecoveryFixture(t)
			run := fixture.enqueueAndClaim(t, "feature/refused", "abcdefabcdefabcdefabcdefabcdefabcdefabcd")
			fixture.setWorkflowName(t, run.ID, "oberth-refused")
			jobs := &adoptingJobs{found: test.adoptErr == nil, adoptErr: test.adoptErr, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.name == "read-failure" {
				jobs.adoptHook = func(context.Context, model.Run) (bool, error) { cancel(); return false, test.adoptErr }
			}
			scheduler := fixture.scheduler(t, jobs)
			if test.holdOther {
				// Another run already holds this run's admission key.
				if _, err := scheduler.submissions.adopt(model.Run{ID: "holder", QueueSequence: run.QueueSequence, AdmissionSequence: run.AdmissionSequence, RepoID: run.RepoID}); err != nil {
					t.Fatal(err)
				}
			}
			err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, func(run model.Run) error {
				_, err := scheduler.submissions.adopt(run)
				return err
			})
			if (err == nil) != test.refused {
				t.Fatalf("adoption failure = %v", err)
			}
			inspection := "kubectl -n oberth-argo get workflow oberth-refused"
			messages := []string{logged.String()}
			if err != nil {
				messages = append(messages, err.Error())
			}
			for _, message := range messages {
				if !strings.Contains(message, "run "+run.ID) || !strings.Contains(message, "Workflow oberth-argo/oberth-refused") || strings.Contains(message, inspection) != test.refused {
					t.Fatalf("operator message does not name the Workflow: %q", message)
				}
			}
			if !strings.Contains(logged.String(), "ERROR: ") {
				t.Fatalf("adoption failure not logged prominently: %q", logged.String())
			}
			current, lookupErr := fixture.store.Run(context.Background(), run.ID)
			wantStatus := model.RunRunning
			if test.refused {
				wantStatus = model.RunFailed
				if current.Phase != "infrastructure" || !strings.Contains(current.Error, "recovery refused for this run") {
					t.Fatal("refusal not reported on its run", current)
				}
			}
			if lookupErr != nil || current.Status != wantStatus || len(jobs.deleted) != 0 {
				t.Fatalf("refused adoption changed Workflow or lost run result: %#v %v deleted=%v", current, lookupErr, jobs.deleted)
			}
		})
	}
}

type refusingDeleteJobs struct {
	*adoptingJobs
	refusals atomic.Int32
	calls    atomic.Int32
}

func (jobs *refusingDeleteJobs) Stop(ctx context.Context, name, id string) error {
	jobs.calls.Add(1)
	if jobs.refusals.Add(-1) >= 0 {
		return errors.New("kube-apiserver temporarily unavailable")
	}
	return jobs.adoptingJobs.Stop(ctx, name, id)
}

// A cancel lands under an adopted Wait while the API refuses the deletion.
// The worker's own cancellation pass must not end the scheduler; the
// dispatch-loop retry deletes the Workflow and releases the slot once.
func TestAdoptedPreemptedObservationDeleteRefusalKeepsSchedulerAlive(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/refused-delete", "efefefefefefefefefefefefefefefefefefefef")
	fixture.setWorkflowName(t, run.ID, "oberth-refused-delete")
	inner := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{
		terminalErr: ErrJobNotTerminal, waitErr: errors.New("argojob: Workflow deleted under Wait"),
		waitBlocks: make(chan struct{}), waitEntered: make(chan struct{}),
	}}
	jobs := &refusingDeleteJobs{adoptingJobs: inner}
	jobs.refusals.Store(2)
	scheduler := fixture.scheduler(t, jobs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.runAcceptedFixtures(ctx) }()
	select {
	case <-inner.waitEntered:
	case err := <-done:
		t.Fatalf("scheduler exited before the adopted Wait: %v", err)
	case <-time.After(testStartTimeout(5 * time.Second)):
		t.Fatal("adopted run never waited")
	}
	if _, err := fixture.store.CancelRun(context.Background(), model.RunCancelRequest{RunID: run.ID, Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}
	close(inner.waitBlocks)
	deadline := time.Now().Add(testStartTimeout(10 * time.Second))
	for {
		select {
		case err := <-done:
			t.Fatalf("a refused Workflow deletion stopped the scheduler: %v", err)
		default:
		}
		pending, err := fixture.store.PendingRunCancellations(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		scheduler.submissions.mu.Lock()
		runs := len(scheduler.submissions.owners)
		scheduler.submissions.mu.Unlock()
		if len(pending) == 0 && runs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("obligation or slot not settled: pending=%+v runs=%d calls=%d", pending, runs, jobs.calls.Load())
		}
		time.Sleep(testTimeout(5 * time.Millisecond))
	}
	if jobs.calls.Load() < 3 {
		t.Fatalf("deletion was not retried after refusals: calls=%d", jobs.calls.Load())
	}
	scheduler.submissions.mu.Lock()
	runs := len(scheduler.submissions.owners)
	scheduler.submissions.mu.Unlock()
	if runs != 0 {
		t.Fatalf("slot not released exactly once: %d", runs)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func (jobs *refusingDeleteJobs) SettleCreate(ctx context.Context, name, id string, _ time.Time) (bool, error) {
	if err := jobs.Stop(ctx, name, id); err != nil {
		return false, err
	}
	return true, nil
}

func (j *refusingDeleteJobs) Terminate(ctx context.Context, name, id string) error {
	return j.Stop(ctx, name, id)
}
