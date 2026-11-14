package service

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func deleteFixtureBranch(t *testing.T, f *groupFixture, repository model.Repository, ref string) {
	t.Helper()
	pushes, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: &stubPushGit{}, Receives: f.store, Branches: f.scheduler, Auditor: f.store})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewReceiveHandler(pushes)
	if err != nil {
		t.Fatal(err)
	}
	if err = handler.HandleReceive(t.Context(), gitcache.ReceiveEvent{ID: "delete-" + repository.Name + "-" + ref, Repo: repository.Name, Actor: "agent@host", Update: gitcache.RefUpdate{Ref: "refs/heads/" + ref, OldSHA: strings.Repeat("a", 40), NewSHA: strings.Repeat("0", 40)}}); err != nil {
		t.Fatal(err)
	}
}

func TestRefDeletionStopsRunningAndAssistedWorkflows(t *testing.T) {
	for _, assist := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "assisted"}[assist], func(t *testing.T) {
			f := newGroupFixture(t, 0)
			f.engine.declareCPU("feature/deleted", 2000)
			f.engine.declareCPU("feature/other", 2000)
			busy, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "busy", Repository: f.exampleService, Branch: "feature/deleted", SHA: strings.Repeat("a", 40), Actor: "agent@host", Assist: assist})
			if err != nil {
				t.Fatal(err)
			}
			other := f.branch(t, f.exampleService, "feature/other", "b")
			f.start(t)
			if made := f.awaitCreated(t, 2); !made[busy.ID] || !made[other.ID] {
				t.Fatal("wrong run admitted", made)
			}
			submitted := f.awaitRunStarted(t, busy.ID)
			deleteFixtureBranch(t, f, f.exampleService, busy.Ref)
			f.awaitNoPendingCancellations(t)
			stopped := f.awaitTerminal(t, busy.ID)
			if !stopped.Cancelled() || stopped.Reason != "ref deleted" {
				t.Fatal("deleted run result overwritten", stopped)
			}
			shown := f.control.presentationRun(t.Context(), stopped)
			if shown.DisplayStatus != model.DisplayFailed || shown.DisplayReason != "ref deleted" {
				t.Fatal("deletion reason not projected", shown)
			}
			f.engine.mu.Lock()
			deleted := slices.Contains(f.engine.deleted, submitted.WorkflowName)
			f.engine.mu.Unlock()
			if !deleted || f.scheduler.ownerForTest(busy.QueueSequence) != nil {
				t.Fatal("workflow or CPU reservation retained after cleanup")
			}
			if current := f.run(t, other.ID); current.Cancelled() {
				t.Fatal("other ref cancelled", current)
			}
			f.assertNoCIIssue(t, f.exampleService, busy.Ref)
			f.engine.finish(t, other.ID)
			f.awaitTerminal(t, other.ID)
			f.engine.assertNoViolations(t)
		})
	}
}

func TestRefDeletionCancelsQueuedAndAdmissionHeldRuns(t *testing.T) {
	for _, claimed := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued", true: "admission-held"}[claimed], func(t *testing.T) {
			f := newGroupFixture(t, 0)
			lift := f.pausePreparation(t)
			old := f.branch(t, f.exampleService, "feature/deleted", "a")
			other := f.branch(t, f.terraform, "feature/other", "b")
			if claimed {
				f.start(t)
				f.awaitPreparing(t, old)
			}
			deleteFixtureBranch(t, f, f.exampleService, old.Ref)
			if !claimed {
				f.start(t)
			}
			lift()
			stopped := f.awaitTerminal(t, old.ID)
			if !stopped.Cancelled() || stopped.Reason != "ref deleted" {
				t.Fatal("pending run survived deletion", stopped)
			}
			f.awaitNoPendingCancellations(t)
			if made := f.awaitCreated(t, 1); !made[other.ID] || f.engine.wasCreated(old.ID) {
				t.Fatal("deleted run held queue or created workflow", made)
			}
			f.engine.finish(t, other.ID)
			f.awaitTerminal(t, other.ID)
		})
	}
}

