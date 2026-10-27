package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

func TestScheduleLegacyBusySkipQueuesAfterRestartWhileRepositoryRemainsBusy(t *testing.T) {
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
			// Emulate the old cron busy gate's durable state at upgrade.
			if err := state.RecordScheduleFire(ctx, "alpha", "nightly", skipped, "skipped"); err != nil {
				t.Fatal(err)
			}
			outcomes, err := state.ScheduleOutcomes(ctx, "alpha")
			if err != nil || len(outcomes) != 1 || outcomes[0].Outcome != "skipped" || !outcomes[0].FiredAt.Equal(skipped) {
				t.Fatalf("latest skip is not reportable: %+v, %v", outcomes, err)
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
			schedules.config.State = state
			restarted := NewSchedules(config)
			restarted.observed = skipped.Add(time.Minute)
			caughtUp := skipped.Add(time.Minute)
			restarted.Tick(ctx, caughtUp)
			if len(enqueuer.requests) != 1 || enqueuer.requests[0].ScheduleEntry != "nightly" {
				t.Fatalf("busy tick did not queue the legacy pending occurrence after restart: %+v", enqueuer.requests)
			}
			outcomes, err = state.ScheduleOutcomes(ctx, "alpha")
			if err != nil || len(outcomes) != 1 || outcomes[0].Outcome != "fired" || !outcomes[0].FiredAt.Equal(caughtUp) {
				t.Fatalf("queued occurrence is not reportable: %+v, %v", outcomes, err)
			}
			if err := state.Close(); err != nil {
				t.Fatal(err)
			}
			state, err = store.Open(ctx, path, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			config.State = state
			schedules.config.State = state
			restarted = NewSchedules(config)
			restarted.observed = caughtUp.Add(time.Minute)
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
