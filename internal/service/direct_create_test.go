package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestDirectCreateTwoPushesDoNotWaitForAnEarlierWorkflow(t *testing.T) {
	f := newGroupFixture(t, 0)
	request := CIRequest{EventID: "direct-one", Repository: f.terraform, Branch: "feature/direct", SHA: strings.Repeat("a", 40), Actor: "owner"}
	ctx, cancel := context.WithCancel(t.Context())
	first, err := f.scheduler.EnqueueCI(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // The accepted Workflow's observer outlives its request.
	request.EventID, request.SHA = "direct-two", strings.Repeat("b", 40)
	second, err := f.scheduler.EnqueueCI(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first.ID, second.ID} {
		run, err := f.store.Run(t.Context(), id)
		if err != nil || run.WorkflowName == "" || run.Phase != "running" {
			t.Fatalf("handler returned before submission: %+v %v", run, err)
		}
	}
	created := f.awaitCreated(t, 2)
	if !created[first.ID] || !created[second.ID] {
		t.Fatal(created)
	}
	duplicate, err := f.scheduler.EnqueueCI(t.Context(), request)
	if err != nil || !duplicate.Duplicate {
		t.Fatalf("receive replay=%+v %v", duplicate, err)
	}
	f.engine.mu.Lock()
	if len(f.engine.jobs) != 2 {
		t.Error("receive replay created a Workflow")
	}
	f.engine.mu.Unlock()
	for _, id := range []string{first.ID, second.ID} {
		f.engine.finish(t, id)
		f.awaitTerminal(t, id)
	}
}

type unavailableCreateJobs struct {
	*fakeJobs
	attempts int
}

func (j *unavailableCreateJobs) CreateWorkflow(context.Context, WorkflowRequest) error {
	j.attempts++
	return errors.New("cluster API unavailable; request another attempt explicitly")
}

func TestDirectCreateAPIFailureRecordsReasonAndNeverResubmits(t *testing.T) {
	f := newControlFixture(t)
	jobs := &unavailableCreateJobs{fakeJobs: f.jobs}
	f.scheduler.jobs = jobs
	accepted, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "direct-api-down", Repository: f.repo, Branch: "feature/down", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.Run(t.Context(), accepted.ID)
	if err != nil || current.Status != model.RunFailed || !strings.Contains(current.Error, "cluster API unavailable") {
		t.Fatalf("missing create failure: %+v %v", current, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.scheduler.Run(ctx) }()
	for !f.scheduler.Recovered() {
		select {
		case err := <-done:
			t.Fatal(err)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if jobs.attempts != 1 {
		t.Fatalf("create attempts=%d", jobs.attempts)
	}
}

type cutCreateJobs struct {
	*pendingCreateJobs
	entered  chan struct{}
	accepted bool
}

func (j *cutCreateJobs) CreateWorkflow(ctx context.Context, request WorkflowRequest) error {
	close(j.entered)
	<-ctx.Done()
	if j.accepted {
		return j.fakeJobs.CreateWorkflow(context.WithoutCancel(ctx), request)
	}
	return ctx.Err()
}

func TestDirectCreateRequestCancellationDoesNotStrandHistory(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunFailed, Phase: "job", Error: "real Workflow result"})
			jobs := &cutCreateJobs{pendingCreateJobs: &pendingCreateJobs{fakeJobs: f.jobs}, entered: make(chan struct{}), accepted: accepted}
			f.scheduler.jobs = jobs
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan model.EnqueueRunResult, 1)
			go func() {
				row, _ := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "request-cut", Repository: f.repo, Branch: "feature/cut", SHA: strings.Repeat("a", 40), Actor: "owner"})
				done <- row
			}()
			<-jobs.entered
			cancel()
			row := <-done
			finished := waitForFixtureRunStatus(t, f, row.ID, model.RunFailed)
			if accepted {
				if finished.Error != "real Workflow result" {
					t.Fatalf("accepted Workflow lost its observer: %+v", finished)
				}
			} else if !strings.Contains(finished.Error, context.Canceled.Error()) {
				t.Fatalf("request failure reason missing: %+v", finished)
			}
		})
	}
}

func TestDirectCreateStoreFailureLeavesAnotherWorkflowObserved(t *testing.T) {
	f := newGroupFixture(t, 0)
	fault := &deadlineFaultStore{Store: f.store, failures: map[string]int{}, calls: map[string]int{}, faultError: store.ErrInvalidState}
	f.scheduler.store = fault
	first, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "healthy-direct", Repository: f.terraform, Branch: "feature/healthy", SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	fault.mu.Lock()
	fault.failures["StartRun"] = 1
	fault.mu.Unlock()
	second, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "invalid-direct", Repository: f.terraform, Branch: "feature/invalid", SHA: strings.Repeat("b", 40), Actor: "owner"})
	if !errors.Is(err, store.ErrInvalidState) || second.ID == "" {
		t.Fatalf("lost rejected run: %+v %v", second, err)
	}
	failed, err := f.store.Run(t.Context(), second.ID)
	if err != nil || failed.Status != model.RunFailed || failed.WorkflowName != "" {
		t.Fatalf("invalid request created work: %+v %v", failed, err)
	}
	f.engine.finish(t, first.ID)
	f.awaitTerminal(t, first.ID)
}
