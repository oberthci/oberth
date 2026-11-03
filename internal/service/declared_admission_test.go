package service

import (
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestDeclaredAdmissionSharesBesideSizeXLAndKeepsIOCeiling(t *testing.T) {
	g := newWeightedAdmission(6)
	g.resources = DefaultDeclaredAdmissionConfig()
	g.resources.IORuns = 2
	big := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{RunID: "step-abc12345", Size: periapsis.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 6000, MemoryBytes: 8 << 30, Pods: 1}})
	defer big()
	small := acquireRequestWithinBudget(t, g.reserve(2), admissionRequest{RunID: "step-11111111", Size: periapsis.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1}})
	defer small()
	g.mu.Lock()
	third := &admissionReservation{size: periapsis.S, pods: &argoworkflow.PodDemand{CPUMilli: 1, MemoryBytes: 1, Pods: 1}}
	if g.canGrantLocked(third) {
		t.Error("IO ceiling bypassed by tiny CPU/memory declarations")
	}
	g.mu.Unlock()
}

func TestDeclaredAdmissionMixedLegacyAndKillSwitch(t *testing.T) {
	g := newWeightedAdmission(6)
	g.resources = DefaultDeclaredAdmissionConfig()
	legacy := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{RunID: "step-abc12345", Size: periapsis.M})
	defer legacy()
	g.mu.Lock()
	tooBig := &admissionReservation{size: periapsis.XL, pods: &argoworkflow.PodDemand{CPUMilli: 6000, MemoryBytes: 1 << 30, Pods: 1}}
	if g.canGrantLocked(tooBig) {
		t.Error("legacy weight did not reserve its share of numeric capacity")
	}
	g.resources.Disabled = true
	tooBig.pods = nil
	tooBig.weight = g.capacity
	if g.canGrantLocked(tooBig) {
		t.Error("kill switch did not restore legacy XL exclusion")
	}
	g.mu.Unlock()
}

func TestDeclaredAdmissionRejectsOversizeAndRetainsCopiedDemand(t *testing.T) {
	g := newWeightedAdmission(6)
	oversize := &argoworkflow.PodDemand{CPUMilli: 8000, MemoryBytes: 1 << 30, Pods: 1}
	legacy := g.reserve(1)
	releaseLegacy, err := legacy.acquireRequest(t.Context(), admissionRequest{Size: periapsis.S, Pods: oversize})
	if err == nil || releaseLegacy != nil || legacy.weight != 0 {
		t.Fatalf("oversize was not refused: %v", err)
	}

	d := &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1}
	r := g.reserve(2)
	release := acquireRequestWithinBudget(t, r, admissionRequest{Size: periapsis.XL, Pods: d})
	defer release()
	d.CPUMilli = 1
	if r.pods.CPUMilli != 1000 || r.declaredResources().CPUMilli != 1100 {
		t.Fatal("caller changed an active reservation")
	}
	g.mu.Lock()
	blocker := g.blockerLocked(r)
	g.mu.Unlock()
	if blocker.FullDrain || blocker.DeclaredResources == nil {
		t.Fatal("declared XL advertised a full drain")
	}
}
