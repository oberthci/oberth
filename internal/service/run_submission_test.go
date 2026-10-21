package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type submissionInspectingJobs struct {
	*fakeJobs
	inspect func(context.Context, JobRequest) error
}

func (jobs *submissionInspectingJobs) CreateCI(ctx context.Context, request JobRequest) error {
	if err := jobs.inspect(ctx, request); err != nil {
		return err
	}
	return jobs.fakeJobs.CreateCI(ctx, request)
}

func TestSchedulerStartsRunAtWeightedAdmission(t *testing.T) {
	fixture := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed"})
	observed := false
	fixture.scheduler.jobs = &submissionInspectingJobs{fakeJobs: fixture.jobs, inspect: func(ctx context.Context, request JobRequest) error {
		observed = true
		run, err := fixture.store.Run(ctx, request.Run.ID)
		if err != nil {
			return err
		}
		if run.StartedAt == nil || run.Phase != "waiting" {
			t.Fatalf("admitted run has no start time before workflow creation: %#v", run)
		}
		return nil
	}}
	ctx := context.Background()
	enqueued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{
		EventID: "submission-start", Repository: fixture.repo, Branch: "feature/submission-start",
		SHA: strings.Repeat("a", 40), Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := fixture.store.Run(ctx, enqueued.ID)
	if err != nil || !observed || run.StartedAt == nil || run.StartedAt.Before(run.QueuedAt) {
		t.Fatalf("weighted admission did not start run: %#v, observed=%v, err=%v", run, observed, err)
	}
}
