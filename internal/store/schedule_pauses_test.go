package store

import (
	"context"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestSchedulePausesFollowTheLatestPauseOrResumeAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()
	record := func(actor, action, repo, entry, details string) {
		t.Helper()
		now = now.Add(time.Minute)
		if _, err := database.AppendAuditAction(ctx, model.AuditActionSpec{
			Actor: actor, Action: action, ResourceType: "schedule", ResourceID: ScheduleAuditResourceID(repo, entry), Details: details,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("a@host", "schedule.pause", "github/org/repo", "nightly", `{"reason":"saturated"}`)
	record("a@host", "schedule.pause", "github/org/repo", "weekly#odd", `{}`)
	record("a@host", "schedule.pause", "github/org/other", "nightly", `{}`)

	got, err := database.SchedulePauses(ctx, "github/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["nightly"].By != "a@host" || got["nightly"].Reason != "saturated" || got["weekly#odd"].By != "a@host" {
		t.Fatalf("pauses = %+v", got)
	}

	record("b@host", "schedule.resume", "github/org/repo", "nightly", `{}`)
	record("b@host", "schedule.trigger", "github/org/repo", "weekly#odd", `{}`)
	got, err = database.SchedulePauses(ctx, "github/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if _, paused := got["nightly"]; paused || len(got) != 1 {
		t.Fatalf("after resume = %+v, want only weekly#odd (a trigger does not resume)", got)
	}
	other, err := database.SchedulePauses(ctx, "github/org/other")
	if err != nil || len(other) != 1 {
		t.Fatalf("other repository = %+v, %v", other, err)
	}
}
