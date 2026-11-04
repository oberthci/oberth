package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// runListGroup classifies a listed run the way the run list groups it.
func runListGroup(run model.Run) int {
	switch {
	case run.Status.Terminal():
		return 2
	case run.StartedAt != nil:
		return 1
	default:
		return 0
	}
}

// TestRunListNeverSortsQueuedBelowRunningOrFinished reproduces a run list
// where queued runs, accepted before several runs started and finished, used
// to sort among the finished rows. Every view, limit and page must list the
// queue (next admission first), then running runs, then finished runs.
func TestRunListNeverSortsQueuedBelowRunningOrFinished(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	if _, err := s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	enqueue := func(ref, sha string) model.Run {
		t.Helper()
		run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat(sha, 40)))
		if err != nil {
			t.Fatal(err)
		}
		return run.Run
	}
	start := func(ref, sha string) model.Run {
		t.Helper()
		run := enqueue(ref, sha)
		if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != run.ID {
			t.Fatalf("claim %s = %+v, %v", ref, claimed, err)
		}
		if _, err := s.MarkRunAdmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetRunJobName(ctx, run.ID, "wf-"+run.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarkRunSubmitted(ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		return run
	}
	finish := func(run model.Run, status model.RunStatus) {
		t.Helper()
		if _, err := s.FinishRun(ctx, run.ID, model.RunResult{Status: status, Phase: string(status)}); err != nil {
			t.Fatal(err)
		}
	}
	// Five runs start first, then the queue fills up while they run.
	alpha := start("alpha", "a")
	now = now.Add(time.Minute)
	beta := start("beta", "b")
	now = now.Add(time.Minute)
	gamma := start("gamma", "c")
	now = now.Add(3 * time.Minute)
	runningEarly := start("running-early", "2")
	now = now.Add(5 * time.Minute)
	runningLate := start("running-late", "3")
	now = now.Add(5 * time.Minute)
	queuedNormalFirst := enqueue("feature/one", "d")
	queuedHighFirst := enqueue("rc/one", "e")
	queuedNormalSecond := enqueue("feature/two", "f")
	queuedHighSecond := enqueue("rc/two", "1")
	// The first-started runs finish after the queue was accepted, so a list
	// ordered by acceptance or admission time would mix them with the queue.
	now = now.Add(15 * time.Minute)
	finish(beta, model.RunPassed)
	now = now.Add(5 * time.Minute)
	finish(gamma, model.RunFailed)
	now = now.Add(5 * time.Minute)
	finish(alpha, model.RunPassed)

	// The queue follows scheduler claim order: high FIFO, then normal FIFO.
	wantQueue := []string{queuedHighFirst.ID, queuedHighSecond.ID, queuedNormalFirst.ID, queuedNormalSecond.ID}
	wantRunning := []string{runningLate.ID, runningEarly.ID}
	wantTerminal := []string{alpha.ID, gamma.ID, beta.ID}
	for _, latest := range []bool{false, true} {
		name := "flat"
		if latest {
			name = "latest-per-ref"
		}
		for _, limit := range []int{1, 2, 100} {
			first, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: latest, Limit: limit})
			if err != nil {
				t.Fatal(err)
			}
			terminal := wantTerminal
			if limit < len(terminal) {
				terminal = terminal[:limit]
			}
			want := append(append(append([]string{}, wantQueue...), wantRunning...), terminal...)
			if got := runIDs(first); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("%s limit %d first page = %v, want %v", name, limit, got, want)
			}
			for i, want := range []int64{1, 2, 3, 4} {
				if first[i].QueuePosition != want {
					t.Fatalf("%s limit %d queue position of %s = %d, want %d", name, limit, first[i].Ref, first[i].QueuePosition, want)
				}
			}
			// Older pages continue the finished runs only.
			var page []model.Run
			for cursor := first; len(cursor) > 0 && cursor[len(cursor)-1].Status.Terminal(); cursor = page {
				page, err = s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: latest, Limit: limit, Before: cursor[len(cursor)-1].QueueSequence})
				if err != nil {
					t.Fatal(err)
				}
				for _, run := range page {
					if runListGroup(run) != 2 {
						t.Fatalf("%s limit %d older page listed active run %s", name, limit, run.Ref)
					}
				}
				if len(page) == 0 {
					break
				}
				terminal = append(terminal, runIDs(page)...)
			}
			if strings.Join(terminal, ",") != strings.Join(wantTerminal, ",") {
				t.Fatalf("%s limit %d paged finished runs = %v, want %v", name, limit, terminal, wantTerminal)
			}
		}
	}
	// The listed queue order is the real admission order.
	for i := range wantQueue {
		claimed, err := s.ClaimNextRun(ctx)
		if err != nil || claimed.ID != wantQueue[i] {
			t.Fatalf("admission order diverged from the list at %d: %+v, %v", i, claimed, err)
		}
	}
}

