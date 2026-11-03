package service

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestExtendedAdmissionReservesReleasesAndCopiesDemand(t *testing.T) {
	gate := newWeightedAdmission(6)
	gate.resources = DefaultDeclaredAdmissionConfig()
	if err := json.Unmarshal([]byte(`{"ExtendedResources":{"devices.example.test/worker":2}}`), &gate.resources); err != nil {
		t.Fatal(err)
	}
	demand := &argoworkflow.PodDemand{CPUMilli: 100, MemoryBytes: 128 << 20, Pods: 1, ExtendedResources: map[string]int64{"devices.example.test/worker": 2}}
	holder := gate.reserve(1)
	release := acquireRequestWithinBudget(t, holder, admissionRequest{RunID: "holder", Size: periapsis.S, Pods: demand})
	defer release()
	demand.ExtendedResources["devices.example.test/worker"] = 0
	candidate := &admissionReservation{size: periapsis.S, pods: &argoworkflow.PodDemand{CPUMilli: 100, MemoryBytes: 128 << 20, Pods: 1, ExtendedResources: map[string]int64{"devices.example.test/worker": 1}}}
	gate.mu.Lock()
	fits := gate.canGrantLocked(candidate)
	gate.mu.Unlock()
	if fits {
		t.Fatal("active extended resource reservation was ignored or mutated through caller data")
	}
	release()
	gate.mu.Lock()
	fits = gate.canGrantLocked(candidate)
	gate.mu.Unlock()
	if !fits {
		t.Fatal("released extended resource capacity remained held")
	}
}

func TestExtendedAdmissionNeverFallsBackToUnpricedWeight(t *testing.T) {
	for _, scenario := range []string{"missing budget", "too large", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			gate := newWeightedAdmission(6)
			gate.resources = DefaultDeclaredAdmissionConfig()
			if scenario != "missing budget" {
				if err := json.Unmarshal([]byte(`{"ExtendedResources":{"devices.example.test/worker":1}}`), &gate.resources); err != nil {
					t.Fatal(err)
				}
			}
			gate.resources.Disabled = scenario == "disabled"
			reservation := gate.reserve(1)
			release, err := reservation.acquireRequest(t.Context(), admissionRequest{RunID: "unpriced", Size: periapsis.S, Pods: &argoworkflow.PodDemand{CPUMilli: 100, MemoryBytes: 128 << 20, Pods: 1, ExtendedResources: map[string]int64{"devices.example.test/worker": 2}}})
			if release != nil {
				release()
			}
			if err == nil {
				t.Fatal("extended resources escaped numeric admission through fallback")
			}
		})
	}
}
