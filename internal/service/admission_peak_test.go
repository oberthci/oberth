package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestBoundedBackfillStopsAtHeadWaitLimit(t *testing.T) {
	g, clock, _ := easyGate(t)
	head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
	t.Cleanup(head.cancel)
	clock.now = clock.now.Add(g.maxHeadWait)
	candidate := pending(t, g, 3, easyRequest("short", 1100, time.Minute))
	t.Cleanup(candidate.cancel)
	if granted(g, candidate) {
		t.Fatal("backfilled at the head wait limit")
	}
	if g.spareWeightForClaim(3) {
		t.Fatal("claimed backfill after the head wait limit")
	}
}

func TestBoundedBackfillUnknownRunningDuration(t *testing.T) {
	for _, duration := range []time.Duration{time.Minute, 11 * time.Minute, 0} {
		t.Run(duration.String(), func(t *testing.T) {
			g, clock, running := easyGate(t)
			g.mu.Lock()
			running.estimate = 0
			g.mu.Unlock()
			head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
			t.Cleanup(head.cancel)
			clock.now = clock.now.Add(10 * time.Minute)
			candidate := pending(t, g, 3, easyRequest("candidate", 4000, duration))
			t.Cleanup(candidate.cancel)
			if got, want := granted(g, candidate), duration == time.Minute; got != want {
				t.Fatalf("backfill=%v, want %v", got, want)
			}
			if granted(g, candidate) {
				candidate.release()
			}
			running.release()
			if !granted(g, head) {
				t.Fatal("head failed to start when capacity released")
			}
		})
	}
}

func TestBoundedBackfillFinishesWithinHeadWaitBudget(t *testing.T) {
	g, clock, _ := easyGate(t)
	head := pending(t, g, 2, easyRequest("head", 8200, time.Hour))
	t.Cleanup(head.cancel)
	clock.now = clock.now.Add(19 * time.Minute)
	candidate := pending(t, g, 3, easyRequest("too-long", 4000, 5*time.Minute))
	t.Cleanup(candidate.cancel)
	if granted(g, candidate) {
		t.Fatal("candidate extends beyond the head wait budget")
	}
}

func TestAdmissionPeakViewsIncludeRawPeak(t *testing.T) {
	g, _, _ := easyGate(t)
	s := &Scheduler{admission: g}
	r := pending(t, g, 2, easyRequest("visible", 1100, time.Minute))
	t.Cleanup(r.cancel)
	for name, view := range map[string]any{"run_get": s.AdmissionSnapshot(r.sequence), "system_status": s.AdmissionState().Reservations[1]} {
		raw, _ := json.Marshal(view)
		var decoded struct {
			DeclaredResources struct {
				Peak *struct {
					CPUMilli int64 `json:"cpu_milli"`
				} `json:"peak"`
			} `json:"declared_resources"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.DeclaredResources.Peak == nil || decoded.DeclaredResources.Peak.CPUMilli != 1000 {
			t.Errorf("%s lacks raw peak beside charged resources: %s", name, raw)
		}
	}
	// Whole-budget legacy work remains exclusive and has no numeric peak.
	if g.weights.of(pipeline.XL, g.capacity) != g.capacity {
		t.Fatal("legacy exclusivity changed")
	}
}

func TestAdmissionPeakTotalsSurviveRecovery(t *testing.T) {
	f := newConcurrencyFixture(t, pipeline.M, 3)
	run := model.Run{ID: strings.Repeat("a", 32), WorkflowName: "step-abc12345", SHA: strings.Repeat("b", 40), RepoID: f.repo.ID, QueueSequence: 1}
	d := &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1, DeclaredTotal: &argoworkflow.PodRequests{CPUMilli: 3000, MemoryBytes: 3 << 30, Pods: 3}}
	if err := f.scheduler.recordCreateAdmission(run, run.WorkflowName, pipeline.M, argoworkflow.VMIDemand{}, d); err != nil {
		t.Fatal(err)
	}
	r, err := f.scheduler.restoreCreateGrant(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := r.declaredResources()
	if v.Peak.CPUMilli != 1000 || v.DeclaredTotal == nil || v.DeclaredTotal.CPUMilli != 3000 {
		t.Fatalf("recovered resources: %+v", v)
	}
	v.DeclaredTotal.CPUMilli = 99
	if r.pods.DeclaredTotal.CPUMilli != 3000 {
		t.Fatal("operator view mutated reservation")
	}
}

func TestAdmissionPeakQueueExamples(t *testing.T) {
	for _, sum := range []int64{9150, 10950} {
		t.Run(fmt.Sprint(sum), func(t *testing.T) {
			otherCPU := (sum - 4500) / 2
			otherMemory := int64(2)
			if sum == 10950 {
				otherMemory = 5
			}
			source := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: root
  templates:
  - name: root
    dag:
      tasks:
      - {name: first, template: big}
      - {name: second, template: small, depends: first}
      - {name: third, template: small, depends: second}
  - name: big
    container:
      resources:
        requests: {cpu: 4500m, memory: 11Gi}
        limits: {memory: 11Gi}
  - name: small
    container:
      resources:
        requests: {cpu: %dm, memory: %dGi}
        limits: {memory: %dGi}
`, otherCPU, otherMemory, otherMemory)
			wf, err := argoworkflow.Decode([]byte(source))
			if err != nil {
				t.Fatal(err)
			}
			d, err := argoworkflow.DeclaredPodPeak(wf)
			if err != nil || d == nil {
				t.Fatalf("%+v %v", d, err)
			}
			if d.CPUMilli != 4500 || d.MemoryBytes != 11<<30 || d.DeclaredTotal == nil || d.DeclaredTotal.CPUMilli != sum || d.DeclaredTotal.MemoryBytes != (11+2*otherMemory)<<30 {
				t.Fatalf("peak versus sum: %+v", d)
			}
			g := newWeightedAdmission(8)
			first := g.reserve(1)
			release := acquireRequestWithinBudget(t, first, admissionRequest{Size: pipeline.XL, Pods: d})
			defer release()
			if g.canGrantLocked(&admissionReservation{size: pipeline.XL, pods: d}) {
				t.Fatal("two large peaks exceed the default budget")
			}
			small := &admissionReservation{size: pipeline.XL, pods: &argoworkflow.PodDemand{CPUMilli: 2000, MemoryBytes: 1 << 30, Pods: 1}}
			if !g.canGrantLocked(small) {
				t.Fatal("default budget should fit a small run beside one peak")
			}
			g.resources.CPUMilli, g.resources.MemoryBytes = 11000, 48<<30
			if !g.canGrantLocked(&admissionReservation{size: pipeline.XL, pods: d}) {
				t.Fatal("11 CPU / 48Gi budget should fit two example peaks")
			}
			t.Logf("sum %dm / %dGi -> peak %dm / 11Gi, charged %dm / %.2fGi; default budget: large + small; 11 CPU/48Gi: two large", sum, 11+2*otherMemory, d.CPUMilli, first.declaredResources().CPUMilli, float64(first.declaredResources().MemoryBytes)/(1<<30))
		})
	}
}

