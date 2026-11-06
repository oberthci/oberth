package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestLegacyFixedChargeAdmitsLiveXLHead(t *testing.T) {
	g := newWeightedAdmission(8)
	g.weights.XL = 4
	g.maxRuns = 4
	g.resources.CPUMilli, g.resources.MemoryBytes = 24000, 96<<30
	legacy := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{RunID: "legacy-L", Size: pipeline.L})
	defer legacy()
	declared := acquireRequestWithinBudget(t, g.reserve(2), admissionRequest{RunID: "declared", Size: pipeline.XL, Pods: &argoworkflow.PodDemand{CPUMilli: 9450, MemoryBytes: 16 << 30, Pods: 1}})
	defer declared()
	head := priorityWaiter(g, 3, model.PriorityNormal, time.Now(), pipeline.XL)
	defer head.cancel()
	if !granted(g, head) {
		t.Fatal("XL head blocked beside one L legacy and one 9.45 CPU declared run under 24 CPU/96Gi")
	}
	s := &Scheduler{admission: g}
	state := s.AdmissionState()
	if state.NumericBudget.CPUUsedMilli != 18550 {
		t.Fatalf("CPU charge = %d, want 3000 + 9550 + 6000", state.NumericBudget.CPUUsedMilli)
	}
	if state.Reservations[1].LegacyResources != nil || state.Reservations[1].DeclaredResources == nil {
		t.Fatal("declared reservation marked legacy")
	}
	body, err := json.Marshal(state)
	if err != nil || strings.Count(string(body), `"legacy_resources"`) != 2 {
		t.Fatalf("system status must identify both legacy reservations: %s (%v)", body, err)
	}
}

func TestLegacyFixedChargeRestoredAndRetained(t *testing.T) {
	g := newWeightedAdmission(8)
	g.resources.LegacyResources = map[pipeline.Size]model.LegacyResourceCharge{pipeline.L: {CPUMilli: 4500, MemoryBytes: 10 << 30}}
	r, err := g.adopt(model.Run{ID: "restored", QueueSequence: 1}, pipeline.L)
	if err != nil {
		t.Fatal(err)
	}
	r.retain()
	r.release() // Uncertain ownership keeps the numeric charge as well as weight.
	state := (&Scheduler{admission: g}).AdmissionState()
	if state.NumericBudget.CPUUsedMilli != 4500 || state.NumericBudget.MemoryUsedBytes != 10<<30 || state.Reservations[0].State != "retained" || state.Reservations[0].LegacyResources.CPUMilli != 4500 {
		t.Fatalf("restored charge not retained: %+v", state)
	}
	r.releaseAfterCleanup()
	state = (&Scheduler{admission: g}).AdmissionState()
	if state.NumericBudget.CPUUsedMilli != 0 || state.NumericBudget.MemoryUsedBytes != 0 {
		t.Fatalf("cleaned up charge still held: %+v", state.NumericBudget)
	}
}

func TestLegacyFixedChargeDoesNotGrowWithBudgetOrWeight(t *testing.T) {
	for _, budget := range []int64{24000, 48000} {
		g := newWeightedAdmission(8)
		g.resources.CPUMilli, g.resources.MemoryBytes = budget, 96<<30
		g.weights.L = 4
		release := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{Size: pipeline.L})
		defer release()
		state := (&Scheduler{admission: g}).AdmissionState()
		if state.NumericBudget.CPUUsedMilli != 3000 || state.NumericBudget.MemoryUsedBytes != 8<<30 {
			t.Fatalf("L charge changes with budget or weight: %+v", state.NumericBudget)
		}
	}
}

