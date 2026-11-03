package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPriorityReleaseHeadMessageMatchesBackfillPolicy(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityNormal, model.PriorityHigh, model.PriorityHighest} {
		for _, elapsed := range []time.Duration{0, 21 * time.Minute} {
			t.Run(string(priority)+"/"+elapsed.String(), func(t *testing.T) {
				g, clock, _ := easyGate(t)
				head := pendingPriority(t, g, 2, priority, easyRequest("head", 8200, time.Hour))
				t.Cleanup(head.cancel)
				clock.now = clock.now.Add(elapsed)
				kick(g)

				view := (&Scheduler{admission: g}).AdmissionSnapshot(head.sequence)
				message := waitingAdmissionMessage(view)
				want := "backfilling"
				if elapsed >= g.maxHeadWait {
					want = "draining"
				}
				if !strings.Contains(message, want) || strings.Contains(message, "every later run waits behind it") {
					t.Errorf("head message=%q, want %q", message, want)
				}

				remaining := 30*time.Minute - elapsed
				cpu := int64(1100) // Fits beside the future head, but a lower class must finish first.
				if priority == model.PriorityNormal {
					cpu = 4000 // Same-class work must finish first when coexistence does not fit.
				}
				for i, duration := range []time.Duration{0, remaining, remaining + time.Minute} {
					candidate := pending(t, g, int64(3+i), easyRequest("held", cpu, duration))
					t.Cleanup(candidate.cancel)
					if granted(g, candidate) {
						t.Fatalf("candidate with p95 %s backfilled with %s until head start", duration, remaining)
					}
				}
				safe := pending(t, g, 6, easyRequest("short", cpu, time.Minute))
				t.Cleanup(safe.cancel)
				if got, want := granted(g, safe), elapsed < g.maxHeadWait; got != want || granted(g, head) {
					t.Fatalf("short backfill=%v, want %v at head wait %s", got, want, elapsed)
				}
				if elapsed >= g.maxHeadWait {
					return
				}
				if view := (&Scheduler{admission: g}).AdmissionSnapshot(safe.sequence); !strings.HasPrefix(view.BackfillMessage, "backfilled: measured finish within remaining head-wait window") {
					t.Fatalf("safe backfill explanation=%q", view.BackfillMessage)
				}
			})
		}
	}
}
