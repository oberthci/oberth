package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"k8s.io/utils/ptr"

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

type sizingAuditor struct {
	stubAuditor
	rows  []model.StepResourceUsage
	calls int
}

func (a *sizingAuditor) CPURequestHistory(_ context.Context, _ int64, _ string) ([]model.StepResourceUsage, error) {
	a.calls++
	return a.rows, nil
}

func TestCPUHistoryRetiredTemplatesCannotInvalidateCurrentPolicy(t *testing.T) {
	a := &sizingAuditor{}
	start := time.Unix(1000, 0)
	end := start.Add(100 * time.Second)
	for template := range 300 {
		for run := range 5 {
			a.rows = append(a.rows, model.StepResourceUsage{RunID: fmt.Sprint(run), TemplateName: fmt.Sprintf("template-%d", template), ContainerName: "main", Terminal: true, ExitCode: ptr.To(int32(0)), CPUTimeNS: ptr.To(int64(time.Second)), CPUTimeAt: end, ContainerStartedAt: start, ContainerFinishedAt: end, CPUCoverage: model.MetricCoverage{Count: 4, FirstAt: start, LastAt: end}})
		}
	}
	jobs := &ArgoJobs{auditor: a}
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{{Name: "template-1"}}}}
	policy := jobs.cpuPolicy(t.Context(), model.Run{RepoID: 1}, argojob.Config{StepCPURequestMaxMilli: 3000}, wf)
	if !policy.Valid() || len(policy.Templates) != 1 || policy.Templates["template-1"] != 50 {
		t.Fatal("history invalidated policy", policy)
	}
}
