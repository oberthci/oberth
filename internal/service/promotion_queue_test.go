package service

import (
	"context"
	"encoding/json"
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
