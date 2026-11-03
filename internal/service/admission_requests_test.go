package service

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	pipeline "github.com/oberthci/oberth/pkg/periapsis"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestDeclaredAdmissionTwoMeasuredRunsFit(t *testing.T) {
	// Each run has two simultaneous containers within XL per-container ceilings.
	container := corev1.Container{Name: "first", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("10Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("15Gi")},
	}}
	second := *container.DeepCopy()
	second.Name = "second"
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "run", Templates: []wfv1.Template{{Name: "run", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: container}, {Container: second}}}}}}}
	d, err := argoworkflow.DeclaredHostPeak(wf, argoworkflow.RunTriggerBranch)
	if err != nil || d == nil || d.MemoryBytes != 20<<30 {
		t.Fatalf("run demand = %+v, %v; want 20Gi requests", d, err)
	}
	g := newWeightedAdmission(6)
	g.resources.MemoryBytes = 48 << 30
	for i := int64(1); i <= 2; i++ {
		r := g.reserve(i)
		release := acquireRequestWithinBudget(t, r, admissionRequest{Size: pipeline.XL, Pods: d})
		defer release()
		if r.pods == nil || r.weight != 0 {
			t.Fatal("measured run fell back to legacy XL")
		}
		if got := r.declaredResources().MemoryBytes; got != (21<<30)+(256<<20) {
			t.Fatalf("charged %d bytes; want 20Gi requests + 1Gi headroom + 256Mi executor", got)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.numericFitsLocked(&admissionReservation{pods: d}) {
		t.Fatal("third measured run exceeded coarse memory ceiling")
	}
	// The former limit-based cost cannot pack a second run into this budget.
	old := *d
	old.MemoryBytes = 30 << 30
	oldGate := newWeightedAdmission(6)
	oldGate.resources = g.resources
	oldGate.resources.MemoryHeadroomBytes = 0 // The former rule had only executor reserves.
	oldGate.reservations[1] = &admissionReservation{pods: &old, granted: true}
	if oldGate.numericFitsLocked(&admissionReservation{pods: &old}) {
		t.Fatal("old limit-based reservations unexpectedly fit")
	}
}

func TestDeclaredMemoryHeadroomChargeAndBudget(t *testing.T) {
	for _, headroom := range []int64{0, 1 << 30, 3 << 30} {
		g := newWeightedAdmission(6)
		g.resources.MemoryHeadroomBytes = headroom
		d := &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 2 << 30, Pods: 3}
		// The fixed headroom is charged once, while helper reserves scale by pods.
		want := (2 << 30) + 3*executorMemoryBytes + headroom
		g.resources.MemoryBytes = want
		r := g.reserve(1)
		release := acquireRequestWithinBudget(t, r, admissionRequest{Size: pipeline.XL, Pods: d})
		if r.pods == nil || r.declaredResources().MemoryBytes != want {
			t.Fatalf("headroom %d: wrong reservation", headroom)
		}
		release()
		g.resources.MemoryBytes--
		r = g.reserve(2)
		release = acquireRequestWithinBudget(t, r, admissionRequest{Size: pipeline.S, Pods: d})
		if r.pods != nil || r.weight != 1 {
			t.Fatalf("headroom %d: oversize demand did not fall back", headroom)
		}
		release()
	}
	for _, headroom := range []int64{-1, (1 << 50) + 1} {
		c := DefaultDeclaredAdmissionConfig()
		c.MemoryHeadroomBytes = headroom
		if c.Validate() == nil {
			t.Fatalf("invalid headroom %d accepted", headroom)
		}
	}
}

func TestDeclaredMemoryHeadroomRestoredAndReported(t *testing.T) {
	g := newWeightedAdmission(6)
	g.resources.MemoryHeadroomBytes = 2 << 30
	d := &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 3 << 30, Pods: 2}
	r, err := g.adopt(model.Run{ID: "run", QueueSequence: 1}, pipeline.XL, argoworkflow.VMIDemand{}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer r.release()
	charged := r.declaredResources()
	if charged.MemoryBytes != (5<<30)+2*executorMemoryBytes || *r.pods != *d {
		t.Fatalf("restored reservation: %+v, sealed %+v", charged, r.pods)
	}
	observation := &model.AdmissionObservation{DeclaredResources: charged}
	for _, message := range []string{waitingAdmissionMessage(observation), (model.AdmissionBlocker{DeclaredResources: charged}).Reason()} {
		if !strings.Contains(message, "helper and memory headroom reserves") || !strings.Contains(message, "5905580032 memory bytes") {
			t.Fatalf("reservation message omits charged reserves: %s", message)
		}
	}
	g.resources.Disabled = true
	if r.declaredResources().MemoryBytes != charged.MemoryBytes {
		t.Fatal("kill switch discounted existing reservation")
	}
}
