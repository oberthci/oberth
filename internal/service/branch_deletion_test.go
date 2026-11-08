package service

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

// blockingUpstream never answers until its context ends, like the forge in
// the v0.17.95 incident.
type blockingUpstream struct {
	mu        sync.Mutex
	calls     int
	abandoned []string
	refuse    bool
}

func (g *blockingUpstream) PropagateBranchDeletions(ctx context.Context, _ string, deletions []gitcache.UpstreamDeletion) ([]gitcache.UpstreamDeletionResult, error) {
	g.mu.Lock()
	g.calls++
	refuse := g.refuse
	g.mu.Unlock()
	if refuse {
		results := make([]gitcache.UpstreamDeletionResult, len(deletions))
		for index, deletion := range deletions {
			results[index] = gitcache.UpstreamDeletionResult{Branch: deletion.Branch, State: gitcache.UpstreamRefused, Err: errors.New("upstream moved")}
		}
		return results, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (g *blockingUpstream) AbandonBranchDeletions(_ context.Context, _ string, branches []string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.abandoned = append(g.abandoned, branches...)
	return nil
}

// A forge that never answers holds neither receives nor the propagator:
// receives keep returning while a pass is blocked, the pass ends at its
// budget, the record stays pending for retry and finally fails after the
// bounded attempts, restoring the mirror branch.
func TestBlockingUpstreamNeverBlocksReceiveAndRetriesBounded(t *testing.T) {
	f := newControlFixture(t)
	upstream := &blockingUpstream{}
	propagator, err := NewBranchDeletionPropagator(f.store, upstream, f.store, func(context.Context) error { return nil }, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	propagator.budget = 200 * time.Millisecond
	clock := time.Now()
	propagator.now = func() time.Time { return clock }
	ingestor, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: &stubPushGit{}, Receives: f.store, Auditor: f.store, DeletionQueued: propagator.Notify})
	if err != nil {
		t.Fatal(err)
	}
	deleteBranch := func(id, branch string) {
		t.Helper()
		started := time.Now()
		result, err := ingestor.Branch(t.Context(), BranchPush{EventID: id, Repository: f.repo.Name, Branch: branch, Actor: "operator@host",
			OldOID: strings.Repeat("a", 40), NewOID: strings.Repeat("0", 40)})
		if err != nil || !result.Deleted {
			t.Fatalf("delete %s = %+v, %v", branch, result, err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("receive of %s took %s", branch, elapsed)
		}
	}
	deleteBranch("event-1", "chore/917-followups")
	passDone := make(chan error, 1)
	go func() {
		_, err := propagator.Propagate(context.Background())
		passDone <- err
	}()
	// The propagator is now blocked on the forge; another receive returns.
	deleteBranch("event-2", "auto/cli-v0.19.133")
	select {
	case err := <-passDone:
		if err != nil {
			t.Fatalf("pass = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("propagation pass ignored its budget")
	}
	records, err := f.store.BranchDeletions(t.Context(), model.BranchDeletionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Branch == "chore/917-followups" && (record.Status != model.BranchDeletionPending || record.Attempts != 1 || !record.RetryAfter.After(clock) || record.Error == "") {
			t.Fatalf("blocked record = %+v", record)
		}
	}
	for range branchDeletionMaxAttempts + 1 {
		clock = clock.Add(time.Hour)
		if _, err := propagator.Propagate(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	records, err = f.store.BranchDeletions(t.Context(), model.BranchDeletionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Status != model.BranchDeletionFailed || record.Attempts != branchDeletionMaxAttempts || !strings.Contains(record.Error, "attempts failed") {
			t.Fatalf("exhausted record = %+v", record)
		}
	}
	if len(upstream.abandoned) != 2 {
		t.Fatalf("abandoned = %v", upstream.abandoned)
	}
}

func TestRefusedUpstreamDeletionFailsAtOnce(t *testing.T) {
	f := newControlFixture(t)
	upstream := &blockingUpstream{refuse: true}
	propagator, err := NewBranchDeletionPropagator(f.store, upstream, f.store, func(context.Context) error { return nil }, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RecordReceiveEvent(t.Context(), receiveSpec("event", "operator@host", f.repo.ID, model.RefBranch, "feature/x", strings.Repeat("b", 40), "", "", "deleted")); err != nil {
		t.Fatal(err)
	}
	if finished, err := propagator.Propagate(t.Context()); err != nil || finished != 1 {
		t.Fatalf("propagate = %d, %v", finished, err)
	}
	records, err := f.store.BranchDeletionBatch(t.Context(), "event")
	if err != nil || len(records) != 1 || records[0].Status != model.BranchDeletionFailed || !strings.Contains(records[0].Error, "moved") {
		t.Fatalf("records = %+v, %v", records, err)
	}
	if strings.Join(upstream.abandoned, ",") != "feature/x" {
		t.Fatalf("abandoned = %v", upstream.abandoned)
	}
}

type mirrorBranchGit struct {
	*retryPublicationGit
	exists bool
}

func (g *mirrorBranchGit) MirrorBranchExists(context.Context, string, string) (bool, error) {
	return g.exists, nil
}

// A feature-branch publication whose branch was deleted on the mirror after
// its run started must not force-push the branch back upstream, and must not
// turn the green run red.
func TestPublicationSkipsBranchDeletedOnMirror(t *testing.T) {
	for _, exists := range []bool{true, false} {
		t.Run(map[bool]string{true: "present", false: "deleted"}[exists], func(t *testing.T) {
			f := newControlFixture(t)
			run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/gone", SHA: retrySourceSHA, Actor: "agent@host", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.store.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			pub, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: "feature/gone", ResultSHA: run.SHA, Actor: run.Actor})
			if err != nil {
				t.Fatal(err)
			}
			g := &mirrorBranchGit{retryPublicationGit: &retryPublicationGit{controlGit: f.git}, exists: exists}
			final, err := deliverPublication(t.Context(), f.scheduler.database(), g, f.store, f.repo, f.repo.Name, pub)
			if err != nil {
				t.Fatal(err)
			}
			if exists {
				if g.forced != 1 || final.Publication.Status != model.PublicationDelivered {
					t.Fatalf("present branch: forced=%d final=%+v", g.forced, final.Publication)
				}
				return
			}
			if g.forced != 0 || final.Publication.Status != model.PublicationAbandoned || !strings.Contains(final.Publication.Error, "deleted on the mirror") {
				t.Fatalf("deleted branch: forced=%d publication=%+v", g.forced, final.Publication)
			}
			// The run's steps passed: it finishes passed with a reason line,
			// not failed/abandoned (#1500).
			if final.Run.Status != model.RunPassed || final.Run.Phase != "passed" || final.Run.Error != "" || !strings.Contains(final.Run.Reason, "deleted on the mirror") {
				t.Fatalf("deleted branch run = %+v", final.Run)
			}
		})
	}
}
