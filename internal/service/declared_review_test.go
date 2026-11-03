package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type fallbackJobs struct {
	declaredJobs
	graph bool
}

func (j fallbackJobs) PipelineSize(context.Context, JobRequest) (periapsis.Size, error) {
	return periapsis.M, nil
}
func (j fallbackJobs) PipelineDemandReceipt(ctx context.Context, req JobRequest) (*argoworkflow.DemandReceipt, error) {
	if !j.graph {
		return j.declaredJobs.PipelineDemandReceipt(ctx, req)
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
		return nil, err
	}
	receipt, err := r.Receipt(argoworkflow.Policy{RunnerImagePrefixes: []string{"example.invalid/"}})
	return receipt, err
}

func TestDeclaredGraphAndBudgetFallbackReachSubmission(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget", true: "graph"}[graph], func(t *testing.T) {
			f := newConcurrencyFixture(t, periapsis.M, 3)
			jobs := newConcurrentJobs(periapsis.M, 2)
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
	release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: strings.Repeat("a", 32), Size: periapsis.S, Pods: &argoworkflow.PodDemand{CPUMilli: 8000, MemoryBytes: 16 << 30, Pods: 2}})
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
			f := newConcurrencyFixture(t, periapsis.M, 3)
			run := model.Run{ID: strings.Repeat("a", 32), JobName: "step-abc12345", SHA: strings.Repeat("b", 40), RepoID: f.repo.ID, QueueSequence: 1}
			if err := f.scheduler.recordResourceAdmission(run, run.JobName, periapsis.M, nil, reason); err != nil {
				t.Fatal(err)
			}
			r, err := f.scheduler.restoreCreateGrant(run, nil)
			if err != nil || r.fallback != reason || r.pods != nil || r.weight != 2 {
				t.Fatalf("restored fallback: %+v %v", r, err)
			}
		})
	}
}
