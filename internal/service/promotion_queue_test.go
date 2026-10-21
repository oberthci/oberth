package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

// TestSecondPromotionWaitsAndIsPreparedOnTheNewTarget is #1021's promotion
// queue: a promotion admitted while another of the same repository and target
// is pending is not merged onto the same stale base (where it would fail with
// "upstream main moved" once the first publishes); it waits, and is prepared
// against the target only once the first promotion is terminal.
func TestSecondPromotionWaitsAndIsPreparedOnTheNewTarget(t *testing.T) {
	const (
		firstSource  = "1111111111111111111111111111111111111111"
		secondSource = "2222222222222222222222222222222222222222"
		base         = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		firstMerged  = "cccccccccccccccccccccccccccccccccccccccc"
		movedBase    = "dddddddddddddddddddddddddddddddddddddddd"
		secondMerged = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	fixture := newControlFixture(t)
	ctx := context.Background()
	for branch, sha := range map[string]string{"feature/one": firstSource, "feature/two": secondSource} {
		run, err := fixture.store.EnqueueRun(ctx, model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: branch,
			SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, TestedSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	control := fixture.api(t)
	admin := api.Actor{Identity: "agent@host", Admin: true}
	promote := func(sha string) model.Promotion {
		t.Helper()
		value, err := control.CallTool(ctx, admin, "promote", json.RawMessage(`{"sha":"`+sha+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		return requireToolPromotion(t, fixture, value)
	}
	prepared := func() int {
		fixture.git.mu.Lock()
		defer fixture.git.mu.Unlock()
		return len(fixture.git.preparedRepos)
	}

	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: base, MergedSHA: firstMerged}
	first := promote(firstSource)
	if first.Status != model.PromotionPending || first.RunID == "" {
		t.Fatalf("first promotion = %#v", first)
	}
	fixture.git.mu.Lock()
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: movedBase, MergedSHA: secondMerged}
	fixture.git.mu.Unlock()
	second := promote(secondSource)
	if second.Status != model.PromotionPending || second.RunID != "" || second.PreviousSHA != "" {
		t.Fatalf("second promotion was planned beside a pending one: %#v", second)
	}
	time.Sleep(200 * time.Millisecond)
	if prepared() != 1 {
		t.Fatalf("second promotion was prepared while the first was pending (%d prepares)", prepared())
	}

	// The first promotion ends; the second is prepared against the target as
	// it now stands and gets its own merged run.
	if _, err := fixture.store.FinishRun(ctx, first.RunID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "unit failed"}); err != nil {
		t.Fatal(err)
	}
	fixture.signals.NotifyPromotion(first.ID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		current, err := fixture.store.Promotion(ctx, second.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.RunID != "" {
			if current.PreviousSHA != movedBase || current.ResultSHA != secondMerged || current.Status != model.PromotionPending {
				t.Fatalf("second promotion after the first = %#v", current)
			}
			break
		}
		if current.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("second promotion never planned: %#v", current)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if prepared() != 2 {
		t.Fatalf("prepares = %d, want 2", prepared())
	}

	// A promotion to a different target is not queued behind either.
	fixture.git.mu.Lock()
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: base, MergedSHA: firstMerged}
	fixture.git.mu.Unlock()
	value, err := control.CallTool(ctx, admin, "promote", json.RawMessage(`{"sha":"`+firstSource+`","branch":"release-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if other := requireToolPromotion(t, fixture, value); other.RunID == "" {
		t.Fatalf("promotion to another target was queued: %#v", other)
	}
}

// A promote_retry of a failed promotion is queued behind a promotion to the
// same target that is still pending, exactly like a fresh promote (#1021
// with #1097), and is prepared only once that one is terminal.
func TestRetriedPromotionQueuesBehindAPendingOne(t *testing.T) {
	const (
		retriedSource = "3333333333333333333333333333333333333333"
		pendingSource = "4444444444444444444444444444444444444444"
		base          = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		firstMerged   = "cccccccccccccccccccccccccccccccccccccccc"
		pendingMerged = "dddddddddddddddddddddddddddddddddddddddd"
		retryMerged   = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	fixture := newControlFixture(t)
	ctx := context.Background()
	for branch, sha := range map[string]string{"feature/retried": retriedSource, "feature/pending": pendingSource} {
		run, err := fixture.store.EnqueueRun(ctx, model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: branch,
			SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, TestedSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	control := fixture.api(t)
	admin := api.Actor{Identity: "agent@host", Admin: true}
	setPlan := func(plan gitcache.MergeCandidate) {
		fixture.git.mu.Lock()
		fixture.git.plan = plan
		fixture.git.mu.Unlock()
	}
	promote := func(sha string) model.Promotion {
		t.Helper()
		value, err := control.CallTool(ctx, admin, "promote", json.RawMessage(`{"sha":"`+sha+`","branch":"main"}`))
		if err != nil {
			t.Fatal(err)
		}
		return requireToolPromotion(t, fixture, value)
	}

	// The promotion to be retried fails in its CI.
	setPlan(gitcache.MergeCandidate{BaseSHA: base, MergedSHA: firstMerged})
	failed := promote(retriedSource)
	if _, err := fixture.store.FinishRun(ctx, failed.RunID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "flaky"}); err != nil {
		t.Fatal(err)
	}
	// Another promotion to the same target is now pending.
	setPlan(gitcache.MergeCandidate{BaseSHA: base, MergedSHA: pendingMerged})
	pending := promote(pendingSource)
	if pending.RunID == "" {
		t.Fatalf("pending promotion = %#v", pending)
	}
	prepares := func() int {
		fixture.git.mu.Lock()
		defer fixture.git.mu.Unlock()
		return len(fixture.git.preparedRepos)
	}
	before := prepares()

	setPlan(gitcache.MergeCandidate{BaseSHA: pendingMerged, MergedSHA: retryMerged})
	value, err := control.CallTool(ctx, admin, "promote_retry", json.RawMessage(`{"id":"`+failed.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := value.(api.PromoteRetryResponse)
	if !ok || response.ID == "" {
		t.Fatalf("promote_retry response = %#v", value)
	}
	retried, err := fixture.store.Promotion(ctx, response.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if current, _ := fixture.store.Promotion(ctx, retried.ID); current.RunID != "" || prepares() != before {
		t.Fatalf("the retry was prepared beside a pending promotion: %#v (%d prepares)", current, prepares()-before)
	}

	if _, err := fixture.store.FinishRun(ctx, pending.RunID, model.RunResult{Status: model.RunFailed, Phase: "job", Error: "unit failed"}); err != nil {
		t.Fatal(err)
	}
	fixture.signals.NotifyPromotion(pending.ID)
	deadline := time.Now().Add(10 * time.Second)
	for {
		current, err := fixture.store.Promotion(ctx, retried.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.RunID != "" {
			if current.PreviousSHA != pendingMerged || current.ResultSHA != retryMerged {
				t.Fatalf("retry after the pending promotion = %#v", current)
			}
			return
		}
		if current.Status.Terminal() || time.Now().After(deadline) {
			t.Fatalf("retry never planned: %#v", current)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closingPromotions refuses (and counts) every promotion read once the
// store is "closed", so a test can prove nothing touches it after Shutdown.
type closingPromotions struct {
	PromotionRepository
	mu     sync.Mutex
	closed bool
	after  int
}

func (promotions *closingPromotions) touch() error {
	promotions.mu.Lock()
	defer promotions.mu.Unlock()
	if promotions.closed {
		promotions.after++
		return errors.New("store is closed")
	}
	return nil
}

func (promotions *closingPromotions) ListPromotions(ctx context.Context, filter model.PromotionListFilter) (model.PromotionPage, error) {
	if err := promotions.touch(); err != nil {
		return model.PromotionPage{}, err
	}
	return promotions.PromotionRepository.ListPromotions(ctx, filter)
}

func (promotions *closingPromotions) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	if err := promotions.touch(); err != nil {
		return model.Promotion{}, err
	}
	return promotions.PromotionRepository.Promotion(ctx, id)
}

// Shutdown with a promotion queued behind another returns promptly, nothing
// touches the store afterwards, and the queued promotion is left pending with
// no plan, which owner-startup recovery then interrupts.
func TestShutdownCancelsAQueuedPromotionWait(t *testing.T) {
	const (
		firstSource  = "5555555555555555555555555555555555555555"
		secondSource = "6666666666666666666666666666666666666666"
		base         = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		merged       = "cccccccccccccccccccccccccccccccccccccccc"
	)
	fixture := newControlFixture(t)
	ctx := context.Background()
	for branch, sha := range map[string]string{"feature/one": firstSource, "feature/two": secondSource} {
		run, err := fixture.store.EnqueueRun(ctx, model.RunSpec{
			RepoID: fixture.repo.ID, RefKind: model.RefBranch, Ref: branch,
			SHA: sha, Actor: "agent@host", Trigger: "branch", TestedSHA: sha,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.FinishRun(ctx, run.ID, model.RunResult{Status: model.RunPassed, TestedSHA: sha}); err != nil {
			t.Fatal(err)
		}
	}
	promotions := &closingPromotions{PromotionRepository: fixture.store}
	control, err := NewAPI(APIConfig{
		Runs: fixture.store, History: fixture.store, Repositories: fixture.store,
		Issues: fixture.store, Promotions: promotions, PromotionRuns: fixture.store,
		Enqueues: fixture.scheduler, Git: fixture.git, Refs: fixture.refs,
		Logs: fixture.logs, Auditor: fixture.store,
		Signals: fixture.signals, MaximumWait: 50 * time.Millisecond,
		PromotionWorkspaceRoot: filepath.Join(fixture.root, "promotion-work"),
	})
	if err != nil {
		t.Fatal(err)
	}
	admin := api.Actor{Identity: "agent@host", Admin: true}
	fixture.git.plan = gitcache.MergeCandidate{BaseSHA: base, MergedSHA: merged}
	for _, sha := range []string{firstSource, secondSource} {
		if _, err := control.CallTool(ctx, admin, "promote", json.RawMessage(`{"sha":"`+sha+`","branch":"main"}`)); err != nil {
			t.Fatal(err)
		}
	}
	page, err := fixture.store.ListPromotions(ctx, model.PromotionListFilter{RepoID: fixture.repo.ID, Limit: 10})
	if err != nil || len(page.Promotions) != 2 {
		t.Fatalf("promotions = %#v, %v", page.Promotions, err)
	}
	queued := page.Promotions[0] // newest first
	if queued.RunID != "" || queued.Status != model.PromotionPending {
		t.Fatalf("second promotion was not queued: %#v", queued)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	started := time.Now()
	if err := control.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown with a queued promotion: %v (after %s)", err, time.Since(started))
	}
	promotions.mu.Lock()
	promotions.closed = true
	promotions.mu.Unlock()
	// The notify is the proof: a planner still waiting would wake on it.
	fixture.signals.NotifyPromotion(queued.ID)
	time.Sleep(100 * time.Millisecond)
	promotions.mu.Lock()
	after := promotions.after
	promotions.mu.Unlock()
	if after != 0 {
		t.Fatalf("the queued planner touched the store %d time(s) after shutdown", after)
	}

	current, err := fixture.store.Promotion(ctx, queued.ID)
	if err != nil || current.Status != model.PromotionPending || current.RunID != "" || current.PreviousSHA != "" {
		t.Fatalf("queued promotion after shutdown = %#v, %v; want still pending and unplanned", current, err)
	}
	if err := fixture.store.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
	if current, err = fixture.store.Promotion(ctx, queued.ID); err != nil || current.Status != model.PromotionInterrupted {
		t.Fatalf("queued promotion after recovery = %#v, %v; want interrupted", current, err)
	}
}

// After Shutdown no background work starts: a late automatic moved-promotion
// retry (the scheduler can deliver one while it drains) and a promotion that
// would queue are both refused cleanly, and racing starts against Shutdown
// never panic the WaitGroup.
func TestNoBackgroundWorkStartsAfterShutdown(t *testing.T) {
	git := &scriptedPromotionGit{}
	fixture, _, control := newPromotionRetryHarness(t, git)
	ctx := context.Background()
	var racers sync.WaitGroup
	for range 32 {
		racers.Add(1)
		go func() {
			defer racers.Done()
			if control.startBackground() {
				control.background.Done()
			}
		}()
	}
	if err := control.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	racers.Wait()
	if control.startBackground() {
		t.Fatal("background work was admitted after Shutdown")
	}
	before, err := fixture.store.ListPromotions(ctx, model.PromotionListFilter{RepoID: fixture.repo.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	control.RetryMovedPromotion(ctx, model.Promotion{ID: "late", RepoID: fixture.repo.ID, SourceSHA: retrySourceSHA, TargetRef: "main"})
	after, err := fixture.store.ListPromotions(ctx, model.PromotionListFilter{RepoID: fixture.repo.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Promotions) != len(before.Promotions) {
		t.Fatal("a moved-promotion retry was admitted after Shutdown")
	}
	done := make(chan struct{})
	go func() { control.waitBackground(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background work outlived Shutdown")
	}
}
