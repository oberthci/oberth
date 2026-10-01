package store

import (
	"context"
	"testing"
	"time"
)

func TestScheduleFiresRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	ctx := context.Background()

	if err := store.RecordScheduleFire(ctx, "alpha", "nightly", now, "fired"); err != nil {
		t.Fatal(err)
	}
	fires, err := store.ScheduleFires(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := fires["nightly"]
	if !ok {
		t.Fatalf("nightly not recorded: %+v", fires)
	}
	if !got.Equal(now) {
		t.Fatalf("recorded %s, want %s", got, now)
	}
}

func TestOnlyAFiredOutcomeAdvancesTheClock(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	ctx := context.Background()

	for _, outcome := range []string{"skipped", "refused", "failed"} {
		if err := store.RecordScheduleFire(ctx, "alpha", "nightly", now, outcome); err != nil {
			t.Fatal(err)
		}
	}
	fires, err := store.ScheduleFires(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fires["nightly"]; ok {
		t.Fatal("a skipped or refused entry advanced its clock, so the fire it missed would never happen")
	}
}

func TestARecordedFireIsUpdatedNotDuplicated(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	ctx := context.Background()

	first := now
	second := now.Add(24 * time.Hour)
	if err := store.RecordScheduleFire(ctx, "alpha", "nightly", first, "fired"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordScheduleFire(ctx, "alpha", "nightly", second, "fired"); err != nil {
		t.Fatal(err)
	}
	fires, err := store.ScheduleFires(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(fires) != 1 {
		t.Fatalf("two fires for one entry produced %d rows", len(fires))
	}
	if !fires["nightly"].Equal(second) {
		t.Fatalf("recorded %s, want the later fire %s", fires["nightly"], second)
	}
}

func TestScheduleOutcomesAreReadableForReporting(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	ctx := context.Background()

	if err := store.RecordScheduleFire(ctx, "alpha", "nightly", now, "fired"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordScheduleFire(ctx, "alpha", "hammer", now, "refused"); err != nil {
		t.Fatal(err)
	}
	outcomes, err := store.ScheduleOutcomes(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	byName := map[string]string{}
	for _, outcome := range outcomes {
		byName[outcome.Entry] = outcome.Outcome
	}
	if byName["hammer"] != "refused" {
		t.Fatalf("a refused entry is not reportable: %+v", outcomes)
	}
}

func TestScheduleFiresOfAnUnknownRepositoryIsEmpty(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	fires, err := store.ScheduleFires(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("reading an unknown repository is not an error: %v", err)
	}
	if len(fires) != 0 {
		t.Fatalf("fires = %+v", fires)
	}
}

func TestRecordScheduleFireRefusesEmptyIdentifiers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 24, 3, 0, 0, 0, time.UTC)
	store := testStore(t, &now)
	if err := store.RecordScheduleFire(context.Background(), "", "nightly", now, "fired"); err == nil {
		t.Fatal("a fire with no repository was recorded")
	}
}
