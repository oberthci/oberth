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

type declaredJobs struct{ *concurrentJobs }

func (j declaredJobs) PipelineKubeVirt(_ context.Context, req WorkflowRequest) (KubeVirtResources, error) {
	source := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations: {oberth.ci/size: XL}
spec:
  activeDeadlineSeconds: 3600
  entrypoint: step-abc12345
  templates:
  - name: step-abc12345
    container:
      image: example.invalid/repo-01@sha256:` + strings.Repeat("a", 64) + `
      resources:
        requests: {cpu: 1000m, memory: 1Gi}
        limits: {memory: 1Gi}
`)
	resolved, err := argoworkflow.ResolveRunSource(source, nil, argoworkflow.RunSourceBinding{RunID: req.Run.ID, RepositoryID: req.Repository.ID, SHA: req.Run.SHA, File: ".oberth/build.yaml", Trigger: argoworkflow.RunTriggerBranch})
	if err != nil {
		return KubeVirtResources{}, err
	}
	receipt, err := resolved.Receipt(argoworkflow.Policy{RunnerImagePrefixes: []string{"example.invalid/"}})
	return KubeVirtResources{Receipt: receipt}, err
}

func TestDeclaredSubmissionIgnoresSizeGrantAndKillSwitchRestoresIt(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "declared", true: "disabled"}[disabled], func(t *testing.T) {
			fixture := newConcurrencyFixture(t, pipeline.XL, 3)
			jobs := newConcurrentJobs(pipeline.XL, 2)
			fixture.scheduler.jobs = declaredJobs{jobs}
			fixture.scheduler.admission.resources.Disabled = disabled
			run, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "step-abc12345", Repository: fixture.repo, Branch: "step-abc12345", SHA: strings.Repeat("a", 40), Actor: "repo-01"})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- fixture.scheduler.Run(ctx) }()
			defer func() { close(jobs.release); cancel(); <-done }()
			if disabled {
				failed := awaitRunStatus(t, fixture, run.ID, model.RunFailed)
				if !strings.Contains(failed.Error, "may run at most L") {
					t.Fatal(failed.Error)
				}
				return
			}
			select {
			case id := <-jobs.admitted:
				if id != run.ID {
					t.Fatal("wrong run admitted")
				}
				fixture.scheduler.admission.mu.Lock()
				for _, r := range fixture.scheduler.admission.reservations {
					if r.runID == run.ID && (r.pods == nil || r.pods.CPUMilli != 1000 || r.weight != 0) {
						t.Error("declaration did not own admission")
					}
				}
				fixture.scheduler.admission.mu.Unlock()
			case <-time.After(testStartTimeout(10 * time.Second)):
				t.Fatal("declared XL must not require a size grant")
			}
		})
	}
}

func TestDeclaredCreateContractRestoresNumericReservationWithKillSwitch(t *testing.T) {
	f := newConcurrencyFixture(t, pipeline.M, 3)
	run := model.Run{ID: strings.Repeat("a", 32), WorkflowName: "step-abc12345", SHA: strings.Repeat("b", 40), RepoID: f.repo.ID, QueueSequence: 1}
	d := &argoworkflow.PodDemand{CPUMilli: 6000, MemoryBytes: 8 << 30, Pods: 2}
	if err := f.scheduler.recordCreateAdmission(run, run.WorkflowName, pipeline.S, argoworkflow.VMIDemand{}, d); err != nil {
		t.Fatal(err)
	}
	f.scheduler.admission.resources.Disabled = true
	r, err := f.scheduler.restoreCreateGrant(run, nil)
	if err != nil || r.pods == nil || *r.pods != *d || r.weight != 0 || !r.retained {
		t.Fatalf("restored grant: %+v %v", r, err)
	}
	// Already submitted declarations must remain reserved when a kill switch
	// sends NEW work back to legacy sizes, even if the old annotation was S.
	f.scheduler.admission.mu.Lock()
	fits := f.scheduler.admission.canGrantLocked(&admissionReservation{size: pipeline.M, weight: 2})
	f.scheduler.admission.mu.Unlock()
	if fits {
		t.Fatal("kill switch undercounted already-running declared work")
	}
	changed := *d
	changed.CPUMilli++
	if _, err := f.scheduler.admission.adopt(run, pipeline.S, argoworkflow.VMIDemand{}, &changed); err == nil {
		t.Fatal("conflicting restored declaration accepted")
	}
}
