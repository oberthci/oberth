package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func awaitRunStatus(t *testing.T, fixture *concurrencyFixture, runID string, want model.RunStatus) model.Run {
	t.Helper()
	deadline := time.Now().Add(testStartTimeout(15 * time.Second))
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
		time.Sleep(testTimeout(20 * time.Millisecond))
	}
}

// TestLegacyXLAnnotationDoesNotRequireAWorkflowAdmissionGrant is #1021 part 1: a
// repository without an XL grant that declares XL is refused before it takes
// any weight, with the command that would grant it, and another repository's
// run queued behind it is admitted at once instead of waiting for a drain.
func TestLegacyXLAnnotationDoesNotRequireAWorkflowAdmissionGrant(t *testing.T) {
	fixture := newConcurrencyFixture(t, pipeline.M, 3)
	jobs := newConcurrentJobs(pipeline.M, 4)
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

	created := map[string]bool{}
	for len(created) < 2 {
		select {
		case id := <-jobs.admitted:
			created[id] = true
		case <-time.After(testStartTimeout(10 * time.Second)):
			t.Fatal("a historical size grant blocked workflow submission")
		}
	}
	if !created[heavy] || !created[light] || fixture.scheduler.AdmissionState().NumericBudget != nil {
		t.Fatalf("Kubernetes placement still depends on size: %v", created)
	}

}

// TestTagRunsAreNotSizeGated pins the deliberate exemption: the XL size grant
// applies to branch, promotion and schedule runs only. A tag run is the
// credentialed tier, sized by its reviewed release.yaml, and is admitted at XL
// without any grant; every other trigger of the same repository is refused.
func TestTagRunsAreNotSizeGated(t *testing.T) {
	fixture := newConcurrencyFixture(t, pipeline.M, 3)
	ctx := context.Background()
	if size, err := fixture.store.MaxRunSize(ctx, fixture.repo.ID); err != nil || size != pipeline.L {
		t.Fatalf("fixture repository maximum = %q, %v; want the default L", size, err)
	}
	tag := model.Run{RepoID: fixture.repo.ID, RefKind: model.RefTag, Ref: "v1.0.0", Trigger: "tag", Release: true}
	if err := fixture.scheduler.authorizeRunSize(ctx, tag, fixture.repo, pipeline.XL); err != nil {
		t.Fatalf("tag run at XL without a grant = %v", err)
	}
	for _, trigger := range []string{"branch", "promotion", "schedule"} {
		run := model.Run{RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: "main", Trigger: trigger}
		if err := fixture.scheduler.authorizeRunSize(ctx, run, fixture.repo, pipeline.XL); err == nil {
			t.Fatalf("%s run at XL without a grant was admitted", trigger)
		}
	}
}
