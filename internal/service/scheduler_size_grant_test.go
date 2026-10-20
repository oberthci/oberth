package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func awaitRunStatus(t *testing.T, fixture *concurrencyFixture, runID string, want model.RunStatus) model.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		run, err := fixture.store.Run(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == want {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s is %s, want %s", runID, run.Status, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestUngrantedXLIsRefusedWithoutHoldingTheQueue is #1021 part 1: a
// repository without an XL grant that declares XL is refused before it takes
// any weight, with the command that would grant it, and another repository's
// run queued behind it is admitted at once instead of waiting for a drain.
func TestUngrantedXLIsRefusedWithoutHoldingTheQueue(t *testing.T) {
	fixture := newConcurrencyFixture(t, periapsis.M, 3)
	jobs := newConcurrentJobs(periapsis.M, 4)
	fixture.scheduler.jobs = sizedByBranch{jobs}
	other, err := fixture.store.CreateRepository(context.Background(), model.RepositorySpec{
		Name: "docs", UpstreamID: fixture.repo.UpstreamID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueue := func(repository model.Repository, branch, sha string) string {
		enqueued, err := fixture.scheduler.EnqueueCI(context.Background(), CIRequest{
			EventID: "grant-" + repository.Name + branch, Repository: repository, Branch: "pack/" + branch,
			SHA: strings.Repeat(sha, 40), Actor: "agent@host",
		})
		if err != nil {
			t.Fatal(err)
		}
		return enqueued.ID
	}
	heavy := enqueue(fixture.repo, "xl-0", "a")
	light := enqueue(other, "m-0", "b")

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- fixture.scheduler.Run(ctx) }()
	defer func() {
		close(jobs.release)
		cancel()
		<-finished
	}()

	refused := awaitRunStatus(t, fixture, heavy, model.RunFailed)
	if refused.Phase != "infrastructure" || !strings.Contains(refused.Error, "may run at most L") ||
		!strings.Contains(refused.Error, "oberth size grant oberth --max XL") {
		t.Fatalf("refused run = phase %q, error %q", refused.Phase, refused.Error)
	}
	select {
	case admitted := <-jobs.admitted:
		if admitted != light {
			t.Fatalf("admitted %s, want the other repository's run %s", admitted, light)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the other repository's run was not admitted")
	}

	// With the grant, the same declaration is admitted.
	if _, err := fixture.store.SetSizeGrant(context.Background(), fixture.repo.ID, "admin@host", periapsis.XL); err != nil {
		t.Fatal(err)
	}
	if err := fixture.scheduler.authorizeRunSize(context.Background(), refused, fixture.repo, periapsis.XL); err != nil {
		t.Fatalf("granted XL refused: %v", err)
	}
	// A tag run keeps its reviewed release.yaml size without a grant.
	if err := fixture.scheduler.authorizeRunSize(context.Background(), model.Run{RepoID: other.ID, Release: true}, other, periapsis.XL); err != nil {
		t.Fatalf("release XL refused: %v", err)
	}
	if err := fixture.scheduler.authorizeRunSize(context.Background(), model.Run{RepoID: other.ID, Trigger: "promotion"}, other, periapsis.XL); err == nil ||
		!strings.Contains(err.Error(), "promotion run") {
		t.Fatalf("ungranted promotion XL = %v", err)
	}
}