func TestLegacyExclusiveXLOnSmallBudgets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cpu, memory int64
		configured  bool
	}{
		{"8Gi", 8000, 8 << 30, false},
		{"12Gi", 8000, 12 << 30, false},
		{"small CPU", 2500, 32 << 30, false},
		{"both small", 2500, 8 << 30, false},
		{"configured XL", 2500, 8 << 30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newWeightedAdmission(8)
			g.resources.CPUMilli, g.resources.MemoryBytes = tc.cpu, tc.memory
			if tc.configured {
				g.resources.LegacyResources = map[pipeline.Size]model.LegacyResourceCharge{pipeline.XL: {CPUMilli: 6000, MemoryBytes: 16 << 30}}
			}
			if err := g.resources.Validate(); err != nil {
				t.Fatal(err)
			}
			release := acquireRequestWithinBudget(t, g.reserve(1), admissionRequest{Size: pipeline.XL})
			defer release()
			state := (&Scheduler{admission: g}).AdmissionState()
			cpu, memory := min(tc.cpu, 6000), min(tc.memory, 16<<30)
			charge := state.Reservations[0].LegacyResources
			if state.NumericBudget.CPUUsedMilli != cpu || state.NumericBudget.MemoryUsedBytes != memory || charge == nil || charge.CPUMilli != cpu || charge.MemoryBytes != memory {
				t.Fatalf("exclusive charge must be bounded and reported consistently: budget=%+v charge=%+v", state.NumericBudget, charge)
			}
			beside := priorityWaiter(g, 2, model.PriorityNormal, time.Now(), pipeline.S)
			defer beside.cancel()
			if granted(g, beside) {
				t.Fatal("S admitted beside exclusive XL")
			}
			release()
			if !granted(g, beside) {
				t.Fatal("S still blocked after XL releases")
			}
			beside.release()

			// A positive XL weight does not earn the exclusive clamp.
			g.weights.XL = 4
			if release, err := g.reserve(3).acquireRequest(t.Context(), admissionRequest{Size: pipeline.XL}); err == nil || release != nil {
				t.Fatal("oversized shared XL accepted")
			}
		})
	}
}

func TestLegacyExclusiveXLRestoredOnSmallBudget(t *testing.T) {
	g := newWeightedAdmission(8)
	g.resources.CPUMilli, g.resources.MemoryBytes = 4000, 8<<30
	r, err := g.adopt(model.Run{ID: "restored-xl", QueueSequence: 1}, pipeline.XL)
	if err != nil {
		t.Fatal(err)
	}
	r.retain()
	r.release()
	state := (&Scheduler{admission: g}).AdmissionState()
	charge := state.Reservations[0].LegacyResources
	if charge == nil || charge.CPUMilli != 4000 || charge.MemoryBytes != 8<<30 || state.NumericBudget.CPUUsedMilli != 4000 || state.NumericBudget.MemoryUsedBytes != 8<<30 {
		t.Fatalf("restored XL charge exceeds the small budget: %+v, %+v", charge, state.NumericBudget)
	}
	beside := priorityWaiter(g, 2, model.PriorityNormal, time.Now(), pipeline.S)
	defer beside.cancel()
	if granted(g, beside) {
		t.Fatal("retained XL lost exclusivity")
	}
	r.releaseAfterCleanup()
	if !granted(g, beside) {
		t.Fatal("S still blocked after XL cleanup")
	}
	beside.release()
}

func TestLegacyConfiguredChargeBudgetValidation(t *testing.T) {
	for _, size := range []pipeline.Size{pipeline.S, pipeline.M, pipeline.L, pipeline.XL} {
		for _, dimension := range []string{"CPU", "memory"} {
			t.Run(string(size)+"/"+dimension, func(t *testing.T) {
				c := DefaultDeclaredAdmissionConfig()
				weights := DefaultSizeWeights
				weights.XL = 4
				c.CPUMilli, c.MemoryBytes = 2500, 8<<30
				charge := model.LegacyResourceCharge{CPUMilli: c.CPUMilli, MemoryBytes: c.MemoryBytes}
				c.LegacyResources = map[pipeline.Size]model.LegacyResourceCharge{size: charge}
				if err := c.ValidateForSizeWeights(weights); err != nil {
					t.Fatalf("exact budget rejected: %v", err)
				}
				if dimension == "CPU" {
					charge.CPUMilli++
				} else {
					charge.MemoryBytes++
				}
				c.LegacyResources[size] = charge
				err := c.ValidateForSizeWeights(weights)
				if err == nil || !strings.Contains(err.Error(), string(size)) || !strings.Contains(err.Error(), dimension) || !strings.Contains(err.Error(), "budget") {
					t.Fatalf("want size and dimension in budget error, got %v", err)
				}
			})
		}
	}
}