func TestAdmissionPeakRunGetReportsDeclaredAndPeak(t *testing.T) {
	f := newControlFixture(t)
	enqueued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "peak-view", Repository: f.repo, Branch: "feature/peak", SHA: strings.Repeat("a", 40), Actor: "reader"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != enqueued.ID {
		t.Fatal("wrong run claimed")
	}
	r := f.scheduler.admission.reserveRun(claimed)
	d := &argoworkflow.PodDemand{CPUMilli: 1000, MemoryBytes: 1 << 30, Pods: 1, DeclaredTotal: &argoworkflow.PodRequests{CPUMilli: 3000, MemoryBytes: 3 << 30, Pods: 3}}
	release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: claimed.ID, RepoID: f.repo.ID, Size: pipeline.XL, Pods: d})
	defer release()
	control := f.api(t)
	control.admission = f.scheduler
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "reader"}, "run_get", json.RawMessage(`{"id":"`+claimed.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	detail := value.(RunDetailResponse)
	if detail.Scheduling == nil || detail.Scheduling.Admission == nil || detail.Scheduling.Admission.DeclaredResources == nil {
		t.Fatalf("missing run admission: %+v", detail.Scheduling)
	}
	resources := detail.Scheduling.Admission.DeclaredResources
	if resources.DeclaredTotal == nil || resources.DeclaredTotal.CPUMilli != 3000 || resources.Peak == nil || resources.Peak.CPUMilli != 1000 || resources.CPUMilli != 1100 {
		t.Fatalf("raw total/peak/charged resources: %+v", resources)
	}
}

func TestBoundedBackfillStreamDrainsForHead(t *testing.T) {
	g, clock, running := easyGate(t)
	g.mu.Lock()
	running.estimate = 0
	g.mu.Unlock()
	head := pending(t, g, 2, easyRequest("head", 11000, time.Hour))
	t.Cleanup(head.cancel)
	start := clock.now
	grants := 0
	for minute := 0; minute < 30; minute++ {
		clock.now = start.Add(time.Duration(minute) * time.Minute)
		r := pending(t, g, int64(minute+3), easyRequest("stream", 1100, time.Minute))
		if granted(g, r) {
			grants++
			if minute >= 19 {
				t.Fatalf("backfill at %dm cannot finish strictly within 20m bound", minute)
			}
			clock.now = clock.now.Add(time.Minute)
			r.release()
		} else {
			r.cancel()
		}
	}
	if grants == 0 {
		t.Fatal("nothing used the initial backfill window")
	}
	clock.now = start.Add(30 * time.Minute)
	running.release()
	if !granted(g, head) {
		t.Fatal("stream starved head after original holder released")
	}
}
