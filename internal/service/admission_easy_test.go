package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/measured"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func easyRequest(id string, cpu int64, duration time.Duration) admissionRequest {
	return admissionRequest{RunID: id, Size: periapsis.M, Estimate: duration,
		Pods: &argoworkflow.PodDemand{CPUMilli: cpu - executorCPUMilli, MemoryBytes: 1 << 30, Pods: 1}}
}

func easyGate(t *testing.T) (*weightedAdmission, *fakeClock, *admissionReservation) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)}
	g := backfillGate(clock)
	g.resources.CPUMilli, g.resources.MemoryBytes = 11000, 48<<30
	running := g.reserve(1)
	acquireRequestWithinBudget(t, running, easyRequest("port-release", 6300, time.Hour))
	t.Cleanup(running.release)
	clock.now = clock.now.Add(30 * time.Minute)
	return g, clock, running
}

func TestEASYDrainingSnapshot(t *testing.T) {
	g, clock, _ := easyGate(t)
	head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
	t.Cleanup(head.cancel)
	g.mu.Lock()
	head.priority = model.PriorityHighest
	g.mu.Unlock()
	clock.now = clock.now.Add(21 * time.Minute)
	kick(g)
	for i, cpu := range []int64{1100, 2450} {
		r := pending(t, g, int64(3+i), easyRequest("short", cpu, 5*time.Minute))
		t.Cleanup(r.cancel)
		if !granted(g, r) {
			t.Fatalf("%dm short run held during drain; 9m remain until head can start", cpu)
		}
		view := (&Scheduler{admission: g}).AdmissionSnapshot(r.sequence)
		if view.BackfillMessage != "backfilled: ends before head can start (p95 5m < 9m)" {
			t.Fatalf("backfill explanation = %q", view.BackfillMessage)
		}
	}
	if !g.spareWeightForClaim(4) {
		t.Fatal("draining prevented claiming another measured candidate")
	}
}

func TestEASYRejectsUnknownAndDelayingRuns(t *testing.T) {
	for _, duration := range []time.Duration{0, 9 * time.Minute, time.Hour} {
		t.Run(duration.String(), func(t *testing.T) {
			g, clock, _ := easyGate(t)
			head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
			t.Cleanup(head.cancel)
			clock.now = clock.now.Add(21 * time.Minute)
			// 4 CPU fits now, but not beside the 8.2 CPU head.
			candidate := pending(t, g, 3, easyRequest("candidate", 4000, duration))
			t.Cleanup(candidate.cancel)
			if granted(g, candidate) {
				t.Fatal("unknown, equal-deadline, or delaying run was backfilled")
			}
			if view := (&Scheduler{admission: g}).AdmissionSnapshot(candidate.sequence); !strings.HasPrefix(view.BackfillMessage, "held: would delay head") {
				t.Fatalf("held explanation = %q", view.BackfillMessage)
			}
		})
	}
}

func TestEASYUnknownDurationNeverBackfillsBeforeDrainEither(t *testing.T) {
	g, _, _ := easyGate(t)
	head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
	t.Cleanup(head.cancel)
	r := pending(t, g, 3, easyRequest("no-samples", 1100, 0))
	t.Cleanup(r.cancel)
	if granted(g, r) {
		t.Fatal("unmeasured run backfilled before drain")
	}
}

func TestEASYForecastUsesEnoughCompletionsAndRunSlots(t *testing.T) {
	for _, slots := range []int{0, 2} {
		g, clock, _ := easyGate(t)
		g.maxRuns = slots
		other := pending(t, g, 2, easyRequest("other", 3000, 45*time.Minute))
		t.Cleanup(other.cancel)
		head := pending(t, g, 3, easyRequest("head", 8200, time.Hour))
		t.Cleanup(head.cancel)
		// Port ends in 30m but the surviving 3 CPU run still blocks head.
		candidate := pending(t, g, 4, easyRequest("candidate", 1100, 35*time.Minute))
		t.Cleanup(candidate.cancel)
		if slots == 0 && !granted(g, candidate) {
			t.Fatal("forecast did not wait for enough running CPU to finish (45m)")
		}
		if slots == 2 && granted(g, candidate) {
			t.Fatal("backfill bypassed current run ceiling")
		}
		clock.now = clock.now.Add(46 * time.Minute)
		if slots == 0 {
			// No real releases: expired estimates must not invent free capacity.
			late := pending(t, g, 5, easyRequest("late", 1100, time.Minute))
			t.Cleanup(late.cancel)
			if granted(g, late) {
				t.Fatal("overrun forecasts invented capacity")
			}
		}
	}
}

