package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

func TestDurableBusySkipCatchesUpAfterRestart(t *testing.T) {
	for _, previousFire := range []bool{false, true} {
		name := "first-fire"
		if previousFire {
			name = "previous-fire"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "schedules.db")
			state, err := store.Open(ctx, path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = state.Close() })
			if previousFire {
				if err := state.RecordScheduleFire(ctx, "alpha", "nightly", instant(t, "2026-08-23T03:00:00Z"), "fired"); err != nil {
					t.Fatal(err)
				}
			}
			git := &fakeScheduleGit{
				sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
				blob: map[string]string{"alpha": nightlyFile},
			}
			schedules, enqueuer, runs, _ := testSchedules(t, git, oneRepo())
			schedules.config.State = state
			schedules.observed = instant(t, "2026-08-24T00:00:00Z")
			runs.active[1] = true
			skipped := instant(t, "2026-08-24T03:05:00Z")
			schedules.Tick(ctx, skipped)
			outcomes, err := state.ScheduleOutcomes(ctx, "alpha")
			if err != nil || len(outcomes) != 1 || outcomes[0].Outcome != "skipped" || !outcomes[0].FiredAt.Equal(skipped) {
				t.Fatalf("latest skip is not reportable: %+v, %v", outcomes, err)
			}
			if len(enqueuer.requests) != 0 {
				t.Fatal("busy repository enqueued a run")
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			state, err = store.Open(ctx, path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			config := schedules.config
			config.State = state
			restarted := NewSchedules(config)
			restarted.observed = skipped.Add(time.Minute)
			restarted.Tick(ctx, skipped.Add(time.Minute)) // Still busy after restart.
			runs.active[1] = false
			caughtUp := skipped.Add(2 * time.Minute)
			restarted.Tick(ctx, caughtUp)
			if len(enqueuer.requests) != 1 || enqueuer.requests[0].ScheduleEntry != "nightly" {
				t.Fatalf("idle tick did not catch up after restart: %+v", enqueuer.requests)
			}
			restarted.Tick(ctx, caughtUp.Add(time.Minute))
			if len(enqueuer.requests) != 1 {
				t.Fatal("catch-up fire repeated")
			}
			restarted.Tick(ctx, instant(t, "2026-08-25T03:01:00Z"))
			if len(enqueuer.requests) != 2 {
				t.Fatal("next daily occurrence did not resume")
			}
		})
	}
}
