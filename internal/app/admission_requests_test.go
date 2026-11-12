package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
func TestCPUHistorySizingPrecedesAdmissionAndIsFrozenForSubmission(t *testing.T) {
	dir := t.TempDir()
	source := strings.Replace(groupedPipeline(""), `        command: ["echo", "ok"]`, `        command: ["echo", "ok"]
        resources:
          requests: {cpu: 1050m, memory: 1Gi}
          limits: {cpu: 4, memory: 2Gi}`, 1)
	writePipeline(t, dir, "build.yaml", source)
	a := &sizingAuditor{}
	start := time.Unix(1000, 0)
	end := start.Add(100 * time.Second)
	for i := range 20 {
		a.rows = append(a.rows, model.StepResourceUsage{RunID: fmt.Sprint(i), TemplateName: "main", ContainerName: "main", Terminal: true, ExitCode: ptr.To(int32(0)), CPUTimeNS: ptr.To(int64(time.Second)), CPUTimeAt: end, ContainerStartedAt: start, ContainerFinishedAt: end, CPUCoverage: model.MetricCoverage{Count: 4, FirstAt: start, LastAt: end}})
	}
	c := &routeController{}
	config := argojob.Config{StepCPURequestMaxMilli: 3000, Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor"}
	jobs, err := NewArgoJobs(c, config, a, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := service.WorkflowRequest{WorkflowName: "workflow", Run: model.Run{ID: "run", Actor: "operator@example.test", RepoID: 7, Trigger: "branch", RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("b", 40)}, Repository: model.Repository{ID: 7, Name: "repo"}, SourceDir: dir}
	request.DemandReceipt, err = jobs.PipelineDemandReceipt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	frozen := request.DemandReceipt.CPURequestPolicy()
	if frozen.Templates["main"] != 50 {
		t.Fatal("oversized 1050m request was not sized from 10m usage", frozen)
	}
	frozen.Templates["main"] = 3000
	a.rows = nil
	demand, err := jobs.AdmissionRequests(t.Context(), request)
	if err != nil || demand.CPUMilli != 100 {
		t.Fatalf("admission did not pack resized 50m + 50m executor: %+v %v", demand, err)
	}
	if err = jobs.CreateWorkflow(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	wf, err := argojob.Build(config, c.request)
	if err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range wf.Spec.Templates {
		if tmpl.Name == "main" && (tmpl.Container.Resources.Requests.Cpu().MilliValue() != 50 || tmpl.Container.Resources.Limits.Cpu().MilliValue() != 4000) {
			t.Fatal("submission changed frozen requests or burst limit", tmpl.Container.Resources)
		}
	}
	if a.calls != 1 {
		t.Fatalf("history was reread after reservation: %d", a.calls)
	}
}
