package service

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

// recoveryJobs is a configurable JobController fake for testing the
// compensation and startup reconciliation paths.
type recoveryJobs struct {
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

func (j *recoveryJobs) CreateCI(context.Context, JobRequest) error { return nil }

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

func (j *recoveryJobs) Delete(_ context.Context, name, _ string) error {
	j.deleted = append(j.deleted, name)
	if j.deleteErr != nil {
		return j.deleteErr
	}
	return nil
}

func (j *recoveryJobs) TerminalResult(_ context.Context, _ string) (JobResult, error) {
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
	root := t.TempDir()
	database, err := store.Open(context.Background(), filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := database.CreateUpstream(context.Background(), model.UpstreamSpec{
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
	enqueued, err := f.store.EnqueueRun(context.Background(), model.RunSpec{
		RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: sha,
		Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.ClaimNextRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != enqueued.ID || claimed.Status != model.RunRunning {
		t.Fatalf("claimed run = %#v, want %s running", claimed, enqueued.ID)
	}
	return claimed
}

func (f *recoveryFixture) setJobName(t *testing.T, runID, jobName string) {
	t.Helper()
	if _, err := f.store.SetRunJobName(context.Background(), runID, jobName); err != nil {
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
		MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

// ---------- Gap 1: compensation path tests ----------

func TestCompensationPathRecordsRealResultWhenJobCompleted(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/completed", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	fixture.setJobName(t, run.ID, "oberth-completed-job")

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

func TestCompensationPathInterruptsWhenJobStillRunning(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/running", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	fixture.setJobName(t, run.ID, "oberth-running-job")

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
	if recovered.Status != model.RunInterrupted {
		t.Fatalf("run status = %q, want %q", recovered.Status, model.RunInterrupted)
	}
	if recovered.Error != "scheduler stopped" {
		t.Fatalf("run error = %q, want %q", recovered.Error, "scheduler stopped")
	}
	if len(jobs.deleted) != 1 {
		t.Fatalf("expected exactly one Job deletion, got %v", jobs.deleted)
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
	database, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
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
	enqueued, err := database.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/reconcile",
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunJobName(ctx, enqueued.ID, "oberth-reconcile-job"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: reopen the store (simulating server restart). The modified
	// recoverInterruptedRuns leaves this run in "running" because it has a
	// job_name.
	database, err = store.Open(ctx, dbPath, store.Options{})
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
		WorkspaceRoot: filepath.Join(root, "work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation); err != nil {
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

// TestStartupReconciliationRequeuesRunWithVanishedJob: a stranded ordinary
// branch run whose Job cannot report a terminal result is superseded by a
// fresh queued copy (issue #270) instead of dying terminally interrupted.
// The stranded Job's deletion is executed by the cancellation pass, not
// inline, and must happen before the replacement can be claimed.
func TestStartupReconciliationRequeuesRunWithVanishedJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "oberth.sqlite")

	// Phase 1: create a run with a job name.
	database, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
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
	enqueued, err := database.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/vanished",
		SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunJobName(ctx, enqueued.ID, "oberth-vanished-job"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// Phase 2: reopen.
	database, err = store.Open(ctx, dbPath, store.Options{})
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
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: jobs,
		Auditor: database, Signals: NewSignals(),
		WorkspaceRoot: filepath.Join(root, "work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}

	recovered, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunInterrupted || recovered.SupersededBy == "" {
		t.Fatalf("reconciled run = %#v, want interrupted and superseded by its requeue", recovered)
	}
	requeued, err := database.Run(ctx, recovered.SupersededBy)
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != model.RunQueued || requeued.Ref != recovered.Ref ||
		requeued.SHA != recovered.SHA || requeued.Trigger != recovered.Trigger ||
		requeued.Actor != recovered.Actor {
		t.Fatalf("requeued run = %#v, want queued copy of %#v", requeued, recovered)
	}
	// Reconciliation must NOT delete inline: the durable supersede obligation
	// owns the deletion and stays pending until the cancellation pass.
	if len(jobs.deleted) != 0 {
		t.Fatalf("deleted before cancellation pass = %v, want none", jobs.deleted)
	}
	pending, err := database.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].RunID != enqueued.ID ||
		pending[0].JobName != "oberth-vanished-job" || pending[0].Reason != "superseded" ||
		pending[0].SupersededBy != requeued.ID {
		t.Fatalf("pending cancellations = %#v, want superseded obligation for the stranded Job", pending)
	}
	if err := scheduler.completePendingCancellationsWithGate(ctx, scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}
	if len(jobs.deleted) != 1 || jobs.deleted[0] != "oberth-vanished-job" {
		t.Fatalf("deleted after cancellation pass = %v, want [oberth-vanished-job]", jobs.deleted)
	}
	pending, err = database.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending cancellations after pass = %#v, want none", pending)
	}
}

// TestStartupReconciliationTerminalizesIneligibleStrandedRun pins the legacy
// conservative path for work the restart requeue must never re-fire: a tag
// (release-tier) run with a vanished Job is deleted inline and terminally
// interrupted, exactly as before issue #270.
func TestStartupReconciliationTerminalizesIneligibleStrandedRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "oberth.sqlite")

	database, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
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
	enqueued, err := database.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v9.9.9",
		SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host", Trigger: "tag",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunJobName(ctx, enqueued.ID, "oberth-tag-job"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(ctx, dbPath, store.Options{})
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
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: jobs,
		Auditor: database, Signals: NewSignals(),
		WorkspaceRoot: filepath.Join(root, "work"), MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}

	recovered, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunInterrupted || recovered.SupersededBy != "" {
		t.Fatalf("reconciled tag run = %#v, want terminally interrupted without requeue", recovered)
	}
	if recovered.Error != "job not in terminal state at restart; result unrecoverable" {
		t.Fatalf("reconciled tag run error = %q", recovered.Error)
	}
	if len(jobs.deleted) != 1 || jobs.deleted[0] != "oberth-tag-job" {
		t.Fatalf("deleted = %v, want [oberth-tag-job]", jobs.deleted)
	}
}

// ---------- Gap 3: Wait-error path must delete before terminalizing ----------

// TestWaitErrorWithRunningWorkflowDeleteSucceedsFails verifies that when
// Wait returns an error and TerminalResult reports the Workflow is still
// running, a successful Delete allows the run to be terminalized as failed.
func TestWaitErrorWithRunningWorkflowDeleteSucceedsFails(t *testing.T) {
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
	// The run must be terminalized as failed — the Workflow was deleted.
	if recovered.Status != model.RunFailed {
		t.Fatalf("run status = %q, want %q", recovered.Status, model.RunFailed)
	}
	if recovered.Error != "network blip during log streaming" {
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
	if err == nil {
		t.Fatal("expected an error from executeAndCleanup when Delete fails, got nil")
	}
	if !strings.Contains(err.Error(), "delete Workflow") {
		t.Fatalf("error = %v, want error mentioning Workflow deletion", err)
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
