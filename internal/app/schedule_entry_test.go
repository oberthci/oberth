package app

import (
	"context"
	"testing"
)

func TestScheduleEntryCronAndManualKeepTheSameResolvedIdentity(t *testing.T) {
	const originalSHA = "0123456789abcdef0123456789abcdef01234567"
	git := &fakeScheduleGit{sha: map[string]string{"alpha": originalSHA}, blob: map[string]string{"alpha": "schedules:\n  - name: nightly\n    cron: \"0 3 * * *\"\n  - name: weekly-sweep\n    cron: \"0 3 * * 1\"\n"}}
	schedules, enqueuer, _, _ := testSchedules(t, git, oneRepo())
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))
	if len(enqueuer.requests) != 2 {
		t.Fatalf("cron enqueued %d runs", len(enqueuer.requests))
	}
	for i, name := range []string{"nightly", "weekly-sweep"} {
		cron := enqueuer.requests[i]
		if cron.ScheduleEntry != name || cron.SHA != originalSHA {
			t.Fatalf("cron lost entry: %+v", cron)
		}
		plan, err := schedules.PlanScheduleTrigger(context.Background(), oneRepo()[0], name, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = schedules.EnqueueSchedule(context.Background(), plan, "admin"); err != nil {
			t.Fatal(err)
		}
		manual := enqueuer.requests[len(enqueuer.requests)-1]
		if manual.ScheduleEntry != cron.ScheduleEntry || manual.SHA != cron.SHA || manual.Branch != cron.Branch || manual.Trigger != cron.Trigger {
			t.Fatalf("manual/cron differ: %+v / %+v", manual, cron)
		}
	}
	plan, err := schedules.PlanScheduleTrigger(context.Background(), oneRepo()[0], "nightly", false)
	if err != nil {
		t.Fatal(err)
	}
	git.blob["alpha"] = "schedules:\n  - name: replacement\n    cron: \"0 3 * * *\"\n"
	git.sha["alpha"] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err = schedules.EnqueueSchedule(context.Background(), plan, "admin"); err != nil {
		t.Fatal(err)
	}
	retained := enqueuer.requests[len(enqueuer.requests)-1]
	if retained.ScheduleEntry != "nightly" || retained.SHA != originalSHA {
		t.Fatalf("plan identity changed with the file: %+v", retained)
	}
}
