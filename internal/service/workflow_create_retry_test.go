package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

type workflowCreateFailureJobs struct{ *fakeJobs }

func (*workflowCreateFailureJobs) CreateWorkflow(context.Context, WorkflowRequest) error {
	return fmt.Errorf("argojob: create Workflow exhausted: %w", context.DeadlineExceeded)
}
func (jobs *workflowCreateFailureJobs) CreateRelease(ctx context.Context, request WorkflowRequest) error {
	return jobs.CreateWorkflow(ctx, request)
}

func TestWorkflowCreateExhaustionIsInfrastructureAndRunRetryApplies(t *testing.T) {
	fixture := newControlFixture(t)
	// Manual run_rerun remains available when the administrator disables automatic recovery.

	fixture.scheduler.jobs = &workflowCreateFailureJobs{fixture.jobs}
	source, err := fixture.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "create-timeout", Repository: fixture.repo, Branch: "feature/create-timeout", SHA: strings.Repeat("a", 40), Actor: "pusher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := fixture.store.Run(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != model.RunFailed || failed.Phase != "infrastructure" || failed.FailedStep != "" || !strings.Contains(failed.Error, context.DeadlineExceeded.Error()) {
		t.Fatalf("create failure became product failure: %+v", failed)
	}
	control := fixture.api(t)
	control.runReruns = fixture.store
	control.refs = stubRefResolver{branches: map[string]map[string]string{control.cacheInputFor(t.Context(), fixture.repo): {source.Ref: source.SHA}}}
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "operator@host"}, "run_rerun", json.RawMessage(fmt.Sprintf(`{"id":%q}`, failed.ID)))
	if err != nil {
		t.Fatal(err)
	}
	retried, err := fixture.store.Run(t.Context(), value.(api.RunRerunResponse).RunID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != model.RunRunning || retried.ID == source.ID || retried.SHA != source.SHA || retried.Ref != source.Ref {
		t.Fatalf("retry lost source identity: %+v", retried)
	}
	if len(fixture.jobs.createdCI) != 0 || len(fixture.jobs.deleted) != 0 {
		t.Fatal("uncertain create outcome treated as a confirmed Workflow")
	}
}

type uncertainWorkflowCreateError struct{}

func (*uncertainWorkflowCreateError) Error() string         { return "Workflow create outcome uncertain" }
func (*uncertainWorkflowCreateError) UncertainCreate() bool { return true }
