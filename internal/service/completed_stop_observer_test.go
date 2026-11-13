package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestStoppedCompletionStillObserves(t *testing.T) {
	for _, completed := range []bool{true, false} {
		name := "running-control"
		if completed {
			name = "completed-before-settlement"
		}
		t.Run(name, func(t *testing.T) {
			f := newControlFixture(t)
			run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: name, Repository: f.repo, Branch: f.repo.DefaultBranch, SHA: strings.Repeat("a", 40), Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := f.store.ClaimNextRun(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			claimed, err = f.store.SetRunWorkflowName(t.Context(), claimed.ID, "workflow")
			if err != nil {
				t.Fatal(err)
			}
			jobs := &exitTokenJobs{fakeJobs: f.jobs, delivered: make(chan struct{})}
			jobs.finished.Store(completed)
			f.scheduler.jobs = jobs
			if err := f.store.MarkRunCreatePending(t.Context(), run.ID, claimed.WorkflowName); err != nil {
				t.Fatal(err)
			}
			f.scheduler.registerCreateCleanup(claimed, true)
			f.scheduler.settleOneCreate(t.Context())
			waitForFixtureRunStatus(t, f, run.ID, model.RunInterrupted)
			f.scheduler.createObservers.Wait()
			select {
			case <-jobs.delivered:
			default:
				t.Error("completed Workflow bypassed normal resumed observer")
			}
			body, _, _, err := f.logs.ReadFrom(run.ID, 0, 1<<20)
			if err != nil || !strings.Contains(string(body), "exit handler received its executor token") {
				t.Errorf("final exit logs were not collected: %q %v", body, err)
			}
			steps, err := f.store.StepResults(t.Context(), run.ID)
			if err != nil || len(steps) != 1 || steps[0].Step != "cleanup" || steps[0].Status != model.StepPassed {
				t.Fatalf("final step evidence lost: %+v %v", steps, err)
			}
			f.scheduler.createMu.Lock()
			f.scheduler.createPending[run.ID].next = time.Time{}
			f.scheduler.createMu.Unlock()
			f.scheduler.settleOneCreate(t.Context())
			if f.scheduler.pendingCreateCount() != 0 {
				t.Fatal("final observation did not release settlement ownership")
			}

		})
	}
}
