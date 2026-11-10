package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestAdminPriorityLoweringToolAndReservation(t *testing.T) {
	f := newControlFixture(t)
	control, err := NewAPI(APIConfig{Runs: f.store, History: f.store, QueuePriorities: f.store, Enqueues: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Shutdown(t.Context())
	run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/admin", SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	for _, priority := range []string{"normal", "low"} {
		raw := json.RawMessage(`{"run_id":"` + run.ID[:8] + `","priority":"` + priority + `","reason":"operator queue correction"}`)
		if _, err = control.CallTool(t.Context(), api.Actor{Identity: "dev"}, "run_priority", raw); !errors.Is(err, ErrForbidden) {
			t.Fatalf("non-admin lower=%v", err)
		}
		value, callErr := control.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "run_priority", raw)
		if callErr != nil {
			t.Fatal(callErr)
		}
		shown := value.(model.Run)
		if shown.Priority != model.RunPriority(priority).Canonical() || !strings.Contains(shown.PriorityReason, "operator queue correction") {
			t.Fatalf("reason line=%+v", shown)
		}
		reservation := &admissionReservation{priority: shown.Priority, priorityReason: shown.PriorityReason, priorityOverride: shown.PriorityOverride, priorityOverrideReason: shown.PriorityOverrideReason, queuedAt: time.Now().Add(-2 * time.Hour)}
		if got := reservation.effectivePriority(time.Now()); got != model.RunPriority(priority).Canonical() {
			t.Fatalf("reservation aged to %s", got)
		}
	}
	for _, reason := range []string{"", "  "} {
		raw, _ := json.Marshal(map[string]any{"run_id": run.ID, "priority": "low", "reason": reason})
		if _, err = control.CallTool(t.Context(), api.Actor{Identity: "admin", Admin: true}, "run_priority", raw); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("missing reason accepted: %v", err)
		}
	}
}

func TestAdminPriorityLoweringReordersBlockedHead(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityNormal, model.PriorityLow} {
		t.Run(string(priority), func(t *testing.T) {
			f := newControlFixture(t)
			gate := newAdmissionGate()
			future := time.Now().Add(2 * time.Hour)
			gate.now = func() time.Time { return future }
			f.scheduler.admission = gate
			holder := gate.reserve(999999)
			release, err := holder.acquire(t.Context(), pipeline.XL)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			blockOnDiskFloor(gate)
			var runs []model.Run
			for _, ref := range []string{"head", "next"} {
				queued, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
				if err != nil {
					t.Fatal(err)
				}
				runs = append(runs, queued.Run)
			}
			if _, err = f.store.BumpRunPriority(t.Context(), "admin", runs[0].ID, model.PriorityHighest, "old urgency"); err != nil {
				t.Fatal(err)
			}
			var waiting []*admissionReservation
			for range runs {
				run, err := f.store.ClaimNextRun(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				r := gate.reserveRun(run)
				gate.mu.Lock()
				r.prepared = true
				gate.mu.Unlock()
				waiting = append(waiting, r)
			}
			lowered, err := f.scheduler.BumpRunPriority(t.Context(), "admin", runs[0].ID, priority, "unblock the queue")
			if err != nil {
				t.Fatal(err)
			}
			if lowered.Priority != priority {
				t.Fatal(lowered)
			}
			release()
			gate.mu.Lock()
			gate.sortWaitersLocked()
			first := gate.waiters[0]
			gate.mu.Unlock()
			if first != waiting[1] {
				t.Fatal("lowered head still leads ordinary work")
			}
			clearDiskFloor(gate)
			if !granted(gate, waiting[1]) || !granted(gate, waiting[0]) {
				t.Fatal("lowered run lost its reservation")
			}
			waiting[1].release()
			if _, err = f.scheduler.BumpRunPriority(t.Context(), "admin", runs[0].ID, model.PriorityLow, "already admitted"); !errors.Is(err, store.ErrInvalidState) {
				t.Fatalf("admitted work was preempted: %v", err)
			}
			waiting[0].release()
		})
	}
}
