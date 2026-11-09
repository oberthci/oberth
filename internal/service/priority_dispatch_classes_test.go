package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

// Exercise the production workflow-count gate and real durable queue, with
// deterministic checkout completion instead of background worker timing.
func TestPriorityDispatchReachesHighPastParkedLowerClasses(t *testing.T) {
	for _, initial := range []model.RunPriority{model.PriorityStandard, model.PriorityLow} {
		t.Run(string(initial), func(t *testing.T) {
			f := newControlFixture(t)
			g := newAdmissionGate(2)
			s := &Scheduler{store: f.store, admission: g, maxConcurrent: 2}
			active := 0
			enqueue := func(ref string, priority model.RunPriority) model.Run {
				t.Helper()
				admin := priority == model.PriorityHigh
				trigger := "push"
				if priority == model.PriorityPromotion {
					trigger, priority = "promotion", ""
				}
				r, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{
					RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: ref,
					SHA: strings.Repeat("a", 40), Actor: "submitter", Trigger: trigger,
					PriorityRequest: model.PriorityRequest{Priority: priority, UnscopedAdmin: &admin},
				})
				if err != nil {
					t.Fatal(err)
				}
				return r.Run
			}
			claim := func(expected model.Run) *admissionReservation {
				t.Helper()
				allowed := s.dispatchSlotAvailable(active)
				if !allowed {
					var err error
					allowed, err = s.priorityDispatchAvailable(t.Context(), active)
					if err != nil {
						t.Fatal(err)
					}
				}
				if !allowed {
					t.Fatalf("fixture cannot dispatch %s at active=%d", expected.Ref, active)
				}
				r, err := f.store.ClaimNextRun(t.Context())
				if err != nil || r.ID != expected.ID {
					t.Fatalf("claim=%s want=%s: %v", r.ID, expected.ID, err)
				}
				active++
				reservation := g.reserveRun(r)
				t.Cleanup(reservation.cancel)
				return reservation
			}
			holder := claim(enqueue("holder", model.PriorityStandard))
			release, err := holder.acquireRequest(t.Context(), admissionRequest{Group: "serial", GroupLimit: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			park := func(ref string, priority model.RunPriority) {
				t.Helper()
				r := claim(enqueue(ref, priority))
				g.mu.Lock()
				r.prepared, r.group, r.groupName, r.groupLimit = true, "serial", "serial", 1
				g.grantLocked()
				g.mu.Unlock()
				if granted(g, r) {
					t.Fatal("group waiter bypassed holder")
				}
			}
			for i := 0; i < 3; i++ {
				park(fmt.Sprintf("initial-%d", i), initial)
			}
			if initial == model.PriorityLow {
				park("standard", model.PriorityStandard)
			}
			park("delivery", model.PriorityPromotion)
			urgent := enqueue("urgent-independent-group", model.PriorityHigh)
			next, err := f.store.NextQueuedRun(t.Context())
			if err != nil || next.ID != urgent.ID {
				t.Fatalf("SQL did not select high: %+v %v", next, err)
			}
			allowed := s.dispatchSlotAvailable(active)
			if !allowed {
				allowed, err = s.priorityDispatchAvailable(t.Context(), active)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !allowed {
				t.Fatalf("authorized high stranded behind parked lower classes: active=%d, granted=%d, maxRuns=%d, waiters=%d; independent workflow slot is free", active, g.runs, g.maxRuns, len(g.waiters))
			}
			// Cross the actual dispatch boundary, then use the otherwise free
			// workflow slot. The existing holder and serial group stay intact.
			urgentReservation := claim(urgent)
			releaseUrgent, err := urgentReservation.acquireRequest(t.Context(), admissionRequest{Group: "independent", GroupLimit: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer releaseUrgent()
			assertLimits := func() {
				t.Helper()
				g.mu.Lock()
				defer g.mu.Unlock()
				if g.runs != 2 || g.maxRuns != 2 || !holder.granted || holder.released || !urgentReservation.granted || urgentReservation.released || len(g.groups["serial"]) != 1 || len(g.groups["independent"]) != 1 {
					t.Fatalf("execution/group bound or running holder changed: runs=%d max=%d serial=%d independent=%d", g.runs, g.maxRuns, len(g.groups["serial"]), len(g.groups["independent"]))
				}
			}
			assertLimits()
			// Even more authorized high work must reach a finite claim bound.
			// In this two-slot workflow fixture, ordinary/group allowance is
			// four claims and there are three overtaking classes above low.
			const maxClaims = 7
			bounded := false
			for i := 0; i < maxClaims; i++ {
				candidate := enqueue(fmt.Sprintf("urgent-following-%d", i), model.PriorityHigh)
				available := s.dispatchSlotAvailable(active)
				if !available {
					available, err = s.priorityDispatchAvailable(t.Context(), active)
				}
				if err != nil {
					t.Fatal(err)
				}
				if !available {
					bounded = true
					break
				}
				r := claim(candidate)
				g.mu.Lock()
				r.prepared = true
				g.grantLocked()
				g.mu.Unlock()
				if active > maxClaims || granted(g, r) {
					t.Fatalf("claim/execution bound escaped: active=%d", active)
				}
				assertLimits()
			}
			if !bounded {
				t.Fatal("priority lookahead never reached a finite bound")
			}
		})
	}
}
