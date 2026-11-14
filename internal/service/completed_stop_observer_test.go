package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
)

func TestStoppedCompletionStillObserves(t *testing.T) {
	for _, completed := range []bool{true, false} {
		name := "running-control"
		if completed {
			name = "completed-before-settlement"
		}
		t.Run(name, func(t *testing.T) {
			f := newControlFixture(t)
			run, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: name, Repository: f.repo, Branch: f.repo.DefaultBranch, SHA: strings.Repeat("a", 40), Actor: "owner"})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := f.store.StartRun(t.Context(), run.ID)
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
			body, _, err := f.logs.ReadRunLog(t.Context(), run.ID, runlog.Filter{})
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

func TestStopSettlementDoesNotShareExistingObserverOwnership(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			for _, completed := range []bool{false, true} {
				name := "running"
				if completed {
					name = "completed"
				}
				t.Run(name, func(t *testing.T) {
					f := newControlFixture(t)
					run, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: name, Repository: f.repo, Branch: f.repo.DefaultBranch, SHA: strings.Repeat("a", 40), Actor: "owner"})
					if err != nil {
						t.Fatal(err)
					}
					claimed, err := f.store.StartRun(t.Context(), run.ID)
					if err != nil {
						t.Fatal(err)
					}
					claimed, err = f.store.SetRunWorkflowName(t.Context(), claimed.ID, "workflow")
					if err != nil {
						t.Fatal(err)
					}
					owner, err := f.scheduler.submissions.adopt(claimed)
					if err != nil {
						t.Fatal(err)
					}
					defer owner.cancel()
					jobs := &exitTokenJobs{fakeJobs: f.jobs, delivered: make(chan struct{})}
					jobs.finished.Store(completed)
					f.scheduler.jobs = jobs
					if err := f.store.MarkRunCreatePending(t.Context(), run.ID, claimed.WorkflowName); err != nil {
						t.Fatal(err)
					}
					if cancelled {
						cancellation, err := f.store.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "owner"})
						if err != nil {
							t.Fatal(err)
						}
						f.scheduler.cancelCreatingRuns(t.Context(), cancellation.Cancellations)
					}
					f.scheduler.registerCreateCleanup(claimed, true)
					f.scheduler.settleOneCreate(t.Context())
					f.scheduler.createObservers.Wait()
					select {
					case <-jobs.delivered:
						t.Error("settlement started a second observer on an owned run")
					default:
					}
					select {
					case <-owner.withdrawn:
						if !cancelled {
							t.Error("settlement released the existing observer's ownership")
						}
					default:
					}
					f.scheduler.submissions.mu.Lock()
					retained := f.scheduler.submissions.owners[claimed.QueueSequence] == owner
					f.scheduler.submissions.mu.Unlock()
					current, err := f.store.Run(t.Context(), run.ID)
					if err != nil || !retained || current.Status != model.RunRunning || f.scheduler.pendingCreateCount() != 1 {
						t.Fatalf("existing owner or settlement obligation changed: run=%+v owner=%v err=%v", current, retained, err)
					}
				})
			}

		})
	}
}
