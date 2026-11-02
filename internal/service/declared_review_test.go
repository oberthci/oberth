package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type fallbackJobs struct {
	declaredJobs
	graph bool
}

func (j fallbackJobs) PipelineSize(context.Context, JobRequest) (pipeline.Size, error) {
	return pipeline.M, nil
}
func (j fallbackJobs) PipelineKubeVirt(ctx context.Context, req JobRequest) (KubeVirtResources, error) {
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

func TestDeclaredGraphAndBudgetFallbackReachSubmission(t *testing.T) {
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
			select {
			case id := <-jobs.admitted:
				if id != run.ID || !strings.Contains(f.scheduler.ResourceAdmissionMessage(id), "legacy size") {
					t.Fatal("fallback was refused or lost its notice")
				}
			case <-time.After(testStartTimeout(10 * time.Second)):
				t.Fatal("fallback never reached submission")
			}
		})
	}
}

func TestDeclaredBudgetFallbackAdmitsAndExplains(t *testing.T) {
	g := newWeightedAdmission(6)
	r := g.reserve(1)
	release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: strings.Repeat("a", 32), Size: pipeline.S, Pods: &argoworkflow.PodDemand{CPUMilli: 8000, MemoryBytes: 16 << 30, Pods: 2}})
	defer release()
	if r.pods != nil || r.weight != 1 || r.fallback != model.ResourceFallbackBudget {
		t.Fatalf("not legacy: %+v", r)
	}
	api := &API{admission: &Scheduler{admission: g}}
	runs := []model.Run{{ID: r.runID, DisplayReason: "Priority normal: ordinary run."}}
	api.applyResourceMessages(t.Context(), runs)
	if !strings.Contains(runs[0].ResourceMessage, "legacy size") || runs[0].DisplayReason != "Priority normal: ordinary run." {
		t.Fatal("fallback notice missing or replaced reason")
	}
}

func TestFallbackSurvivesUncertainCreateRecovery(t *testing.T) {
	for _, reason := range []model.ResourceAdmissionFallback{model.ResourceFallbackGraph, model.ResourceFallbackMemory, model.ResourceFallbackRequests} {
		t.Run(string(reason), func(t *testing.T) {
			f := newConcurrencyFixture(t, pipeline.M, 3)
			run := model.Run{ID: strings.Repeat("a", 32), JobName: "step-abc12345", SHA: strings.Repeat("b", 40), RepoID: f.repo.ID, QueueSequence: 1}
			if err := f.scheduler.recordResourceAdmission(run, run.JobName, pipeline.M, argoworkflow.VMIDemand{}, nil, reason); err != nil {
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
