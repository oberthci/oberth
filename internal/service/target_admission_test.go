package service

import (
	"context"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type targetAdmissionJobs struct {
	JobController
	request WorkflowRequest
	legacy  bool
}

func (j *targetAdmissionJobs) AwaitRunAdmission(_ context.Context, r WorkflowRequest) error {
	j.request = r
	return nil
}
func (j *targetAdmissionJobs) AwaitAdmission(context.Context) error { j.legacy = true; return nil }
func TestSchedulerAdmissionPassesCanonicalRunToSelectedTarget(t *testing.T) {
	jobs := &targetAdmissionJobs{}
	scheduler := &Scheduler{jobs: jobs}
	r := WorkflowRequest{Run: model.Run{ID: "run", RepoID: 7, SHA: "exact-source"}, WorkflowName: "workflow"}
	if err := scheduler.awaitAdmission(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if jobs.legacy || jobs.request.Run.ID != r.Run.ID || jobs.request.WorkflowName != r.WorkflowName {
		t.Fatal("target-aware admission silently used local gate")
	}
	if err := scheduler.awaitAdmission(t.Context()); err != nil || !jobs.legacy {
		t.Fatal("legacy admission changed", err)
	}
}
