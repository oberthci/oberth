package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func runIDs(runs []model.Run) []string {
	ids := make([]string, len(runs))
	for index, run := range runs {
		ids[index] = run.ID
	}
	return ids
}

// A run still waiting for admission must lead the queue part of the active
// list even though newer runs are running or finished, and the history view
// must never repeat it.
func TestListActiveRunsOrdersByStartThenAdmissionThenQueue(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	other, err := s.CreateRepository(context.Background(), model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	enqueue := func(repoID int64, ref string) model.Run {
		t.Helper()
		now = now.Add(time.Second)
		result, err := s.EnqueueRun(ctx, testRunSpec(repoID, ref, strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	claim := func(want model.Run) {
		t.Helper()
		claimed, err := s.ClaimNextRun(ctx)
		if err != nil || claimed.ID != want.ID {
			t.Fatalf("claim = %s, %v; want %s", claimed.ID, err, want.ID)
		}
	}
	submit := func(run model.Run) {
		t.Helper()
		now = now.Add(time.Second)
		if _, err := s.SetRunJobName(ctx, run.ID, "oberth-"+run.ID[:12]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkRunSubmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	waiting := enqueue(repo.ID, "feature/waiting")    // oldest: claimed, never admitted
	runningLate := enqueue(repo.ID, "feature/late")   // started second
	finished := enqueue(repo.ID, "feature/finished")  // terminal
	runningEarly := enqueue(repo.ID, "feature/early") // started first
	for _, run := range []model.Run{waiting, runningLate, finished, runningEarly} {
		claim(run)
	}
	submit(runningEarly)
	submit(runningLate)
	submit(finished)
	if _, err := s.FinishRun(ctx, finished.ID, model.RunResult{Status: model.RunFailed, Phase: "ci"}); err != nil {
		t.Fatal(err)
	}
	queuedFirst := enqueue(repo.ID, "feature/q1")
	hidden := enqueue(other.ID, "feature/hidden")
	queuedSecond := enqueue(repo.ID, "feature/q2")

	active, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{runningEarly.ID, runningLate.ID, waiting.ID, queuedFirst.ID, hidden.ID, queuedSecond.ID}
	if got := runIDs(active); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("active order = %v, want %v", got, want)
	}

	// The uplink repository restriction applies to the active view.
	scoped, err := s.ListActiveRuns(ctx, model.RunListFilter{RepoIDs: []int64{other.ID}})
	if err != nil || strings.Join(runIDs(scoped), ",") != hidden.ID {
		t.Fatalf("restricted active = %v, %v", runIDs(scoped), err)
	}
	if none, err := s.ListActiveRuns(ctx, model.RunListFilter{RepoIDs: []int64{}}); err != nil || len(none) != 0 {
		t.Fatalf("empty restriction active = %v, %v", runIDs(none), err)
	}
	byRef, err := s.ListActiveRuns(ctx, model.RunListFilter{RepoID: repo.ID, Ref: "feature/waiting"})
	if err != nil || strings.Join(runIDs(byRef), ",") != waiting.ID {
		t.Fatalf("ref-filtered active = %v, %v", runIDs(byRef), err)
	}

	// The default list keeps its creation order, active runs interleaved.
	all, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantAll := []string{queuedSecond.ID, hidden.ID, queuedFirst.ID, runningEarly.ID, finished.ID, runningLate.ID, waiting.ID}
	if got := runIDs(all); strings.Join(got, ",") != strings.Join(wantAll, ",") {
		t.Fatalf("recent order = %v, want %v", got, wantAll)
	}
	history, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 100, Terminal: true})
	if err != nil || strings.Join(runIDs(history), ",") != finished.ID {
		t.Fatalf("history = %v, %v", runIDs(history), err)
	}
}

// History pages by queue_sequence exactly like the default list, skipping
// active runs without shortening a page.
func TestListRecentRunsTerminalPagination(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	var terminal []model.Run
	for index := 0; index < 7; index++ {
		now = now.Add(time.Second)
		result, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/"+string(rune('a'+index)), strings.Repeat("b", 40)))
		if err != nil {
			t.Fatal(err)
		}
		if index%2 == 1 {
			continue // left queued: active, interleaved by sequence
		}
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		finished, err := s.FinishRun(ctx, result.ID, model.RunResult{Status: model.RunPassed, Phase: "ci"})
		if err != nil {
			t.Fatal(err)
		}
		terminal = append([]model.Run{finished}, terminal...)
	}
	first, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2, Terminal: true})
	if err != nil || len(first) != 2 || first[0].ID != terminal[0].ID || first[1].ID != terminal[1].ID {
		t.Fatalf("first history page = %v, %v", runIDs(first), err)
	}
	second, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2, Terminal: true, Before: first[1].QueueSequence})
	if err != nil || len(second) != 2 || second[0].ID != terminal[2].ID || second[1].ID != terminal[3].ID {
		t.Fatalf("second history page = %v, %v", runIDs(second), err)
	}
	if rest, _ := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 2, Terminal: true, Before: second[1].QueueSequence}); len(rest) != 0 {
		t.Fatalf("history past the oldest = %v", runIDs(rest))
	}
}
