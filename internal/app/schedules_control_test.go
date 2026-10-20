package app

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

// pausedScheduleState is a fakeScheduleState that also reports pauses and
// stored outcomes.
type pausedScheduleState struct {
	fakeScheduleState
	pauses   map[string]model.SchedulePause
	outcomes []store.ScheduleOutcome
}

func (s *pausedScheduleState) SchedulePauses(context.Context, string) (map[string]model.SchedulePause, error) {
	return s.pauses, nil
}

func (s *pausedScheduleState) ScheduleOutcomes(context.Context, string) ([]store.ScheduleOutcome, error) {
	return s.outcomes, nil
}

func pausedSchedules(t *testing.T) (*Schedules, *fakeEnqueuer, *fakeRunLister, *pausedScheduleState) {
	t.Helper()
	git := &fakeScheduleGit{
		sha:  map[string]string{"alpha": "0123456789abcdef0123456789abcdef01234567"},
		blob: map[string]string{"alpha": nightlyFile},
	}
	schedules, enqueuer, runs, _ := testSchedules(t, git, oneRepo())
	state := &pausedScheduleState{pauses: map[string]model.SchedulePause{}}
	schedules.config.State = state
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	return schedules, enqueuer, runs, state
}

func TestAPausedEntryRecordsPausedInsteadOfFiringAndResumes(t *testing.T) {
	t.Parallel()
	schedules, enqueuer, _, state := pausedSchedules(t)
	state.pauses["nightly"] = model.SchedulePause{By: "admin@host", At: instant(t, "2026-08-24T01:00:00Z"), Reason: "forge saturated"}

	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:05:00Z"))
	if len(enqueuer.requests) != 0 {
		t.Fatalf("a paused entry enqueued %d runs", len(enqueuer.requests))
	}
	if len(state.events) != 1 || state.events[0] != "alpha/nightly:paused" {
		t.Fatalf("events = %v, want one paused outcome", state.events)
	}

	delete(state.pauses, "nightly")
	schedules.Tick(context.Background(), instant(t, "2026-08-24T03:06:00Z"))
	if len(enqueuer.requests) != 1 {
		t.Fatalf("a resumed entry enqueued %d runs, want 1", len(enqueuer.requests))
	}
}

func TestInspectScheduleReportsNextOutcomeAndPause(t *testing.T) {
	t.Parallel()
	schedules, _, _, state := pausedSchedules(t)
	state.pauses["nightly"] = model.SchedulePause{By: "admin@host", At: instant(t, "2026-08-24T01:00:00Z"), Reason: "hold"}
	state.outcomes = []store.ScheduleOutcome{{Entry: "nightly", Outcome: "paused", FiredAt: instant(t, "2026-08-24T03:05:00Z")}}

	view, ok, err := schedules.InspectSchedule(context.Background(), oneRepo()[0])
	if err != nil || !ok {
		t.Fatalf("InspectSchedule = %v, %v", ok, err)
	}
	if view.Repo != "alpha" || len(view.Entries) != 1 {
		t.Fatalf("view = %+v", view)
	}
	entry := view.Entries[0]
	if entry.Name != "nightly" || entry.NextAt == "" || entry.LastOutcome != "paused" ||
		entry.Paused == nil || entry.Paused.By != "admin@host" || entry.Paused.Reason != "hold" {
		t.Fatalf("entry = %+v", entry)
	}
	if _, ok, err := schedules.InspectSchedule(context.Background(), model.Repository{ID: 2, Name: "beta", DefaultBranch: "main"}); err != nil || ok {
		t.Fatalf("repository without a schedule = %v, %v", ok, err)
	}
}

func TestManualTriggerUsesTheNormalEnqueuePathAsTheCaller(t *testing.T) {
	t.Parallel()
	schedules, enqueuer, runs, _ := pausedSchedules(t)
	repository := oneRepo()[0]

	plan, err := schedules.PlanScheduleTrigger(context.Background(), repository, "nightly", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := schedules.EnqueueSchedule(context.Background(), plan, "admin@host"); err != nil {
		t.Fatal(err)
	}
	if len(enqueuer.requests) != 1 {
		t.Fatalf("enqueued %d runs", len(enqueuer.requests))
	}
	request := enqueuer.requests[0]
	if request.Trigger != "schedule" || request.Actor != "admin@host" || request.Branch != "main" ||
		request.SHA != "0123456789abcdef0123456789abcdef01234567" || request.EventID == "" || request.Repository.ID != repository.ID {
		t.Fatalf("request = %+v", request)
	}

	if _, err := schedules.PlanScheduleTrigger(context.Background(), repository, "missing", false); !errors.Is(err, service.ErrInvalidInput) {
		t.Fatalf("unknown entry error = %v", err)
	}
	runs.active[repository.ID] = true
	if _, err := schedules.PlanScheduleTrigger(context.Background(), repository, "nightly", false); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("busy repository error = %v, want invalid state", err)
	}
	if _, err := schedules.PlanScheduleTrigger(context.Background(), repository, "nightly", true); err != nil {
		t.Fatalf("forced plan on a busy repository: %v", err)
	}
}
