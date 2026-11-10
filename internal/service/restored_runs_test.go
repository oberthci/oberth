package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

// A run the restored backup recorded as running, whose Workflow does not
// exist here, is interrupted instead of requeued: a restore never
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
	if _, err := database.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := database.SetRunWorkflowName(ctx, enqueued.ID, "oberth-restored-job"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.MarkRunAdmitted(ctx, enqueued.ID); err != nil {
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
	if err := os.WriteFile(marker, []byte(`{"runs":["`+enqueued.ID+`"]}`), 0o600); err != nil {
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
	run, err := database.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunInterrupted || run.Error != restoredRunInterruption {
		t.Fatalf("restored run = %s %q, want interrupted, not requeued", run.Status, run.Error)
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
