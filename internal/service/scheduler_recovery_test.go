package service

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

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
	waitBlocks     chan struct{} // blocks Wait until closed
	deleted        []string
}

func (j *recoveryJobs) CreateCI(context.Context, JobRequest) error { return nil }

func (j *recoveryJobs) Wait(ctx context.Context, _ string, _ io.Writer) (JobResult, error) {
	if j.waitBlocks != nil {
		select {
		case <-j.waitBlocks:
		case <-ctx.Done():
			return JobResult{}, ctx.Err()
		}
	}
	return j.waitResult, nil
}

func (j *recoveryJobs) Delete(_ context.Context, name, _ string) error {
	j.deleted = append(j.deleted, name)
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
		waitBlocks: make(chan struct{}),
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
	// Give Wait time to block, then cancel.
	time.Sleep(20 * time.Millisecond)
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
		terminalErr: ErrJobNotTerminal, // not terminal
	}

	scheduler := fixture.scheduler(t, jobs)

	ctx, cancel := context.WithCancel(context.Background())
	executeErr := make(chan error, 1)
	go func() {
		executeErr <- scheduler.executeAndCleanup(ctx, run)
	}()
	time.Sleep(20 * time.Millisecond)
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

func TestStartupReconciliationInterruptsRunWithVanishedJob(t *testing.T) {
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
	if recovered.Status != model.RunInterrupted {
		t.Fatalf("reconciled run status = %q, want interrupted", recovered.Status)
	}
	if recovered.Error != "job not in terminal state at restart; result unrecoverable" {
		t.Fatalf("reconciled run error = %q", recovered.Error)
	}
	if len(jobs.deleted) != 1 || jobs.deleted[0] != "oberth-vanished-job" {
		t.Fatalf("deleted = %v, want [oberth-vanished-job]", jobs.deleted)
	}
}
