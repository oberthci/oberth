package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestStage2DeclaredBudgetNeverUsesSize(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		g := newWeightedAdmission(6)
		g.resources.Disabled = disabled
		r := g.reserve(1)
		release, err := r.acquireRequest(t.Context(), admissionRequest{Size: periapsis.S, Pods: &argoworkflow.PodDemand{CPUMilli: 8000, MemoryBytes: 1 << 30, Pods: 1}})
		if release != nil {
			release()
		}
		if err == nil || r.weight != 0 {
			t.Fatalf("declared demand used size fallback (disabled=%v): weight=%d err=%v", disabled, r.weight, err)
		}
	}
}

func TestStage2LegacyFallbackDeprecated(t *testing.T) {
	message := model.ResourceFallbackRequests.Message()
	if !strings.Contains(strings.ToLower(message), "deprecated") || !strings.Contains(message, "git") {
		t.Fatalf("missing deprecation guidance: %s", message)
	}
}

func TestStage2DeclaredIOPressureStillHolds(t *testing.T) {
	f := pressureTestGate()
	f.sample.IO.FullAvg60 = 40
	r := f.gate.reserve(1)
	f.gate.mu.Lock()
	r.prepared = true
	r.preparedAt = f.now
	r.size = periapsis.XL
	r.pods = &argoworkflow.PodDemand{CPUMilli: 100, MemoryBytes: 128 << 20, Pods: 1}
	f.gate.grantLocked()
	held := !r.granted && f.gate.pressureWaitLocked(r) != nil && r.weight == 0
	f.gate.mu.Unlock()
	if !held {
		t.Fatal("declared demand bypassed IO pressure")
	}
	f.sample.IO.FullAvg60 = 19
	f.advance(time.Second)
	if !r.granted {
		t.Fatal("pressure recovery did not admit measured work")
	}
	r.release()
}