func TestLegacyFixedChargeDefaultsConfigurationAndLimits(t *testing.T) {
	g := newWeightedAdmission(8)
	g.weights.XL = 4
	g.resources.CPUMilli, g.resources.MemoryBytes = 24000, 96<<30
	for _, tc := range []struct {
		size        pipeline.Size
		cpu, memory int64
	}{{pipeline.S, 1000, 2 << 30}, {pipeline.M, 2000, 4 << 30}, {pipeline.L, 3000, 8 << 30}, {pipeline.XL, 6000, 16 << 30}} {
		t.Run(string(tc.size), func(t *testing.T) {
			r := g.reserve(1)
			if view := g.reservationViewLocked(r); view.LegacyResources != nil {
				t.Fatal("unsized claim marked legacy")
			}
			release := acquireRequestWithinBudget(t, r, admissionRequest{Size: tc.size})
			defer release()
			view := (&Scheduler{admission: g}).AdmissionSnapshot(1)
			if view.LegacyResources == nil || view.LegacyResources.CPUMilli != tc.cpu || view.LegacyResources.MemoryBytes != tc.memory || view.DeclaredResources != nil {
				t.Fatalf("legacy view = %+v", view)
			}
			g.mu.Lock()
			candidate := &admissionReservation{size: tc.size, weight: r.weight}
			g.resources.CPUMilli, g.resources.MemoryBytes = 2*tc.cpu, 2*tc.memory
			exact := g.canGrantLocked(candidate)
			g.resources.CPUMilli--
			cpuOver := g.canGrantLocked(candidate)
			g.resources.CPUMilli++
			g.resources.MemoryBytes--
			memoryOver := g.canGrantLocked(candidate)
			g.resources.CPUMilli, g.resources.MemoryBytes = 24000, 96<<30
			g.maxRuns = 1
			runCap := g.canGrantLocked(candidate)
			g.maxRuns = 0
			g.resources.IORuns = 1
			ioCap := g.canGrantLocked(candidate)
			g.resources.IORuns = 0
			g.mu.Unlock()
			if !exact || cpuOver || memoryOver || runCap || ioCap {
				t.Fatalf("exact=%t cpuOver=%t memoryOver=%t runCap=%t ioCap=%t", exact, cpuOver, memoryOver, runCap, ioCap)
			}
		})
	}
	g.resources.LegacyResources = map[pipeline.Size]model.LegacyResourceCharge{pipeline.L: {CPUMilli: 4500, MemoryBytes: 10 << 30}}
	if err := g.resources.Validate(); err != nil {
		t.Fatal(err)
	}
	if g.resources.legacyCharge(pipeline.L).CPUMilli != 4500 || g.resources.legacyCharge(pipeline.S).CPUMilli != 1000 {
		t.Fatal("custom charge or fallback lost")
	}
	r := g.reserve(2)
	release := acquireRequestWithinBudget(t, r, admissionRequest{Size: pipeline.L})
	state := (&Scheduler{admission: g}).AdmissionState()
	if state.NumericBudget.CPUUsedMilli != 4500 || state.NumericBudget.MemoryUsedBytes != 10<<30 {
		t.Fatalf("override not charged: %+v", state.NumericBudget)
	}
	release()
	g.resources.CPUMilli = 4499
	if release, err := g.reserve(3).acquireRequest(t.Context(), admissionRequest{Size: pipeline.L}); err == nil || release != nil {
		t.Fatal("impossible legacy run queued")
	}
	for _, charge := range []model.LegacyResourceCharge{{CPUMilli: 0, MemoryBytes: 1}, {CPUMilli: 1, MemoryBytes: 0}, {CPUMilli: 1000001, MemoryBytes: 1}, {CPUMilli: 1, MemoryBytes: 1<<50 + 1}} {
		c := DefaultDeclaredAdmissionConfig()
		c.LegacyResources = map[pipeline.Size]model.LegacyResourceCharge{pipeline.S: charge}
		if c.Validate() == nil {
			t.Fatalf("invalid charge accepted: %+v", charge)
		}
	}
	for _, size := range []pipeline.Size{"", "XXL"} {
		c := DefaultDeclaredAdmissionConfig()
		c.LegacyResources = map[pipeline.Size]model.LegacyResourceCharge{size: {CPUMilli: 1, MemoryBytes: 1}}
		if c.Validate() == nil {
			t.Fatalf("invalid size accepted: %q", size)
		}
	}
}
