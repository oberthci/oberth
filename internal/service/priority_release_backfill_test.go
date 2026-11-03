package service

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestPriorityReleaseHeadMessageMatchesBackfillPolicy(t *testing.T) {
	for _, priority := range []model.RunPriority{model.PriorityNormal, model.PriorityHigh, model.PriorityHighest} {
		t.Run(string(priority), func(t *testing.T) {
			clock := &fakeClock{now: time.Now()}
			g := newWeightedAdmission(4)
			g.now, g.maxHeadWait = clock.Now, 20*time.Minute
			holder := g.reserve(1)
			release, err := holder.acquire(t.Context(), periapsis.M)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			head := priorityWaiter(g, 2, priority, clock.now, periapsis.L)
			defer head.cancel()
			scheduler := &Scheduler{admission: g}
			message := waitingAdmissionMessage(scheduler.AdmissionSnapshot(2))
			want := "every later run waits behind it"
			if priority == model.PriorityNormal {
				want = "backfilling"
			}
			if !strings.Contains(message, want) {
				t.Fatalf("head message=%q, want %q", message, want)
			}
		})
	}
}
