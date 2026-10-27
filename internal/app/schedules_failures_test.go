package app

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
)

type failingScheduleState struct {
	ScheduleState
	readErr  error
	writeErr error
}

func (state *failingScheduleState) ScheduleFires(ctx context.Context, repo string) (map[string]time.Time, error) {
	if state.readErr != nil {
		return nil, state.readErr
	}
	return state.ScheduleState.ScheduleFires(ctx, repo)
}

func (state *failingScheduleState) RecordScheduleFire(ctx context.Context, repo, entry string, at time.Time, outcome string) error {
	if state.writeErr != nil {
		return state.writeErr
	}
	return state.ScheduleState.RecordScheduleFire(ctx, repo, entry, at, outcome)
}

func TestScheduleStateReadFailureDoesNotRefire(t *testing.T) {
	git := &fakeScheduleGit{sha: map[string]string{"alpha": strings.Repeat("a", 40)}, blob: map[string]string{"alpha": nightlyFile}}
	schedules, enqueuer, _, _ := testSchedules(t, git, oneRepo())
	state := &failingScheduleState{ScheduleState: NewMemoryScheduleState()}
	schedules.config.State = state
	var logs bytes.Buffer
	schedules.config.Logger = log.New(&logs, "", 0)
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	fired := instant(t, "2026-08-24T03:05:00Z")
	schedules.Tick(t.Context(), fired)
	state.readErr = errors.New("injected schedule state read failure")
	schedules.Tick(t.Context(), fired.Add(2*time.Minute))
	if len(enqueuer.requests) != 1 {
		t.Fatalf("state read failure refired nightly: %+v", enqueuer.requests)
	}
	if !strings.Contains(logs.String(), "injected schedule state read failure") || !strings.Contains(logs.String(), `repo="alpha"`) {
		t.Fatalf("state failure invisible: %s", &logs)
	}
	state.readErr = nil
	schedules.Tick(t.Context(), fired.Add(3*time.Minute))
	if len(enqueuer.requests) != 1 {
		t.Fatal("state read failure advanced or erased successful cursor")
	}
}

func TestScheduleEnqueueReadFailureIsVisibleAndRetriesWithoutAdvancingCursor(t *testing.T) {
	git := &fakeScheduleGit{sha: map[string]string{"alpha": strings.Repeat("a", 40)}, blob: map[string]string{"alpha": nightlyFile}}
	schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
	memory := NewMemoryScheduleState()
	schedules.config.State = memory
	var logs bytes.Buffer
	schedules.config.Logger = log.New(&logs, "", 0)
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	at := instant(t, "2026-08-24T03:05:00Z")
	enqueuer.err = errors.New("read active schedule: injected query failure")
	schedules.Tick(t.Context(), at)
	cursors, err := memory.ScheduleFires(t.Context(), "alpha")
	if err != nil || !cursors["nightly"].IsZero() || !strings.Contains(logs.String(), "injected query failure") {
		t.Fatalf("failed query consumed cursor or was silent: %+v, %v, %s", cursors, err, &logs)
	}
	enqueuer.err = nil
	schedules.Tick(t.Context(), at.Add(time.Minute))
	if len(enqueuer.requests) != 2 {
		t.Fatalf("pending nightly was not retried: %+v (%v)", enqueuer.requests, state.events)
	}
}

func TestScheduleRecordFailureIsLogged(t *testing.T) {
	schedules, _, _, _ := testSchedules(t, &fakeScheduleGit{}, oneRepo())
	schedules.config.State = &failingScheduleState{ScheduleState: NewMemoryScheduleState(), writeErr: errors.New("injected outcome write failure")}
	var logs bytes.Buffer
	schedules.config.Logger = log.New(&logs, "", 0)
	schedules.record(t.Context(), "alpha", "nightly", time.Now(), "failed")
	if !strings.Contains(logs.String(), "injected outcome write failure") || !strings.Contains(logs.String(), `entry="nightly"`) {
		t.Fatalf("outcome error ignored: %s", &logs)
	}
}

