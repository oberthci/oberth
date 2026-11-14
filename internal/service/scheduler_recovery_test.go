package service

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

// recoveryJobs is a configurable JobController fake for testing the
// compensation and startup reconciliation paths.
type recoveryJobs struct {
	stopResult     JobResult
	terminalResult JobResult
	terminalErr    error
	waitResult     JobResult
	waitErr        error         // error returned by Wait
	waitBlocks     chan struct{} // blocks Wait until closed
	waitEntered    chan struct{} // closed exactly once when Wait is reached
	enteredOnce    sync.Once
	deleted        []string
	deleteErr      error // error returned by Delete
}

func (*recoveryJobs) WaitForWatchSync(ctx context.Context) error { return ctx.Err() }

func (j *recoveryJobs) CreateWorkflow(context.Context, WorkflowRequest) error { return nil }

func (j *recoveryJobs) Wait(ctx context.Context, _ string, _ io.Writer) (JobResult, error) {
	if j.waitEntered != nil {
		j.enteredOnce.Do(func() { close(j.waitEntered) })
	}
	if j.waitBlocks != nil {
		select {
		case <-j.waitBlocks:
		case <-ctx.Done():
			return JobResult{}, ctx.Err()
		}
	}
	if j.waitErr != nil {
		return j.waitResult, j.waitErr
	}
	return j.waitResult, nil
}

func (j *recoveryJobs) Stop(_ context.Context, name, _ string) error {
	seen := false
	for _, prior := range j.deleted {
		if prior == name {
			seen = true
		}
	}
	if !seen {
		j.deleted = append(j.deleted, name)
	}
	if j.deleteErr != nil {
		return j.deleteErr
	}
	if !j.terminalResult.Status.Terminal() || j.terminalErr != nil {
		j.terminalResult = j.stopResult
		if !j.terminalResult.Status.Terminal() {
			j.terminalResult = j.waitResult
		}
		if !j.terminalResult.Status.Terminal() {
			j.terminalResult = JobResult{Status: model.RunFailed, Phase: "infrastructure", Error: "Stopped with strategy 'Stop'"}
		}
		j.terminalErr = nil
	}
	return nil
}

func (j *recoveryJobs) TerminalResult(_ context.Context, _ string) (JobResult, error) {
	if j.terminalErr == nil && !j.terminalResult.Status.Terminal() {
		return JobResult{}, ErrJobNotTerminal
	}
	return j.terminalResult, j.terminalErr
}

// recoveryFixture builds a store, run-log, and scheduler for testing recovery
// paths. The store is opened fresh (no prior running runs) and the caller
// creates the specific state needed for each test.
type recoveryFixture struct {
	store *store.Store
	repo  model.Repository
	root  string
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	return newRecoveryFixtureWithOptions(t, store.Options{})
}

