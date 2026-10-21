package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestFlatRunsMissingCursorIsNotFound(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	for _, filter := range []model.RunListFilter{{Before: 12345}, {BeforeRun: "missing-run"}} {
		if _, err := s.ListRecentRuns(t.Context(), filter); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cursor %+v: got %v, want not found", filter, err)
		}
	}
}

func TestParkedRunLosesAdmissionTimeUntilReacquired(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "parked", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunParked(t.Context(), run.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("park without admission: %v", err)
	}
	if _, err := s.MarkRunAdmitted(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	parked, err := s.MarkRunParked(t.Context(), run.ID)
	if err != nil || parked.StartedAt != nil {
		t.Fatalf("parked: %+v, %v", parked, err)
	}
	readmitted, err := s.MarkRunAdmitted(t.Context(), run.ID)
	if err != nil || readmitted.StartedAt == nil {
		t.Fatalf("readmitted: %+v, %v", readmitted, err)
	}
	if _, err := s.SetRunJobName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunParked(t.Context(), run.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("park after job creation: %v", err)
	}
}

func TestFlatRunsKeepQueueFirstAndPageByAdmissionTime(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	enqueue := func(ref, sha string) model.Run {
		t.Helper()
		run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat(sha, 40)))
		if err != nil {
			t.Fatal(err)
		}
		return run.Run
	}
	finish := func(ref, sha string) model.Run {
		t.Helper()
		run := enqueue(ref, sha)
		if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != run.ID {
			t.Fatalf("claim %s = %+v, %v", ref, claimed, err)
		}
		now = now.Add(time.Minute)
		if _, err := s.MarkRunAdmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRunJobName(ctx, run.ID, "wf-"+run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkRunSubmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
			t.Fatal(err)
		}
		return run
	}
	a := finish("a", "a")
	b := finish("b", "b")
	c := finish("c", "c")
	firstQueued := enqueue("queued-1", "d")
	secondQueued := enqueue("queued-2", "e")
	first, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2})
	if err != nil || strings.Join(runIDs(first), ",") != strings.Join([]string{firstQueued.ID, secondQueued.ID, c.ID, b.ID}, ",") {
		t.Fatalf("flat first page = %v, %v", runIDs(first), err)
	}
	// Admission changes the first page but cannot duplicate the moved run on
	// the older page, whose cursor is the admitted run B's fixed start time.
	if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != firstQueued.ID {
		t.Fatalf("claim queued run = %+v, %v", claimed, err)
	}
	now = now.Add(time.Minute)
	if _, err := s.MarkRunAdmitted(ctx, firstQueued.ID); err != nil {
		t.Fatal(err)
	}
	older, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2, BeforeRun: b.ID})
	if err != nil || strings.Join(runIDs(older), ",") != a.ID {
		t.Fatalf("older page after admission = %v, %v", runIDs(older), err)
	}
	// A queued run cancelled before admission has no start time; its end
	// time places it among dated rows, ahead of older admitted runs.
	neverAdmitted := enqueue("cancelled", "f")
	now = now.Add(time.Minute)
	if _, err := s.CancelRun(ctx, model.RunCancelRequest{RunID: neverAdmitted.ID, Actor: "owner@host"}); err != nil {
		t.Fatal(err)
	}
	current, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2})
	if err != nil || strings.Join(runIDs(current), ",") != strings.Join([]string{secondQueued.ID, neverAdmitted.ID, firstQueued.ID}, ",") {
		t.Fatalf("never-admitted terminal order = %v, %v", runIDs(current), err)
	}
}
