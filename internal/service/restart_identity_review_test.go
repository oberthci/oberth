package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/model"
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
	if err != nil || current.Status != model.RunRunning || current.SupersededBy != "" {
		t.Fatalf("adoption changed durable identity: %+v %v", current, err)
	}
}
