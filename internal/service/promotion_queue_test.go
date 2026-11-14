package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

// After Shutdown no background work starts: a late automatic moved-promotion
// retry (the scheduler can deliver one while it drains) and a promotion that
// would queue are both refused cleanly, and racing starts against Shutdown
// never panic the WaitGroup.
func TestNoBackgroundWorkStartsAfterShutdown(t *testing.T) {
	// A real failed promotion: its fast-forward delivery hits an SSH reset,
	// so a late automatic retry would have something to admit.
	git := &scriptedPromotionGit{
		plans:  []gitcache.MergeCandidate{{BaseSHA: retryBaseSHA, MergedSHA: retrySourceSHA, FastForward: true}},
		pushes: []scriptedPush{{err: errors.New("ssh reset")}},
	}
	fixture, _, control := newPromotionRetryHarness(t, git)
	ctx := context.Background()
	admin := api.Actor{Identity: "admin@host", Fingerprint: "SHA256:admin", Admin: true}
	id := promoteForRetry(t, control, admin)
	failed, err := fixture.store.Promotion(ctx, id)
	if err != nil || failed.Status != model.PromotionFailed {
		t.Fatalf("setup promotion = %#v, %v; want failed", failed, err)
	}
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
	control.RetryMovedPromotion(ctx, failed)
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
	case <-time.After(testStartTimeout(2 * time.Second)):
		t.Fatal("background work outlived Shutdown")
	}
}

// Both promotions test the same base concurrently. Only the first result may
// publish; the second needs a new merge and a new passing Workflow.
func TestConcurrentPromotionsRetestMovedTargetWithoutDoublePublish(t *testing.T) {
	const secondSource = "2222222222222222222222222222222222222222"
	const staleMerge = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	const freshMerge = "ffffffffffffffffffffffffffffffffffffffff"
	git := &scriptedPromotionGit{plans: []gitcache.MergeCandidate{
		{BaseSHA: retryBaseSHA, MergedSHA: retryMergedSHA},
		{BaseSHA: retryBaseSHA, MergedSHA: staleMerge},
		{BaseSHA: retryMergedSHA, MergedSHA: freshMerge},
	}}
	fixture, scheduler, service := newPromotionRetryHarness(t, git)
	seedGreenPromotionCandidate(t, fixture, secondSource)
	engine := newGroupEngine()
	scheduler.jobs, scheduler.releaseJobs = engine, engine
	actor := api.Actor{Identity: "agent@host", Admin: true}
	firstID := promoteForRetry(t, service, actor)
	value, err := service.CallTool(t.Context(), actor, "promote", json.RawMessage(`{"sha":"`+secondSource+`","branch":"main"}`))
	if err != nil {
		t.Fatal(err)
	}
	secondID := value.(api.PromoteResponse).ID
	first, err := fixture.store.Promotion(t.Context(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.store.Promotion(t.Context(), secondID)
	if err != nil || first.RunID == "" || second.RunID == "" || second.PreviousSHA != retryBaseSHA {
		t.Fatalf("promotion waited instead of testing immediately: first=%+v second=%+v err=%v", first, second, err)
	}
	_, cancel := startDeadlineScheduler(t, scheduler)
	defer cancel()
	awaitCreated := func(count int) {
		t.Helper()
		for range count {
			select {
			case <-engine.created:
			case <-time.After(testStartTimeout(10 * time.Second)):
				t.Fatal("promotion Workflow not created")
			}
		}
	}
	finish := func(id string) {
		t.Helper()
		engine.mu.Lock()
		job := engine.byRun[id]
		engine.mu.Unlock()
		if job == nil {
			t.Fatal("missing Workflow", id)
		}
		job.result <- passedPromotionCI
	}
	awaitPromotion := func(id string, status model.PromotionStatus) model.Promotion {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), testStartTimeout(15*time.Second))
		defer cancel()
		for {
			wake := fixture.signals.Promotion(id)
			promotion, err := fixture.store.Promotion(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if promotion.Status == status {
				return promotion
			}
			if promotion.Status.Terminal() {
				t.Fatalf("promotion ended unexpectedly: %+v", promotion)
			}
			select {
			case <-wake:
			case <-ctx.Done():
				t.Fatal("promotion stalled", promotion)
			}
		}
	}
	awaitCreated(2)
	finish(first.RunID)
	awaitPromotion(firstID, model.PromotionPassed)
	finish(second.RunID)
	awaitPromotion(secondID, model.PromotionFailed)
	// A moved-target retry must reach a third Workflow before it can publish.
	awaitCreated(1)
	service.waitBackground()
	lineage, err := fixture.store.PromotionLineage(t.Context(), secondID)
	if err != nil || lineage.RetriedAs == "" {
		t.Fatal(lineage, err)
	}
	retry, err := fixture.store.Promotion(t.Context(), lineage.RetriedAs)
	if err != nil || retry.PreviousSHA != retryMergedSHA || retry.ResultSHA != freshMerge || retry.RunID == second.RunID {
		t.Fatalf("moved target did not re-prepare: %+v %v", retry, err)
	}
	if prepared, pushed := git.counts(); prepared != 3 || pushed != 1 {
		t.Fatalf("stale result published or retry skipped CI: prepares=%d pushes=%d", prepared, pushed)
	}
	finish(retry.RunID)
	awaitPromotion(retry.ID, model.PromotionPassed)
	git.mu.Lock()
	pushed := append([]string(nil), git.pushed...)
	git.mu.Unlock()
	if len(pushed) != 2 || pushed[0] != "main:"+retryMergedSHA || pushed[1] != "main:"+freshMerge {
		t.Fatalf("publication must contain only tested current results: %v", pushed)
	}
}
