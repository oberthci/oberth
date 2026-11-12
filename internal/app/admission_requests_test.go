package app

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

func TestSubmissionPropagatesFrozenAgedPriorityAndCPUReservation(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", groupedPipeline(""))
	controller := &routeController{}
	jobs, err := NewArgoJobs(controller, argojob.Config{StepCPURequestMaxMilli: 3000, Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}, &stubAuditor{}, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	request := service.WorkflowRequest{WorkflowName: "workflow", Run: model.Run{ID: "run", Actor: "operator@example.test", RepoID: 7, Trigger: "branch", RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("b", 40), Priority: model.PriorityStandard, QueuedAt: now.Add(-2 * time.Hour), StartedAt: &now}, Repository: model.Repository{ID: 7, Name: "repo"}, SourceDir: dir}
	request.DemandReceipt, err = jobs.PipelineDemandReceipt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := jobs.AdmissionRequests(t.Context(), request)
	if err != nil || requests == nil || requests.CPUMilli != 2050 {
		t.Fatalf("requests=%+v err=%v", requests, err)
	}
	if err = jobs.CreateWorkflow(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if controller.request.Priority != model.PriorityPromotion {
		t.Fatal("aged priority lost at Argo handoff", controller.request.Priority)
	}
}