func TestEASYPressureHoldsSafeDrainingBackfill(t *testing.T) {
	g, clock, _ := easyGate(t)
	head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
	t.Cleanup(head.cancel)
	clock.now = clock.now.Add(21 * time.Minute)
	sample := model.PressureSample{}
	sample.IO.FullAvg60 = 80
	g.pressure = newPressureCheck(AdmissionPressureConfig{IOFullAvg60Max: 30, Hysteresis: 10, MaxDefer: 20 * time.Minute,
		Source: func() (model.PressureSample, error) { return sample, nil }})
	r := pending(t, g, 3, easyRequest("safe-but-pressure-held", 1100, 5*time.Minute))
	t.Cleanup(r.cancel)
	view := (&Scheduler{admission: g}).AdmissionSnapshot(r.sequence)
	if granted(g, r) || view.Pressure == nil || view.BackfillMessage != "" {
		t.Fatalf("safe candidate bypassed pressure or claimed a false grant: %+v", view)
	}
	sample.IO.FullAvg60 = 0
	clock.now = clock.now.Add(time.Second)
	kick(g)
	if !granted(g, r) {
		t.Fatal("safe candidate stayed held after pressure recovered")
	}
}

func TestEASYCoexistenceIncludesMemoryAndExecutorReserves(t *testing.T) {
	g, clock, _ := easyGate(t)
	h := easyRequest("head", 8200, time.Hour)
	h.Pods.MemoryBytes = 40 << 30
	head := pending(t, g, 2, h)
	t.Cleanup(head.cancel)
	clock.now = clock.now.Add(21 * time.Minute)
	c := easyRequest("cpu-fits-memory-does-not", 1100, time.Hour)
	c.Pods.MemoryBytes = 10 << 30
	r := pending(t, g, 3, c)
	t.Cleanup(r.cancel)
	if granted(g, r) {
		t.Fatal("coexistence checked CPU but ignored memory")
	}
}

func TestEASYCoexistencePreservesHeadVMCapacity(t *testing.T) {
	g, clock, _ := easyGate(t)
	vm := &KubeVirtResources{Demand: argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30},
		Limit: argoworkflow.VMIDemand{VMs: 1, Cores: 1, MemoryBytes: 1 << 30}}
	h := easyRequest("vm-head", 8200, time.Hour)
	h.RepoID, h.KubeVirt = 1, vm
	head := pending(t, g, 2, h)
	t.Cleanup(head.cancel)
	clock.now = clock.now.Add(21 * time.Minute)
	c := easyRequest("cpu-fits-vm-does-not", 1100, time.Hour)
	c.RepoID, c.KubeVirt = 1, vm
	r := pending(t, g, 3, c)
	t.Cleanup(r.cancel)
	if granted(g, r) {
		t.Fatal("long backfill consumes the only VM slot the head needs at its reserved start")
	}
}

func TestEASYHeadStartTimeInvariant(t *testing.T) {
	simulate := func(backfill bool) time.Time {
		g, clock, running := easyGate(t)
		head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
		t.Cleanup(head.cancel)
		clock.now = clock.now.Add(21 * time.Minute)
		var short, alongside *admissionReservation
		if backfill {
			short = pending(t, g, 3, easyRequest("short", 3500, 5*time.Minute))
			t.Cleanup(short.cancel)
			alongside = pending(t, g, 4, easyRequest("alongside", 1100, time.Hour))
			t.Cleanup(alongside.cancel)
			if !granted(g, short) || !granted(g, alongside) {
				t.Fatal("safe duration/coexistence backfill was not admitted")
			}
		}
		clock.now = clock.now.Add(5 * time.Minute)
		if short != nil {
			short.release()
		}
		clock.now = clock.now.Add(4 * time.Minute)
		running.release()
		if !granted(g, head) {
			t.Fatal("backfill delayed the head at the running release's predicted finish")
		}
		return clock.now
	}
	if with, without := simulate(true), simulate(false); !with.Equal(without) {
		t.Fatalf("head start with backfill %s, without %s", with, without)
	}
}

