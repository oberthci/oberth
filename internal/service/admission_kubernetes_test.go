package service

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestKubernetesPlacementIgnoresDeclaredAdmissionBudget(t *testing.T) {
	f := newConcurrencyFixture(t, pipeline.XL, 10)
	g := f.scheduler.admission
	// Kubernetes may keep these pods Pending. Oberth must submit their workflows
	// rather than refuse them against its historical 8-CPU/16-GiB pool.
	for i := int64(1); i <= 10; i++ {
		r := g.reserve(i)
		release, err := r.acquireRequest(t.Context(), admissionRequest{Size: pipeline.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 32000, MemoryBytes: 64 << 30, Pods: 1}})
		if err != nil {
			t.Fatalf("workflow %d refused before Kubernetes placement: %v", i, err)
		}
		defer release()
	}
	if state := f.scheduler.AdmissionState(); state.Runs != 10 || state.NumericBudget != nil || state.Blocker != nil {
		t.Fatalf("application resource accounting still controls placement: %+v", state)
	}
}

func TestKubernetesPlacementIncident2125HasNoResourceDrain(t *testing.T) {
	g := newWorkflowAdmissionForJobs(10)
	g.resources.CPUMilli, g.resources.MemoryBytes = 40000, 128<<30
	g.maxHeadWait = 20 * time.Minute // Historical settings cannot restore draining.
	now := time.Now()
	for i, cpu := range []int64{18750, 4000, 11350, 12800, 1100} {
		r := g.reserve(int64(i + 1))
		release := acquireRequestWithinBudget(t, r, admissionRequest{Size: pipeline.XL, HeadSince: now.Add(-90 * time.Minute), Pods: &argoworkflow.PodDemand{CPUMilli: cpu, MemoryBytes: 32 << 30, Pods: 1}})
		defer release()
		view := (&Scheduler{admission: g}).AdmissionSnapshot(r.sequence)
		if view.State != "admitted" || view.NumericBudget != nil || view.BlockedBy != nil || view.BackfillMessage != "" {
			t.Fatalf("incident resource drain survived Kubernetes placement: %+v", view)
		}
	}
	if g.runs != 5 {
		t.Fatal("workflow head or backfill was held by resource arithmetic")
	}
}

func TestKubernetesPlacementHardPSIHasNoPriorityOrTimeoutBypass(t *testing.T) {
	g := newWorkflowAdmissionForJobs(10)
	now := time.Now()
	g.now = func() time.Time { return now }
	config := DefaultAdmissionPressureConfig()
	config.HardOnly = true
	io := 41.0
	config.Source = func() (model.PressureSample, error) {
		return model.PressureSample{IO: model.PressureResource{SomeAvg10: io}}, nil
	}
	config.Logf = func(string, ...any) {}
	g.pressure = newPressureCheck(config)
	r := pending(t, g, 1, admissionRequest{Size: pipeline.S, RefKind: model.RefTag})
	defer r.cancel()
	now = now.Add(2 * time.Hour)
	kick(g)
	if granted(g, r) || (&Scheduler{admission: g}).QueuedAdmissionMessage().Text != "host IO busy" {
		t.Fatal("hard pressure was bypassed by idle tag or elapsed defer")
	}
	io = 0
	kick(g)
	if !granted(g, r) {
		t.Fatal("hard valve did not reopen after pressure recovered")
	}
	r.release()
}

func TestKubernetesPlacementIgnoresSoftPressureAndSettling(t *testing.T) {
	g := newWorkflowAdmissionForJobs(10)
	config := DefaultAdmissionPressureConfig()
	config.HardOnly = true
	config.Source = func() (model.PressureSample, error) {
		return model.PressureSample{CPU: model.PressureResource{SomeAvg10: 40}, IO: model.PressureResource{SomeAvg10: 30}}, nil
	}
	config.Logf = func(string, ...any) {}
	g.pressure = newPressureCheck(config)
	for i := int64(1); i <= 10; i++ {
		release := acquireRequestWithinBudget(t, g.reserve(i), admissionRequest{Size: pipeline.XL})
		defer release()
	}
	r := pending(t, g, 11, admissionRequest{Size: pipeline.S})
	defer r.cancel()
	if g.runs != 10 || (&Scheduler{admission: g}).QueuedAdmissionMessage().Text != "10 of 10 runs running" {
		t.Fatal("ceiling bypassed or soft pressure blocked fitting workflows")
	}
}
