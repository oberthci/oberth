package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// sizedByBranch sizes each run from its branch name ("s-3" is S), so one
// scheduler can be fed a mix of sizes.
type sizedByBranch struct{ *concurrentJobs }

func (jobs sizedByBranch) PipelineSize(_ context.Context, request JobRequest) (periapsis.Size, error) {
	prefix, _, _ := strings.Cut(strings.TrimPrefix(request.Run.Ref, "pack/"), "-")
	return periapsis.Size(strings.ToUpper(prefix)), nil
}

func runPacking(t *testing.T, maxConcurrentJobs, expected int, branches []string) (admitted int) {
	t.Helper()
	fixture := newConcurrencyFixture(t, periapsis.M, maxConcurrentJobs)
	jobs := newConcurrentJobs(periapsis.M, len(branches)+1)
	fixture.scheduler.jobs = sizedByBranch{jobs}
	fixture.scheduler.releaseJobs = sizedByBranch{jobs}
	var waiting model.Run
	for index, branch := range branches {
		enqueued, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
			EventID: "pack-" + branch, Repository: fixture.repo, Branch: "pack/" + branch,
			SHA: fmt.Sprintf("%040x", index+1), Actor: "agent@host",
		})
		if err != nil {
			t.Fatal(err)
		}
		waiting = enqueued.Run
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var schedulerErr error
	go func() {
		schedulerErr = fixture.scheduler.Run(ctx)
		close(finished)
	}()
	t.Cleanup(func() {
		close(jobs.release)
		cancel()
		select {
		case <-finished:
		case <-time.After(testTimeout(15 * time.Second)):
			t.Error("scheduler did not stop")
		}
	})
	// Wait for every expected Job creation, not for a quiet interval that
	// can expire before even the first admission on a busy host.
	deadline := time.NewTimer(testTimeout(20 * time.Second))
	defer deadline.Stop()
	for admitted < expected {
		select {
		case <-jobs.admitted:
			admitted++
		case <-finished:
			t.Fatalf("scheduler stopped after %d admissions: %v", admitted, schedulerErr)
		case <-deadline.C:
			t.Fatalf("admitted %d runs, want %d before the deadline", admitted, expected)
		}
	}
	// All Jobs stay in Wait, so full weight cannot become available. The
	// extra run must either remain queued behind a closed dispatch gate or
	// have a prepared reservation waiting for weight. Neither needs silence.
	poll := time.NewTicker(testTimeout(10 * time.Millisecond))
	defer poll.Stop()
	for {
		state := fixture.scheduler.AdmissionState()
		if state.Runs != expected || state.Used != state.Capacity {
			t.Fatalf("packing did not fill exactly the budget: %+v", state)
		}
		run, err := fixture.store.Run(ctx, waiting.ID)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := fixture.scheduler.AdmissionSnapshot(waiting.QueueSequence)
		if run.Status == model.RunQueued && !fixture.scheduler.dispatchSlotAvailable(expected) {
			return admitted
		}
		if snapshot != nil && snapshot.State == "waiting" && snapshot.Used+snapshot.Weight > snapshot.Capacity {
			return admitted
		}
		select {
		case runID := <-jobs.admitted:
			t.Fatalf("admitted extra run %s past the weight budget", runID)
		case <-finished:
			t.Fatalf("scheduler stopped before the extra run waited: %v", schedulerErr)
		case <-deadline.C:
			t.Fatalf("extra run never waited for weight: run=%+v admission=%+v", run, snapshot)
		case <-poll.C:
		}
	}
}

// TestSmallRunsPackIntoTheWeightBudget is #1021's acceptance: with
// maxConcurrentJobs=10 the weight budget is 20, so twenty S runs run together
// where the old run ceiling stopped at ten, and the run past the budget waits.
func TestSmallRunsPackIntoTheWeightBudget(t *testing.T) {
	branches := make([]string, 21)
	for index := range branches {
		branches[index] = fmt.Sprintf("s-%d", index)
	}
	admitted := runPacking(t, 10, 20, branches)
	if admitted != 20 {
		t.Fatalf("admitted %d S runs together, want 20 (the weight budget)", admitted)
	}
}

// TestMixedSizesRespectTheWeightBudget keeps the budget honest once runs may
// outnumber maxConcurrentJobs: 4 L (12) + 3 M (6) + 2 S (2) is exactly 20, and
// the S queued behind them must wait.
func TestMixedSizesRespectTheWeightBudget(t *testing.T) {
	branches := []string{"l-0", "l-1", "l-2", "l-3", "m-0", "m-1", "m-2", "s-0", "s-1", "s-2"}
	admitted := runPacking(t, 10, 9, branches)
	if admitted != 9 {
		t.Fatalf("admitted %d runs, want 9 (weight 20 of 20, the tenth waits)", admitted)
	}
}