func TestEASYAggregateReservationsAndUnknownRunning(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		g, clock, running := easyGate(t)
		if unknown {
			g.mu.Lock()
			running.estimate = 0
			g.mu.Unlock()
		}
		head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
		t.Cleanup(head.cancel)
		clock.now = clock.now.Add(21 * time.Minute)
		first := pending(t, g, 3, easyRequest("first", 2000, time.Hour))
		t.Cleanup(first.cancel)
		second := pending(t, g, 4, easyRequest("second", 2000, time.Hour))
		t.Cleanup(second.cancel)
		if granted(g, first) == unknown || granted(g, second) {
			t.Fatalf("unknown=%v: first=%v second=%v", unknown, first.granted, second.granted)
		}
	}
}

type easyHistoryStore struct {
	SchedulerStore
	history store.RunResourceHistory
	err     error
}

func (s easyHistoryStore) RunResources(context.Context, int64, store.RunResourceFilter) (store.RunResourceHistory, error) {
	return s.history, s.err
}

func TestEASYHistoryUsesMeasuredWorkflowP95(t *testing.T) {
	at := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	history := store.RunResourceHistory{}
	for i := 1; i <= 20; i++ {
		id := fmt.Sprint(i)
		history.Runs = append(history.Runs, model.Run{ID: id, Status: model.RunPassed})
		history.Observations = append(history.Observations, measured.Observation{Usage: model.StepResourceUsage{
			RunID: id, Terminal: true, ContainerStartedAt: at, ContainerFinishedAt: at.Add(time.Duration(i) * time.Minute),
		}})
	}
	s := &Scheduler{store: easyHistoryStore{history: history}}
	_, got := s.admissionHistory(t.Context(), model.Run{RepoID: 1})
	if got != 19*time.Minute {
		t.Fatalf("measured workflow p95 = %s, want 19m", got)
	}
	history.Truncated = true
	s.store = easyHistoryStore{history: history}
	_, got = s.admissionHistory(t.Context(), model.Run{RepoID: 1})
	if got != 0 {
		t.Fatal("truncated telemetry supplied a duration")
	}
}

func TestEASYHistoryRejectsMissingIncompleteAndOtherWorkflowClasses(t *testing.T) {
	at := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	complete := measured.Observation{Usage: model.StepResourceUsage{RunID: "sample", Terminal: true,
		ContainerStartedAt: at, ContainerFinishedAt: at.Add(5 * time.Minute)}}
	for _, tc := range []struct {
		name         string
		run          model.Run
		observations []measured.Observation
		want         time.Duration
	}{
		{"missing", model.Run{ID: "sample", Status: model.RunPassed}, nil, 0},
		{"one sample", model.Run{ID: "sample", Status: model.RunPassed}, []measured.Observation{complete}, 5 * time.Minute},
		{"other workflow class", model.Run{ID: "sample", Status: model.RunPassed, Release: true}, []measured.Observation{complete}, 0},
		{"failed", model.Run{ID: "sample", Status: model.RunFailed}, []measured.Observation{complete}, 0},
		{"incomplete", model.Run{ID: "sample", Status: model.RunPassed}, []measured.Observation{complete, {Usage: model.StepResourceUsage{RunID: "sample"}}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{store: easyHistoryStore{history: store.RunResourceHistory{Runs: []model.Run{tc.run}, Observations: tc.observations}}}
			_, got := s.admissionHistory(t.Context(), model.Run{RepoID: 1})
			if got != tc.want {
				t.Fatalf("duration = %s, want %s", got, tc.want)
			}
		})
	}
}
