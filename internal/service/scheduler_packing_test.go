package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// sizedByBranch sizes each run from its branch name ("s-3" is S), so one
// scheduler can be fed a mix of sizes.
type sizedByBranch struct{ *concurrentJobs }

func (jobs sizedByBranch) PipelineSize(_ context.Context, request JobRequest) (periapsis.Size, error) {
	prefix, _, _ := strings.Cut(strings.TrimPrefix(request.Run.Ref, "pack/"), "-")
	return periapsis.Size(strings.ToUpper(prefix)), nil
}

func runPacking(t *testing.T, maxConcurrentJobs int, branches []string) (admitted int, stop func()) {
	t.Helper()
	fixture := newConcurrencyFixture(t, periapsis.M, maxConcurrentJobs)
	jobs := newConcurrentJobs(periapsis.M, len(branches)+1)
	fixture.scheduler.jobs = sizedByBranch{jobs}
	fixture.scheduler.releaseJobs = sizedByBranch{jobs}
	for index, branch := range branches {
		if _, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
			EventID: "pack-" + branch, Repository: fixture.repo, Branch: "pack/" + branch,
			SHA: fmt.Sprintf("%040x", index+1), Actor: "agent@host",
		}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- fixture.scheduler.Run(ctx) }()
	// Count Job creations until none arrives for a settle window.
	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-jobs.admitted:
			admitted++
			continue
		case err := <-finished:
			t.Fatalf("scheduler stopped after %d admissions: %v", admitted, err)
		case <-deadline:
			t.Fatalf("admissions did not settle (%d so far)", admitted)
		case <-time.After(750 * time.Millisecond):
		}
		break
	}
	return admitted, func() {
		close(jobs.release)
		cancel()
		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Error("scheduler did not stop")
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
	admitted, stop := runPacking(t, 10, branches)
	defer stop()
	if admitted != 20 {
		t.Fatalf("admitted %d S runs together, want 20 (the weight budget)", admitted)
	}
}

// TestMixedSizesRespectTheWeightBudget keeps the budget honest once runs may
// outnumber maxConcurrentJobs: 4 L (12) + 3 M (6) + 2 S (2) is exactly 20, and
// the S queued behind them must wait.
func TestMixedSizesRespectTheWeightBudget(t *testing.T) {
	branches := []string{"l-0", "l-1", "l-2", "l-3", "m-0", "m-1", "m-2", "s-0", "s-1", "s-2"}
	admitted, stop := runPacking(t, 10, branches)
	defer stop()
	if admitted != 9 {
		t.Fatalf("admitted %d runs, want 9 (weight 20 of 20, the tenth waits)", admitted)
	}
}