func TestScheduleCronDoesNotWriteCursorAfterAtomicEnqueue(t *testing.T) {
	git := &fakeScheduleGit{sha: map[string]string{"alpha": strings.Repeat("a", 40)}, blob: map[string]string{"alpha": nightlyFile}}
	schedules, enqueuer, _, _ := testSchedules(t, git, oneRepo())
	state := &failingScheduleState{ScheduleState: NewMemoryScheduleState()}
	schedules.config.State = state
	var logs bytes.Buffer
	schedules.config.Logger = log.New(&logs, "", 0)
	schedules.observed = instant(t, "2026-08-24T00:00:00Z")
	enqueuer.committed = func(request service.CIRequest, fire model.ScheduleFireSpec) error {
		if err := state.RecordScheduleFire(t.Context(), fire.Repo, request.ScheduleEntry, fire.At, "fired"); err != nil {
			return err
		}
		state.writeErr = errors.New("app tried a separate cursor write")
		return nil
	}
	schedules.Tick(t.Context(), instant(t, "2026-08-24T03:05:00Z"))
	if logs.Len() != 0 || len(enqueuer.requests) != 1 {
		t.Fatalf("post-enqueue cursor write: requests=%d logs=%s", len(enqueuer.requests), &logs)
	}
}

func (state *failingScheduleState) SchedulePauses(ctx context.Context, repo string) (map[string]model.SchedulePause, error) {
	if reader, ok := state.ScheduleState.(SchedulePauseReader); ok {
		return reader.SchedulePauses(ctx, repo)
	}
	return nil, nil
}

func TestScheduleStateFailurePreservesPausedOutcome(t *testing.T) {
	schedules, enqueuer, _, state := pausedSchedules(t)
	git := schedules.config.Git.(*fakeScheduleGit)
	git.blob["alpha"] += "  - name: weekly\n    cron: \"0 4 * * 1\"\n"
	state.pauses["nightly"] = model.SchedulePause{By: "admin@host"}
	at := instant(t, "2026-08-24T03:05:00Z")
	if err := state.RecordScheduleFire(t.Context(), "alpha", "nightly", at, "paused"); err != nil {
		t.Fatal(err)
	}
	schedules.config.State = &failingScheduleState{ScheduleState: state, readErr: errors.New("cursor unavailable")}
	schedules.Tick(t.Context(), at.Add(time.Minute))
	if len(enqueuer.requests) != 0 || len(state.events) != 2 || state.events[0] != "alpha/nightly:paused" || state.events[1] != "alpha/weekly:failed_state" {
		t.Fatalf("state failure overwrote pause or enqueued: events=%v requests=%v", state.events, enqueuer.requests)
	}
}

func TestSchedulePostCommitErrorPreservesOutcome(t *testing.T) {
	for _, outcome := range []string{"fired", "coalesced"} {
		t.Run(outcome, func(t *testing.T) {
			git := &fakeScheduleGit{sha: map[string]string{"alpha": strings.Repeat("a", 40)}, blob: map[string]string{"alpha": nightlyFile}}
			schedules, enqueuer, _, state := testSchedules(t, git, oneRepo())
			schedules.observed = instant(t, "2026-08-24T00:00:00Z")
			var logs bytes.Buffer
			schedules.config.Logger = log.New(&logs, "", 0)
			enqueuer.result.ID = "committed-run"
			enqueuer.committed = func(request service.CIRequest, fire model.ScheduleFireSpec) error {
				if err := state.RecordScheduleFire(t.Context(), fire.Repo, request.ScheduleEntry, fire.At, outcome); err != nil {
					return err
				}
				return errors.New("post-commit acceptance error")
			}
			schedules.Tick(t.Context(), instant(t, "2026-08-24T03:05:00Z"))
			if len(state.events) != 1 || state.events[0] != "alpha/nightly:"+outcome {
				t.Fatalf("committed outcome overwritten: %v", state.events)
			}
			if !strings.Contains(logs.String(), "post-commit acceptance error") {
				t.Fatalf("acceptance error hidden: %s", &logs)
			}
		})
	}
}