func TestPromotionSourcePruneLeavesPendingPromotionCIAlone(t *testing.T) {
	f, git := newPrunePromotionFixture(t)
	queued, promotion, err := storetest.EnqueuePromotionRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/test", SHA: f.initial, TestedSHA: f.initial, BaseSHA: f.initial, Actor: "dev@host", Trigger: "promotion"}, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/a", SourceSHA: f.initial, TargetRef: "main", PreviousSHA: f.initial, ResultSHA: f.initial, Actor: "dev@host"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.RequestPromotionPrune(t.Context(), promotion.ID, promotion.Actor); err != nil {
		t.Fatal(err)
	}
	request, err := f.store.PromotionPrune(t.Context(), promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Pending promotion work cannot prune its own source before verification.
	got := f.service.settlePromotionPrune(t.Context(), f.store, promotion, request, promotion.CreatedAt)
	if got.Status != model.PromotionPruneKept || !f.mirrorHas(t, promotion.SourceBranch) {
		t.Fatal("pending promotion pruned source", got)
	}
	// Exercise the real post-verification prune path while a separate promotion
	// still needs the same source. Neither may be cancelled by source pruning.
	git.head = f.initial
	passed, prune := f.promotion(t, promotion.SourceBranch, f.initial, model.PromotionPassed, f.initial)
	got = f.service.settlePromotionPrune(t.Context(), f.store, passed, prune, passed.CreatedAt)
	if got.Status != model.PromotionPrunePruned || f.mirrorHas(t, promotion.SourceBranch) {
		t.Fatal("promotion prune stopped working", got)
	}
	current, err := f.store.Run(t.Context(), queued.ID)
	if err != nil || current.Status != model.RunQueued || current.Cancelled() {
		t.Fatalf("source prune cancelled promotion: %+v %v", current, err)
	}
	if pending, err := f.store.PendingRunCancellations(t.Context()); err != nil || len(pending) != 0 {
		t.Fatalf("prune created cancellation: %+v %v", pending, err)
	}
}

type pausedDeletionReceipt struct {
	ReceiveRecorder
	committed, resume chan struct{}
}

func (r *pausedDeletionReceipt) RecordReceiveEvent(ctx context.Context, event model.ReceiveEventSpec) (bool, error) {
	duplicate, err := r.ReceiveRecorder.RecordReceiveEvent(ctx, event)
	close(r.committed)
	select {
	case <-r.resume:
	case <-ctx.Done():
		return duplicate, ctx.Err()
	}
	return duplicate, err
}

func TestRefDeletionOverlappingRePushDoesNotCancelNewRun(t *testing.T) {
	f := newGroupFixture(t, 0)
	old := f.branch(t, f.exampleService, "feature/recreated", "a")
	recorder := &pausedDeletionReceipt{ReceiveRecorder: f.store, committed: make(chan struct{}), resume: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(recorder.resume) })
	defer resume()
	pushes, err := NewPushIngestor(PushConfig{Repositories: f.store, Discoverer: stubRepositoryDiscoverer{}, Git: &stubPushGit{}, Receives: recorder, Branches: f.scheduler})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pushes.Branch(t.Context(), BranchPush{EventID: "delete", Repository: f.exampleService.Name, Branch: old.Ref, Actor: "agent@host", OldOID: old.SHA, NewOID: strings.Repeat("0", 40)})
		done <- err
	}()
	select {
	case <-recorder.committed:
	case <-time.After(testStartTimeout(15 * time.Second)):
		t.Fatal("deletion did not commit")
	}
	// Accept an identical SHA before the deletion callback returns. Cleanup must
	// use the atomically captured old run, not re-select all current ref runs.
	newer, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "re-push", Repository: f.exampleService, Branch: old.Ref, SHA: old.SHA, Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	resume()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	f.start(t)
	if created := f.awaitCreated(t, 1); !created[newer.ID] || f.engine.wasCreated(old.ID) {
		t.Fatal("delete callback cancelled later push", created)
	}
	stopped := f.run(t, old.ID)
	if !stopped.Cancelled() || stopped.Reason != "ref deleted" || f.run(t, newer.ID).Cancelled() {
		t.Fatal("delete/re-push states incorrect")
	}
	f.engine.finish(t, newer.ID)
	f.awaitTerminal(t, newer.ID)
}
