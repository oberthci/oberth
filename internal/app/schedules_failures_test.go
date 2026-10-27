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
