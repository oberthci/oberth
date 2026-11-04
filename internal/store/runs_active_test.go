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
		if _, err := s.SetRunWorkflowName(ctx, run.ID, "oberth-"+run.ID[:12]); err != nil {
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
	// The not-started rows, claimed or not, are one queue in queue_position order.
	for i, run := range active[2:] {
		if run.QueuePosition != int64(i+1) {
			t.Fatalf("queue position of %s = %d, want %d", run.Ref, run.QueuePosition, i+1)
		}
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

	// The default list has one queue in claim order,
	// then running runs newest start first, then finished runs.
	all, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantAll := []string{waiting.ID, queuedFirst.ID, hidden.ID, queuedSecond.ID, runningLate.ID, runningEarly.ID, finished.ID}
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

// The queue is one order in every view, claimed and unclaimed runs together,
// exactly as the scheduler claims: a claimed normal run never lists ahead of
// an unclaimed high run the gate admits first, and an aged run moves with
// its effective class. Flat, latest-per-ref and active views agree.
func TestQueueOrderIsMonotoneAcrossClaimedAndUnclaimedClasses(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	if _, err := s.SetQueuePriorityPatterns(ctx, "admin", []string{"rc/*"}); err != nil {
		t.Fatal(err)
	}
	enqueue := func(ref, sha string) model.Run {
		t.Helper()
		result, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, ref, strings.Repeat(sha, 40)))
		if err != nil {
			t.Fatal(err)
		}
		return result.Run
	}
	rank := map[model.RunPriority]int{model.PriorityLow: -1, model.PriorityStandard: 0, model.PriorityPromotion: 1, model.PriorityHigh: 2}
	check := func(label string, wantAdmission []string) {
		t.Helper()
		active, err := s.ListActiveRuns(ctx, model.RunListFilter{})
		if err != nil {
			t.Fatal(err)
		}
		flat, err := s.ListRecentRuns(ctx, model.RunListFilter{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		latest, err := s.ListRecentRuns(ctx, model.RunListFilter{LatestPerRef: true, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		queueOf := func(runs []model.Run, bottomUp bool) []model.Run {
			queue := []model.Run{}
			for _, run := range runs {
				if run.Status == model.RunQueued || run.ClaimedUnstarted() {
					queue = append(queue, run)
				}
			}
			if bottomUp {
				for i, j := 0, len(queue)-1; i < j; i, j = i+1, j-1 {
					queue[i], queue[j] = queue[j], queue[i]
				}
			}
			return queue
		}
		for name, queue := range map[string][]model.Run{"active": queueOf(active, false), "flat": queueOf(flat, false), "latest-per-ref": queueOf(latest, false)} {
			if got := runIDs(queue); strings.Join(got, ",") != strings.Join(wantAdmission, ",") {
				t.Fatalf("%s: %s queue in admission order = %v, want %v", label, name, got, wantAdmission)
			}
			previousRank := rank[model.PriorityHighest] + 1
			for i, run := range queue {
				if run.QueuePosition != int64(i+1) {
					t.Fatalf("%s: %s queue position of %s = %d, want %d", label, name, run.Ref, run.QueuePosition, i+1)
				}
				class, _ := run.EffectivePriority(now)
				if rank[class] > previousRank {
					t.Fatalf("%s: %s queue interleaves classes at %s (%s after a lower class)", label, name, run.Ref, class)
				}
				previousRank = rank[class]
			}
		}
	}

	claimed := enqueue("feature/claimed", "a") // normal, claimed before any high work arrives
	if got, err := s.ClaimNextRun(ctx); err != nil || got.ID != claimed.ID {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	now = now.Add(time.Minute)
	high := enqueue("rc/one", "b") // high: admitted before the claimed normal run
	now = now.Add(time.Minute)
	fresh := enqueue("feature/fresh", "c")
	check("high arrives after a normal claim", []string{high.ID, claimed.ID, fresh.ID})

	// An hour later the claimed run and the fresh one have aged into high;
	// the queue is FIFO within the high class, still one order everywhere.
	now = now.Add(61 * time.Minute)
	check("aged", []string{claimed.ID, high.ID, fresh.ID})
	later := enqueue("feature/later", "d")
	check("fresh normal behind aged work", []string{claimed.ID, high.ID, fresh.ID, later.ID})
}
