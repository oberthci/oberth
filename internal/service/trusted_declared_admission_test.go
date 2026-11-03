package service

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// The release declares a small numeric demand, but its trusted build preflight
// needs L. Both executions must keep the preflight's larger size reservation.
type contractDeclaredJobs struct{ *fakeJobs }

func (j contractDeclaredJobs) PipelineKubeVirt(ctx context.Context, req WorkflowRequest) (KubeVirtResources, error) {
	return (declaredJobs{}).PipelineKubeVirt(ctx, req)
}

type checkingDeclaredPreflight struct {
	*protectedFixture
	t         *testing.T
	scheduler *Scheduler
	size      pipeline.Size
	calls     int
	retry     bool
}

func (p *checkingDeclaredPreflight) Size(context.Context, WorkflowRequest) (pipeline.Size, error) {
	return p.size, nil
}
func (p *checkingDeclaredPreflight) Execute(ctx context.Context, job WorkflowRequest, requirement vmrunner.Requirement, output io.Writer) error {
	p.calls++
	if job.PodDemand != nil || job.Size != p.size {
		p.t.Errorf("trusted request: demand=%+v size=%s, want legacy %s", job.PodDemand, job.Size, p.size)
	}
	p.scheduler.admission.mu.Lock()
	found := false
	for _, r := range p.scheduler.admission.reservations {
		if r.runID == job.Run.ID {
			found = true
			if r.pods != nil || r.size != p.size || r.weight != p.scheduler.admission.weights.of(p.size, p.scheduler.admission.capacity) || !r.granted {
				p.t.Errorf("trusted reservation: demand=%+v size=%s weight=%d granted=%v", r.pods, r.size, r.weight, r.granted)
			}
		}
	}
	p.scheduler.admission.mu.Unlock()
	if !found {
		p.t.Error("trusted preflight lacks a reservation")
	}
	if p.retry && p.calls == 1 {
		return store.ErrVMCapacity
	}
	return p.protectedFixture.Execute(ctx, job, requirement, output)
}

func TestTrustedDeclaredRunKeepsPreflightSizeAdmission(t *testing.T) {
	for _, mode := range []string{"release", "branch", "disabled", "capacity retry", "ungranted XL"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "Succeeded"})
			contract := []byte("version: 2\nprofile: kvm-suite-amd64-v1\nsuite_sha256: " + strings.Repeat("a", 64) + "\nguest_sha256: " + strings.Repeat("b", 64) + "\nartifacts: [build/program]\n")
			f.scheduler.git = &policyTestGit{controlGit: f.git, candidate: contract, baseline: contract}
			f.jobs.size = pipeline.S
			f.scheduler.jobs = contractDeclaredJobs{f.jobs}
			executor := &checkingDeclaredPreflight{protectedFixture: &protectedFixture{store: f.store}, t: t, scheduler: f.scheduler, size: pipeline.L, retry: mode == "capacity retry"}
			if mode == "ungranted XL" {
				executor.size = pipeline.XL
			}
			f.scheduler.trusted = executor
			f.scheduler.admission.resources.Disabled = mode == "disabled"
			f.scheduler.capacityWait = func(context.Context) error { return nil }
			var queued model.EnqueueRunResult
			var err error
			if mode == "branch" || mode == "ungranted XL" {
				queued, err = f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "trusted-declared", Repository: f.repo, Branch: "feature/declared", SHA: strings.Repeat("c", 40), Actor: "agent@host"})
			} else {
				queued, err = f.scheduler.AdmitRelease(ctx, ReleaseRequest{EventID: "trusted-declared", Repository: f.repo, Tag: "v1.0.0", ObjectSHA: strings.Repeat("d", 40), CommitSHA: strings.Repeat("c", 40), Actor: "agent@host"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.scheduler.ProcessNext(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := f.store.Run(ctx, queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ungranted XL" {
				if run.Status != model.RunFailed || executor.calls != 0 || !strings.Contains(run.Error, "size") {
					t.Fatalf("ungranted trusted preflight bypassed size authorization: calls=%d run=%+v", executor.calls, run)
				}
				return
			}
			wantCalls := 1
			if mode == "capacity retry" {
				wantCalls = 2
			}
			if run.Status != model.RunPassed || executor.calls != wantCalls {
				t.Fatalf("trusted declared run: calls=%d want=%d run=%+v", executor.calls, wantCalls, run)
			}
		})
	}
}
