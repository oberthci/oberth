package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestStartupAdoptsNonReleaseTagWorkflow(t *testing.T) {
	fixture := newRecoveryFixture(t)
	queued, err := storetest.EnqueueRun(fixture.store, t.Context(), model.RunSpec{RepoID: fixture.repo.ID, RefKind: model.RefTag, Ref: "v1.2.3", SHA: strings.Repeat("a", 40), Actor: "tester", Trigger: "tag"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.ClaimNextRun(t.Context())
	if err != nil || run.ID != queued.ID {
		t.Fatalf("claim: %v", err)
	}
	fixture.setWorkflowName(t, run.ID, "oberth-tag-restart")
	jobs := &adoptingJobs{found: true, recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal}}
	scheduler := fixture.scheduler(t, jobs)
	adopted := ""
	err = scheduler.reconcileStrandedRunsWithGate(t.Context(), func(context.Context) error { return nil }, func(current model.Run) error { adopted = current.ID; return nil })
	if err != nil || adopted != run.ID || len(jobs.deleted) != 0 {
		t.Fatalf("tag was not adopted: id=%s deletes=%v err=%v", adopted, jobs.deleted, err)
	}
	current, err := fixture.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunRunning {
		t.Fatalf("adoption changed durable identity: %+v %v", current, err)
	}
}

type restartCheckoutGit struct{ controlGit }

func (git *restartCheckoutGit) Checkout(ctx context.Context, input, sha, destination string) error {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("old workspace still exists")
	}
	return git.controlGit.Checkout(ctx, input, sha, destination)
}
func TestSameIDRestartRecreatesCheckoutAndAppendsLog(t *testing.T) {
	fixture := newRecoveryFixture(t)
	run := fixture.enqueueAndClaim(t, "feature/restart-files", strings.Repeat("b", 40))
	jobs := &recoveryJobs{waitResult: JobResult{Status: model.RunFailed, Phase: "job", Error: "fixture completed", Steps: []model.StepResult{stepResult(model.StepFailed)}}}
	scheduler := fixture.scheduler(t, jobs)
	scheduler.git = &restartCheckoutGit{}
	source := filepath.Join(scheduler.workspaceRoot, run.ID, "src")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "old-file"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := scheduler.logs.Create(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("before restart\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.store.RequeueStrandedRun(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.Run(t.Context(), run.ID)
	if err != nil || current.Status != model.RunFailed || current.Error != "fixture completed" {
		t.Fatalf("restart failed during preparation: %+v %v", current, err)
	}
	log, err := scheduler.logs.Tail(run.ID, 1024)
	if err != nil || !strings.Contains(string(log), "before restart") {
		t.Fatalf("old log lost: %q %v", log, err)
	}
}