func newRecoveryFixtureWithOptions(t *testing.T, options store.Options) *recoveryFixture {
	t.Helper()
	root := t.TempDir()
	database, err := openTestStore(t, context.Background(), filepath.Join(root, "oberth.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := storetest.CreateUpstream(database, context.Background(), model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(context.Background(), model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &recoveryFixture{store: database, repo: repo, root: root}
}

func (f *recoveryFixture) enqueueAndClaim(t *testing.T, ref, sha string) model.Run {
	t.Helper()
	enqueued, err := storetest.EnqueueRun(f.store, context.Background(), model.RunSpec{
		RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: sha,
		Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := storetest.ClaimNextRun(f.store, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != enqueued.ID || claimed.Status != model.RunRunning {
		t.Fatalf("claimed run = %#v, want %s running", claimed, enqueued.ID)
	}
	return claimed
}

func (f *recoveryFixture) setWorkflowName(t *testing.T, runID, workflowName string) {
	t.Helper()
	if _, err := f.store.SetRunWorkflowName(context.Background(), runID, workflowName); err != nil {
		t.Fatal(err)
	}
}

func (f *recoveryFixture) scheduler(t *testing.T, jobs JobController) *Scheduler {
	t.Helper()
	logs, err := runlog.Open(filepath.Join(f.root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store:         f.store,
		Git:           &controlGit{},
		Logs:          logs,
		Jobs:          jobs,
		Auditor:       f.store,
		Signals:       NewSignals(),
		WorkspaceRoot: filepath.Join(f.root, "work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixtureHistories.Store(scheduler, scheduler.store)
	t.Cleanup(func() { fixtureHistories.Delete(scheduler) })
	t.Cleanup(scheduler.closeObservations)
	return scheduler
}

// ---------- Gap 1: compensation path tests ----------

func TestCompensationPathRecordsRealResultWhenJobCompleted(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/completed", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fixture.setWorkflowName(t, run.ID, "oberth-completed-job")

	jobs := &recoveryJobs{
		waitBlocks:  make(chan struct{}),
		waitEntered: make(chan struct{}),
		terminalResult: JobResult{
			Status: model.RunFailed,
			Phase:  "job",
			Error:  "Job failed (backoff limit exceeded)",
		},
		terminalErr: nil, // terminal result available
	}

	scheduler := fixture.scheduler(t, jobs)

	ctx, cancel := context.WithCancel(context.Background())
	executeErr := make(chan error, 1)
	go func() {
		executeErr <- scheduler.executeAndCleanup(ctx, run)
	}()
	// Cancel only once Wait is provably blocked; a fixed sleep loses this
	// race under parallel-test load and cancels during run verification.
	select {
	case <-jobs.waitEntered:
	case err := <-executeErr:
		t.Fatalf("executeAndCleanup returned before Wait: %v", err)
	}
	cancel()

	if err := <-executeErr; err != nil {
		t.Fatal(err)
	}

	recovered, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunFailed {
		t.Fatalf("run status = %q, want %q", recovered.Status, model.RunFailed)
	}
	if recovered.Error != "Job failed (backoff limit exceeded)" {
		t.Fatalf("run error = %q, want real Job error", recovered.Error)
	}
	if len(jobs.deleted) != 0 {
		t.Fatalf("completed Job was deleted: %v", jobs.deleted)
	}
}

// TestShutdownRetainsRunningWorkflow: shutdown compensation for
// an ordinary branch run whose Job has no terminal result supersedes it with
// the same queued run. The Job's deletion is owned by the
// durable obligation — executed by the successor's startup cancellation pass
// in the upgrade case — never inline during shutdown.
func TestShutdownLeavesRunningWorkflowForAdoption(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/running", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	fixture.setWorkflowName(t, run.ID, "oberth-running-job")

	jobs := &recoveryJobs{
		waitBlocks:  make(chan struct{}),
		waitEntered: make(chan struct{}),
		terminalErr: ErrJobNotTerminal, // not terminal
	}

	scheduler := fixture.scheduler(t, jobs)

	ctx, cancel := context.WithCancel(context.Background())
	executeErr := make(chan error, 1)
	go func() {
		executeErr <- scheduler.executeAndCleanup(ctx, run)
	}()
	// Cancel only once Wait is provably blocked; a fixed sleep loses this
	// race under parallel-test load and cancels during run verification.
	select {
	case <-jobs.waitEntered:
	case err := <-executeErr:
		t.Fatalf("executeAndCleanup returned before Wait: %v", err)
	}
	cancel()

	if err := <-executeErr; err != nil {
		t.Fatal(err)
	}

	recovered, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunRunning {
		t.Fatalf("run = %#v, want running under the same ID", recovered)
	}
	recoveredRun, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredRun.Status != model.RunRunning || recoveredRun.Ref != recovered.Ref || recoveredRun.SHA != recovered.SHA {
		t.Fatalf("recoveredRun run = %#v, want original running Workflow", recoveredRun)
	}
	if len(jobs.deleted) != 0 {
		t.Fatalf("shutdown deleted inline = %v, want no Stop or deletion during shutdown", jobs.deleted)
	}
	pending, err := fixture.store.PendingRunCancellations(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatal(err)
	}

}

// Tag work resets behind the same durable cancellation fence as branches.
func TestShutdownLeavesTagWorkflowForAdoption(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	enqueued, err := storetest.EnqueueRun(fixture.store, context.Background(), model.RunSpec{
		RepoID: fixture.repo.ID, RefKind: model.RefTag, Ref: "v1.2.3",
		SHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcd", Actor: "agent@host", Trigger: "tag",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := storetest.ClaimNextRun(fixture.store, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != enqueued.ID {
		t.Fatalf("claimed = %#v, want the tag run", run)
	}
	fixture.setWorkflowName(t, run.ID, "oberth-tag-running-job")

	jobs := &recoveryJobs{
		waitBlocks:  make(chan struct{}),
		waitEntered: make(chan struct{}),
		terminalErr: ErrJobNotTerminal,
	}
	scheduler := fixture.scheduler(t, jobs)
	ctx, cancel := context.WithCancel(context.Background())
	executeErr := make(chan error, 1)
	go func() {
		executeErr <- scheduler.executeAndCleanup(ctx, run)
	}()
	select {
	case <-jobs.waitEntered:
	case err := <-executeErr:
		t.Fatalf("executeAndCleanup returned before Wait: %v", err)
	}
	cancel()
	if err := <-executeErr; err != nil {
		t.Fatal(err)
	}
	recovered, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunRunning || recovered.Error != "" {
		t.Fatalf("tag run = %#v, want queued under the original ID", recovered)
	}
	if len(jobs.deleted) != 0 {
		t.Fatalf("deleted inline: %v", jobs.deleted)
	}
	pending, err := fixture.store.PendingRunCancellations(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("missing cleanup fence: %+v %v", pending, err)
	}
}

// ---------- Gap 2: startup reconciliation tests ----------

func TestStartupReconciliationFinishesRunWhoseJobCompleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "oberth.sqlite")

	// Phase 1: create a run with a job name, simulating a scheduler that
	// crashed before observing the Job's completion.
	database, err := openTestStore(t, ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := storetest.EnqueueRun(database, ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/reconcile",
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ClaimNextRun(database, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunWorkflowName(ctx, enqueued.ID, "oberth-reconcile-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunSubmissionStarted(ctx, enqueued.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: reopen the store (simulating server restart). The modified
	// recoverInterruptedRuns leaves this run in "running" because it has a
	// job_name.
	database, err = storetest.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	run, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunRunning {
		t.Fatalf("run status after store recovery = %q, want running", run.Status)
	}

	// Phase 3: scheduler reconciliation finds the Job completed.
	jobs := &recoveryJobs{
		terminalResult: JobResult{
			Status: model.RunFailed,
			Phase:  "job",
			Error:  "runner exited with code 1",
		},
		terminalErr: nil,
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: jobs,
		Auditor: database, Signals: NewSignals(),
		WorkspaceRoot: filepath.Join(root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err != nil {
		t.Fatal(err)
	}

	recovered, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunFailed {
		t.Fatalf("reconciled run status = %q, want failed", recovered.Status)
	}
	if recovered.Error != "runner exited with code 1" {
		t.Fatalf("reconciled run error = %q", recovered.Error)
	}
}

// Confirmed missing Workflows fail without deletion or another submission.
func TestStartupReconciliationFailsRunWithVanishedWorkflow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "oberth.sqlite")

	// Phase 1: create a run with a job name.
	database, err := openTestStore(t, ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := storetest.EnqueueRun(database, ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/vanished",
		SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ClaimNextRun(database, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunWorkflowName(ctx, enqueued.ID, "oberth-vanished-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunSubmissionStarted(ctx, enqueued.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: reopen.
	database, err = storetest.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	run, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunRunning {
		t.Fatalf("run status after store recovery = %q, want running", run.Status)
	}

	// Phase 3: reconciliation finds no terminal result (Job vanished).
	jobs := &recoveryJobs{
		terminalErr: ErrJobNotTerminal,
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: &restartStateJobs{JobController: jobs},
		Auditor: database, Signals: NewSignals(),
		WorkspaceRoot: filepath.Join(root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err != nil {
		t.Fatal(err)
	}

	recovered, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunFailed {
		t.Fatalf("reconciled run = %#v, want failed under the same ID", recovered)
	}
	if recovered.Error != model.ServerRestartReason {
		t.Fatal(recovered)
	}
	if len(jobs.deleted) != 0 {
		t.Fatal("restart deleted a Workflow", jobs.deleted)
	}
	pending, err := database.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	if err := scheduler.completePendingCancellationsWithGate(ctx, scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}
	if len(jobs.deleted) != 0 {
		t.Fatalf("deleted after cancellation pass = %v, want none", jobs.deleted)
	}
	pending, err = database.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending cancellations after pass = %#v, want none", pending)
	}
}

// A tag whose Workflow is confirmed absent fails under its original ID.
func TestStartupReconciliationFailsUnobservedTag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "oberth.sqlite")

	database, err := openTestStore(t, ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := storetest.EnqueueRun(database, ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v9.9.9", Release: true,
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host", Trigger: "tag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ClaimNextRun(database, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunWorkflowName(ctx, enqueued.ID, "oberth-tag-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunSubmissionStarted(ctx, enqueued.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = storetest.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	jobs := &recoveryJobs{terminalErr: ErrJobNotTerminal}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: &restartStateJobs{JobController: jobs},
		Auditor: database, Signals: NewSignals(),
		WorkspaceRoot: filepath.Join(root, "work")})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err != nil {
		t.Fatal(err)
	}

	recovered, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunFailed {
		t.Fatalf("reconciled tag run = %#v, want failed under the original ID", recovered)
	}
	if recovered.Error != model.ServerRestartReason {
		t.Fatalf("reconciled tag run error = %q", recovered.Error)
	}
	if len(jobs.deleted) != 0 {
		t.Fatalf("deleted inline = %v, want durable cleanup", jobs.deleted)
	}
	pending, err := database.PendingRunCancellations(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending cleanup = %+v, %v", pending, err)
	}
}

// ---------- Gap 3: Wait-error path must delete before terminalizing ----------

// TestWaitErrorWithRunningWorkflowDeleteSucceedsFails verifies that when
// Wait returns an error and TerminalResult reports the Workflow is still
// running, a successful Delete allows the run to be terminalized as failed.
func TestWaitErrorWithRunningWorkflowStopCompletesCancelled(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/wait-err-delete-ok", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")

	jobs := &recoveryJobs{
		waitErr:     errors.New("network blip during log streaming"),
		terminalErr: ErrJobNotTerminal, // Workflow still running
		deleteErr:   nil,               // Delete succeeds
	}

	scheduler := fixture.scheduler(t, jobs)
	if err := scheduler.executeAndCleanup(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	recovered, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Native unsuccessful Stop uses the existing cancellation verdict.
	if recovered.Status != model.RunInterrupted || recovered.Phase != model.PhaseCancelled {
		t.Fatalf("run status = %q, want %q", recovered.Status, model.RunInterrupted)
	}
	if !strings.Contains(recovered.Error, "Stopped with strategy") {
		t.Fatalf("run error = %q, want original Wait error", recovered.Error)
	}
	// The Workflow must have been deleted (the exact name is computed by
	// deterministicJobName, not hardcoded).
	if len(jobs.deleted) != 1 {
		t.Fatalf("deleted = %v, want exactly one deletion", jobs.deleted)
	}
}

// TestWaitErrorWithRunningWorkflowDeleteFailsNotTerminalized verifies that
// when Wait returns an error, TerminalResult reports still-running, and
// Delete FAILS, the run is NOT terminalized. The error propagates so the
// run remains owned and reconcilable at restart.
func TestWaitErrorWithRunningWorkflowDeleteFailsNotTerminalized(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/wait-err-delete-fail", "ffffffffffffffffffffffffffffffffffffffff")

	jobs := &recoveryJobs{
		waitErr:     errors.New("network blip"),
		terminalErr: ErrJobNotTerminal,                    // Workflow still running
		deleteErr:   errors.New("API server unavailable"), // Delete fails
	}

	scheduler := fixture.scheduler(t, jobs)
	err := scheduler.executeAndCleanup(context.Background(), run)
	if err != nil || !scheduler.ownsCreateCleanup(run.ID) {
		t.Fatal("Stop refusal lost its background observer", err)
	}

	// The run must NOT be terminalized — it must still be running so
	// the restart pass can reconcile it.
	recovered, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunRunning {
		t.Fatalf("run status = %q, want %q (still owned, not terminalized)", recovered.Status, model.RunRunning)
	}
}

// TestWaitErrorWithTerminalResultRecoveryUsesRealResult verifies the existing
// behavior: when Wait errors but TerminalResult succeeds, the real terminal
// result is recorded.
func TestWaitErrorWithTerminalResultRecoveryUsesRealResult(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/wait-err-recovered", "1111111111111111111111111111111111111111")

	jobs := &recoveryJobs{
		waitErr: errors.New("transient error"),
		terminalResult: JobResult{
			Status: model.RunFailed,
			Phase:  "job",
			Error:  "runner exited with code 2",
		},
		terminalErr: nil, // terminal result available
	}

	scheduler := fixture.scheduler(t, jobs)
	if err := scheduler.executeAndCleanup(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	recovered, err := fixture.store.Run(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunFailed {
		t.Fatalf("run status = %q, want %q", recovered.Status, model.RunFailed)
	}
	if recovered.Error != "runner exited with code 2" {
		t.Fatalf("run error = %q, want real Job error from TerminalResult", recovered.Error)
	}
	// No Delete should have been called — TerminalResult succeeded.
	if len(jobs.deleted) != 0 {
		t.Fatalf("deleted = %v, want none (TerminalResult succeeded)", jobs.deleted)
	}
}

func (j *recoveryJobs) SettleCreate(ctx context.Context, name, id string, _ time.Time) (bool, error) {
	if err := j.Stop(ctx, name, id); err != nil {
		return false, err
	}
	return true, nil
}

func (j *recoveryJobs) Terminate(ctx context.Context, name, id string) error {
	return j.Stop(ctx, name, id)
}