// TestRunListGroupsAreMonotonicWithRepositoryFilter keeps the group order
// under a repository filter and when the queue holds a claimed run that has
// not started: it is still waiting, so it stays in the queue group.
func TestRunListGroupsAreMonotonicWithRepositoryFilter(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	other, err := s.CreateRepository(t.Context(), model.RepositorySpec{Name: "other", UpstreamID: repo.UpstreamID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	finished := projectionRun(t, s, repo.ID, "finished", projectionSourceSHA)
	now = now.Add(time.Minute)
	if _, err := s.FinishRun(ctx, finished.ID, model.RunResult{Status: model.RunFailed}); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "claimed", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ClaimNextRun(ctx); err != nil || got.ID != claimed.ID {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	foreign, err := s.EnqueueRun(ctx, testRunSpec(other.ID, "foreign", strings.Repeat("b", 40)))
	if err != nil {
		t.Fatal(err)
	}
	queued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "queued", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	for _, latest := range []bool{false, true} {
		rows, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: latest, RepoID: repo.ID, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{claimed.ID, queued.ID, finished.ID}
		if got := runIDs(rows); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("latest=%v filtered list = %v, want %v", latest, got, want)
		}
		if rows[0].QueuePosition != 1 || rows[1].QueuePosition != 3 {
			t.Fatalf("latest=%v global queue positions = %d, %d", latest, rows[0].QueuePosition, rows[1].QueuePosition)
		}
		scoped, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: latest, RepoIDs: []int64{other.ID}, Limit: 1})
		if err != nil || strings.Join(runIDs(scoped), ",") != foreign.ID || scoped[0].QueuePosition != 2 {
			t.Fatalf("latest=%v scoped list = %v, %v", latest, scoped, err)
		}
	}
}

// A claimed run that has not started (durable status running, phase waiting,
// no started_at; the wire's "waiting" projection) is a queued run: it lists
// in the queue group, never among running or finished runs, in both views.
func TestRunListKeepsClaimedUnstartedRunsInTheQueue(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
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
	running := enqueue("running", "a")
	if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != running.ID {
		t.Fatalf("claim running = %+v, %v", claimed, err)
	}
	if _, err := s.MarkRunAdmitted(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, running.ID, "wf-"+running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkRunSubmitted(ctx, running.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	finished := enqueue("finished", "b")
	if claimed, err := s.ClaimNextRun(ctx); err != nil || claimed.ID != finished.ID {
		t.Fatalf("claim finished = %+v, %v", claimed, err)
	}
	if _, err := s.MarkRunAdmitted(ctx, finished.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, finished.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	claimed := enqueue("claimed", "c")
	if got, err := s.ClaimNextRun(ctx); err != nil || got.ID != claimed.ID {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	now = now.Add(time.Minute)
	queued := enqueue("queued", "d")

	for _, latest := range []bool{false, true} {
		runs, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: latest, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		// The claimed run is next in claim order, followed by unclaimed work.
		want := []string{claimed.ID, queued.ID, running.ID, finished.ID}
		if got := runIDs(runs); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("latest=%v list = %v, want %v", latest, got, want)
		}
		if runs[0].Status != model.RunRunning || runs[0].StartedAt != nil || runs[0].Phase != "waiting" || !runs[0].ClaimedUnstarted() {
			t.Fatalf("claimed run is not the raw running/unstarted record: %+v", runs[0])
		}
		if model.DisplayRunStatus(runs[0]) != model.DisplayQueued || runListGroup(runs[0]) != 0 {
			t.Fatalf("claimed run must display queued in the queue group: %+v", runs[0])
		}
		if runs[0].QueuePosition != 1 || runs[1].QueuePosition != 2 {
			t.Fatalf("queue positions = %d, %d; want claimed 1, queued 2", runs[0].QueuePosition, runs[1].QueuePosition)
		}
	}
}
