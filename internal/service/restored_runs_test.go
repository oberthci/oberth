package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

// A run the restored backup recorded as running, whose Workflow does not
// exist here, is never dispatched again: a restore never
// re-executes a pipeline by itself. The marker is retired afterwards.
func TestStartupReconciliationDoesNotReexecuteRestoredRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "oberth.sqlite")
	database, err := openTestStore(t, ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := storetest.CreateUpstream(database, ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := storetest.EnqueueRun(database, ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/restored",
		SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host", Trigger: "branch",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.ClaimNextRun(database, ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunWorkflowName(ctx, enqueued.ID, "oberth-restored-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunSubmissionStarted(ctx, enqueued.ID); err != nil {
		t.Fatal(err)
	}
	// A run still queued in the backup may already have run on the version a
	// rollback undid: it is cancelled, not dispatched again.
	queued, err := storetest.EnqueueRun(database, ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.2.3",
		SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Actor: "agent@host", Trigger: "tag",
	})
	if err != nil {
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

	marker := filepath.Join(root, RestoredRunsFile)
	if err := os.WriteFile(marker, []byte(`{"runs":["`+enqueued.ID+`","`+queued.ID+`","no-such-run"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Store: database, Git: &controlGit{}, Logs: logs, Jobs: &recoveryJobs{terminalErr: ErrJobNotTerminal},
		Auditor: database, Signals: NewSignals(), WorkspaceRoot: filepath.Join(root, "work"), RestoredRunsFile: marker})
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.completePendingCancellationsWithGate(ctx, scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}
	scheduler.settleOneCreate(ctx)
	run, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Restored work requests Stop and records the native terminal verdict.
	if run.Status != model.RunFailed && run.Status != model.RunInterrupted {
		t.Fatalf("restored run = %s %q, want interrupted, not requeued", run.Status, run.Error)
	}
	cancelled, err := database.Run(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled.Cancelled() || !strings.Contains(cancelled.Reason, restoredRunCancelReason) {
		t.Fatalf("restored queued run = %s/%s %q, want cancelled with the restore reason", cancelled.Status, cancelled.Phase, cancelled.Reason)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("marker not retired: %v", err)
	}

	// A malformed marker refuses startup instead of re-executing.
	if err := os.WriteFile(marker, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err == nil {
		t.Fatal("malformed marker ignored")
	}
}

// Activation constructs the scheduler before owner recovery. A marker outside
// the database directory must preserve unnamed restored claims for cancellation.
func TestOwnerRecoveryPreservesUnnamedRestoredRunForCancellation(t *testing.T) {
	ctx := t.Context()
	f := newRecoveryFixture(t)
	restored := f.enqueueAndClaim(t, "feature/restored-unnamed", strings.Repeat("a", 40))
	ordinary := f.enqueueAndClaim(t, "feature/ordinary-unnamed", strings.Repeat("b", 40))
	marker := filepath.Join(t.TempDir(), RestoredRunsFile)
	if err := os.WriteFile(marker, []byte(`{"runs":["`+restored.ID+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(f.root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	jobs := &recoveryJobs{terminalErr: ErrJobNotTerminal}
	scheduler, err := NewScheduler(SchedulerConfig{Store: f.store, Git: &controlGit{}, Logs: logs, Jobs: jobs,
		Auditor: f.store, Signals: NewSignals(), WorkspaceRoot: filepath.Join(f.root, "work"), RestoredRunsFile: marker})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	preserved, err := f.store.Run(ctx, restored.ID)
	if err != nil || preserved.Status != model.RunRunning {
		t.Fatal("restore-owned claim failed before cancellation", preserved, err)
	}
	failed, err := f.store.Run(ctx, ordinary.ID)
	if err != nil || failed.Status != model.RunFailed || failed.Error != model.ServerRestartReason {
		t.Fatal("ordinary restart recovery changed", failed, err)
	}
	if err := scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err != nil {
		t.Fatal(err)
	}
	cancelled, err := f.store.Run(ctx, restored.ID)
	if err != nil || !cancelled.Cancelled() || !strings.Contains(cancelled.Reason, restoredRunCancelReason) {
		t.Fatal(cancelled, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("restore marker not retired", err)
	}
	queued, err := storetest.ClaimNextRun(f.store, ctx)
	if !errors.Is(err, store.ErrNotFound) || len(jobs.deleted) != 0 {
		t.Fatal("restored run redispatched or deleted", queued, jobs.deleted, err)
	}
}
