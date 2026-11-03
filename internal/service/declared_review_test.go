package service

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type fallbackJobs struct {
	declaredJobs
	graph bool
}

func (j fallbackJobs) PipelineSize(context.Context, WorkflowRequest) (pipeline.Size, error) {
	return pipeline.M, nil
}
func (j fallbackJobs) PipelineKubeVirt(ctx context.Context, req WorkflowRequest) (KubeVirtResources, error) {
	if !j.graph {
		return j.declaredJobs.PipelineKubeVirt(ctx, req)
	}
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  activeDeadlineSeconds: 3600
  entrypoint: step-abc12345
  onExit: step-11111111
  templates:
  - name: step-abc12345
    container: &step
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      resources:
        requests: {cpu: 1, memory: 1Gi}
        limits: {memory: 1Gi}
  - name: step-11111111
    container: *step
`)
	r, err := argoworkflow.ResolveRunSource(source, nil, argoworkflow.RunSourceBinding{RunID: req.Run.ID, RepositoryID: req.Repository.ID, SHA: req.Run.SHA, File: ".oberth/build.yaml", Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		return KubeVirtResources{}, err
	}
	receipt, err := r.Receipt(argoworkflow.Policy{RunnerImagePrefixes: []string{"example.invalid/"}})
	return KubeVirtResources{Receipt: receipt}, err
}

func TestDeclaredGraphAndBudgetAreRefusedBeforeSubmission(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget", true: "graph"}[graph], func(t *testing.T) {
			f := newConcurrencyFixture(t, pipeline.M, 3)
			jobs := newConcurrentJobs(pipeline.M, 2)
			f.scheduler.jobs = fallbackJobs{declaredJobs: declaredJobs{jobs}, graph: graph}
			if !graph {
				f.scheduler.admission.resources.CPUMilli = 1000
			}
			run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "step-abc12345", Repository: f.repo, Branch: "step-abc12345", SHA: strings.Repeat("a", 40), Actor: "repo-01"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- f.scheduler.Run(ctx) }()
			defer func() { close(jobs.release); cancel(); <-done }()
			failed := awaitRunStatus(t, f, run.ID, model.RunFailed)
			want := "numeric budget"
			if graph {
				want = "graph"
			}
			if !strings.Contains(failed.Error, want) {
				t.Fatalf("wrong refusal: %s", failed.Error)
			}
			select {
			case <-jobs.admitted:
				t.Fatal("unsafe declared workflow submitted")
			default:
			}

		})
	}
}

func TestFallbackSurvivesUncertainCreateRecovery(t *testing.T) {
	for _, reason := range []model.ResourceAdmissionFallback{model.ResourceFallbackGraph, model.ResourceFallbackMemory, model.ResourceFallbackRequests} {
		t.Run(string(reason), func(t *testing.T) {
			f := newConcurrencyFixture(t, pipeline.M, 3)
			run := model.Run{ID: strings.Repeat("a", 32), WorkflowName: "step-abc12345", SHA: strings.Repeat("b", 40), RepoID: f.repo.ID, QueueSequence: 1}
			if err := f.scheduler.recordResourceAdmission(run, run.WorkflowName, pipeline.M, argoworkflow.VMIDemand{}, nil, reason); err != nil {
				t.Fatal(err)
			}
			r, err := f.scheduler.restoreCreateGrant(run, nil)
			if err != nil || r.fallback != reason || r.pods != nil || r.weight != 2 {
				t.Fatalf("restored fallback: %+v %v", r, err)
			}
		})
	}
}

func TestDeclaredVMAndSmallRunsShareButCellLimitRemains(t *testing.T) {
	g := newWeightedAdmission(6)
	vm := &KubeVirtResources{Demand: argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 4 << 30}, Limit: argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 4 << 30}}
	releaseVM := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{RunID: "step-abc12345", RepoID: 1, Size: pipeline.XL, KubeVirt: vm, Pods: &argoworkflow.PodDemand{CPUMilli: 2600, MemoryBytes: (5 << 30) + (256 << 20), Pods: 1}})
	defer releaseVM()
	releaseSmall := acquireRequestWithinBudget(t, g.reserve(2), admissionRequest{RunID: "step-11111111", RepoID: 1, Size: pipeline.M, Pods: &argoworkflow.PodDemand{CPUMilli: 500, MemoryBytes: 1 << 30, Pods: 1}})
	defer releaseSmall()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.runs != 2 {
		t.Fatal("VM run remained exclusive")
	}
	if g.kubeVirtFitsLocked(&admissionReservation{repoID: 1, kubeVirt: *vm}) {
		t.Fatal("host admission bypassed independent cell limit")
	}
}
