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
	"github.com/oberthci/oberth/pkg/argoworkflow"
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
	if err != nil || demand.CPUMilli != 155 {
		t.Fatalf("admission did not pack declared floor 105m + 50m executor: %+v %v", demand, err)
	}
	if err = jobs.CreateWorkflow(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	wf, err := argojob.Build(config, c.request)
	if err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range wf.Spec.Templates {
		if tmpl.Name == "main" && (tmpl.Container.Resources.Requests.Cpu().MilliValue() != 105 || tmpl.Container.Resources.Limits.Cpu().MilliValue() != 4000) {
			t.Fatal("submission changed frozen requests or burst limit", tmpl.Container.Resources)
		}
	}
	if a.calls != 1 {
		t.Fatalf("history was reread after reservation: %d", a.calls)
	}
}

func TestCPUAdmissionDisabledSkipsHistorySizing(t *testing.T) {
	a := &sizingAuditor{}
	jobs := &ArgoJobs{auditor: a}
	policy := jobs.cpuPolicy(t.Context(), model.Run{}, argojob.Config{CPUAdmissionDisabled: true, StepCPURequestMaxMilli: 3000}, nil)
	if a.calls != 0 || policy.MaxMilli != 0 || len(policy.Templates) != 0 {
		t.Fatal("off switch retained measured overrides", policy, a.calls)
	}
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

func TestWideFanoutFreezesArgoParallelismToPoolCapacity(t *testing.T) {
	source := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: root
  templates:
  - name: root
    steps:
    - - name: work
        template: work
        withItems: [1,2,3,4,5,6,7,8,9,10]
  - name: work
    container:
      image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      command: [/bin/true]
      resources:
        requests: {cpu: 1, memory: 256Mi}
        limits: {cpu: 4, memory: 1Gi}
`
	wf, err := argoworkflow.Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	jobs := &ArgoJobs{}
	config := argojob.Config{StepCPURequestMaxMilli: 3000, CPUAdmission: argoworkflow.CPUAdmission{Milli: 9600}}
	policy := jobs.cpuPolicy(t.Context(), model.Run{}, config, wf)
	if policy.Parallelism != 9 {
		t.Fatal("fanout not bounded before admission", policy)
	}
	demand, err := argojob.SchedulingDemand(wf, policy)
	if err != nil || demand.CPUMilli != 9450 {
		t.Fatalf("effective demand: %+v %v", demand, err)
	}
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", source)
	c := &routeController{}
	config.Namespace = "pipelines"
	config.PipelineServiceAccount = "pipeline"
	config.CredentialedServiceAccount = "release"
	config.CISecretsServiceAccount = "ci"
	config.ExecutorServiceAccount = "executor"
	jobs, err = NewArgoJobs(c, config, &stubAuditor{}, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := service.WorkflowRequest{WorkflowName: "fanout", Run: model.Run{ID: "fanout", Actor: "operator@example.test", RepoID: 7, Trigger: "branch", RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("b", 40)}, Repository: model.Repository{ID: 7, Name: "repo"}, SourceDir: dir}
	request.DemandReceipt, err = jobs.PipelineDemandReceipt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = jobs.CreateWorkflow(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	built, err := argojob.Build(config, c.request)
	if err != nil || built.Spec.Parallelism == nil || *built.Spec.Parallelism != 9 {
		t.Fatalf("submitted fanout not bounded: %v %v", built, err)
	}
}

func TestRecoveredCapacityPolicyFreezesBeforeSubmissionAudit(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", groupedPipeline(""))
	current := argoworkflow.CPUAdmission{Pool: "cluster", HoldReason: "node capacity unavailable"}
	config := argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor", CPUCapacity: func(context.Context) argoworkflow.CPUAdmission { return current }}
	c := &routeController{}
	jobs, err := NewArgoJobs(c, config, &stubAuditor{}, emptySecretAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := service.WorkflowRequest{WorkflowName: "recovered", Run: model.Run{ID: "recovered", Actor: "operator@example.test", RepoID: 7, Trigger: "branch", RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("b", 40)}, Repository: model.Repository{ID: 7, Name: "repo"}, SourceDir: dir}
	request.DemandReceipt, err = jobs.PipelineDemandReceipt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if request.DemandReceipt.CPURequestPolicy().MaxMilli != 0 {
		t.Fatal("test requires unavailable preparation capacity")
	}
	current = argoworkflow.CPUAdmission{Pool: "cluster", Milli: 3200, NodeMilli: 4000, StepMaxMilli: 1000}
	if err = jobs.CreateWorkflow(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	// The request retained by audit/controller creation must not change if a
	// capacity refresh occurs between the two native Build calls.
	current.StepMaxMilli = 500
	built, err := argojob.Build(config, c.request)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range built.Spec.Templates {
		if template.Name == "main" && template.Container.Resources.Requests.Cpu().MilliValue() != 1000 {
			t.Fatal("submitted CPU policy changed after freeze", template.Container.Resources)
		}
	}
}
