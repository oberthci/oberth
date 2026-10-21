package store

import (
	"context"
	"errors"
	"fmt"
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
	now = now.Add(time.Minute)
	readmitted, err := s.MarkRunAdmitted(t.Context(), run.ID)
	if err != nil || readmitted.StartedAt == nil || !readmitted.StartedAt.Equal(now) {
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

func TestFlatRunsGlobalQueuePositionsAndCounts(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	other, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	var queued []model.Run
	for i, id := range []int64{other.ID, repo.ID, other.ID, repo.ID} {
		run, err := s.EnqueueRun(t.Context(), testRunSpec(id, fmt.Sprintf("queue-%d", i), strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, run.Run)
	}
	if _, err := s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: repo.ID, Limit: 1})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v, %v", rows, err)
	}
	for i, want := range []int64{2, 4} {
		if rows[i].QueuePosition != want {
			t.Fatalf("position %+v, want %d", rows[i], want)
		}
		position, err := s.QueuedRunPosition(t.Context(), rows[i].ID)
		if err != nil || position != want {
			t.Fatalf("single position %d, %v", position, err)
		}
	}
	counts, err := s.CountRuns(t.Context())
	if err != nil || counts.Queued != 4 || counts.Running != 0 || counts.QueuedByRepo[repo.ID] != 2 {
		t.Fatalf("counts %+v, %v", counts, err)
	}
	if _, err := s.MarkRunAdmitted(t.Context(), queued[0].ID); err != nil {
		t.Fatal(err)
	}
	counts, err = s.CountRuns(t.Context())
	if err != nil || counts.Total != 4 || counts.Queued != 3 || counts.Running != 1 || counts.RunningByRepo[other.ID] != 1 {
		t.Fatalf("admitted counts %+v, %v", counts, err)
	}
	rows, err = s.ListRecentRuns(t.Context(), model.RunListFilter{RepoID: other.ID})
	if err != nil || len(rows) != 2 || rows[0].ID != queued[2].ID || rows[0].QueuePosition != 2 {
		t.Fatalf("claimed queue %+v, %v", rows, err)
	}
}

func TestStrandedRequeueRecordsCauseAndDoesNotFileIssue(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	old := projectionRun(t, s, repo.ID, "restart", projectionSourceSHA)
	copy, err := s.RequeueStrandedRun(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	old, err = s.Run(t.Context(), old.ID)
	if err != nil || old.Reason != "re-queued after restart" || old.SupersededBy != copy.ID {
		t.Fatalf("restart %+v, %v", old, err)
	}
	if _, err = s.OpenCIIssue(t.Context(), repo.ID, "restart"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("restart issue: %v", err)
	}
}

func TestInterruptedRunNeitherOpensNorResolvesIssue(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	finish := func(status model.RunStatus) {
		t.Helper()
		run := projectionRun(t, s, repo.ID, "interrupt", projectionSourceSHA)
		if _, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: status, Error: "scheduler stopped"}); err != nil {
			t.Fatal(err)
		}
	}
	finish(model.RunInterrupted)
	if _, err := s.OpenCIIssue(t.Context(), repo.ID, "interrupt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("interrupted created issue: %v", err)
	}
	finish(model.RunFailed)
	before, err := s.OpenCIIssue(t.Context(), repo.ID, "interrupt")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	finish(model.RunInterrupted)
	after, err := s.OpenCIIssue(t.Context(), repo.ID, "interrupt")
	if err != nil || before.ID != after.ID || before.Body != after.Body || before.Occurrences != after.Occurrences || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("interrupted changed issue %+v -> %+v (%v)", before, after, err)
	}
}

func TestFlatRunsPrunedCursorIsNotFound(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "pruned", strings.Repeat("a", 40))
	spec.RefKind, spec.Release, spec.Trigger = model.RefTag, true, "tag"
	run, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(t.Context(), `DELETE FROM runs WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []model.RunListFilter{{Before: run.QueueSequence}, {BeforeRun: run.ID}} {
		if _, err = s.ListRecentRuns(t.Context(), filter); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "restart from the first page") {
			t.Fatalf("pruned cursor: %v", err)
		}
	}
}

func TestFlatRunsFinishedRowsSortByAdmissionNotFinish(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	first := projectionRun(t, s, repo.ID, "first", projectionSourceSHA)
	if _, err := s.MarkRunAdmitted(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	second := projectionRun(t, s, repo.ID, "second", projectionSourceSHA)
	if _, err := s.MarkRunAdmitted(t.Context(), second.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{second.ID, first.ID} {
		now = now.Add(time.Minute)
		if _, err := s.FinishRun(t.Context(), id, model.RunResult{Status: model.RunPassed}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListRecentRuns(t.Context(), model.RunListFilter{})
	if err != nil || len(rows) != 2 || rows[0].ID != second.ID || rows[1].ID != first.ID {
		t.Fatalf("finish order displaced admission order: %+v, %v", rows, err)
	}
}
